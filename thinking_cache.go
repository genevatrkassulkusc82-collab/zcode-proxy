package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"time"
)

// R6 签名思考块重放：GLM 的 Anthropic 兼容端点在 thinking 块上携带 signature，
// 多轮工具循环里下一轮请求必须原样重放该签名块。OpenAI 协议客户端只见
// reasoning_content 文本、重放时签名缺失，导致下一轮上游失败/降级。
// 这里按"助手可见输出（文本 + 工具调用）"缓存签名块，请求转换时静默回填。

// thinkingBlock 一个已签名思考块
type thinkingBlock struct {
	Text      string
	Signature string
}

// thinkingCacheEntry 缓存条目（写入时间用于 TTL 淘汰）
type thinkingCacheEntry struct {
	blocks []thinkingBlock
	stored time.Time
}

const (
	thinkingCacheMaxEntries = 1000
	thinkingCacheTTL        = time.Hour
)

// thinkingCache 进程内签名思考块缓存：FIFO 淘汰 + 插入时 TTL 清理
type thinkingCache struct {
	mu      sync.Mutex
	entries map[string]thinkingCacheEntry
	order   []string // FIFO 插入序
	max     int
	ttl     time.Duration
}

func newThinkingCache() *thinkingCache {
	return &thinkingCache{
		entries: map[string]thinkingCacheEntry{},
		max:     thinkingCacheMaxEntries,
		ttl:     thinkingCacheTTL,
	}
}

// thinkingReplay 缓存单例
var thinkingReplay = newThinkingCache()

// thinkingReplayKey 由助手可见输出构造确定性缓存键：sha256(规范化 JSON)。
// toolCalls 归一为 [{id,name,input}] 切片，input 经 JSON 往返归一化后
// 由 json.Marshal 按键序输出（Go 对 map 键排序），键序天然稳定。
func thinkingReplayKey(text string, toolCalls []map[string]interface{}) string {
	norm := make([]interface{}, 0, len(toolCalls))
	for _, c := range toolCalls {
		id, _ := c["id"].(string)
		name, _ := c["name"].(string)
		var input interface{}
		if raw, err := json.Marshal(c["input"]); err == nil {
			json.Unmarshal(raw, &input)
		}
		if input == nil {
			input = map[string]interface{}{}
		}
		norm = append(norm, map[string]interface{}{"id": id, "name": name, "input": input})
	}
	payload, err := json.Marshal([]interface{}{text, norm})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// store 写入缓存；空签名块不存，键空不存。重复写同键视为最新（刷新 FIFO 位次）。
func (c *thinkingCache) store(key string, blocks []thinkingBlock) {
	var signed []thinkingBlock
	for _, b := range blocks {
		if b.Signature != "" {
			signed = append(signed, b)
		}
	}
	if key == "" || len(signed) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	// 插入时顺带按 TTL 清理过期条目
	for k, e := range c.entries {
		if now.Sub(e.stored) > c.ttl {
			delete(c.entries, k)
		}
	}
	kept := c.order[:0]
	for _, k := range c.order {
		if _, ok := c.entries[k]; ok {
			kept = append(kept, k)
		}
	}
	c.order = kept
	for i, k := range c.order {
		if k == key {
			c.order = append(c.order[:i], c.order[i+1:]...)
			break
		}
	}
	c.entries[key] = thinkingCacheEntry{blocks: signed, stored: now}
	c.order = append(c.order, key)
	for len(c.entries) > c.max {
		oldest := c.order[0]
		c.order = c.order[1:]
		delete(c.entries, oldest)
	}
}

// lookup 查询缓存；未命中或已过期返回 nil
func (c *thinkingCache) lookup(key string) []thinkingBlock {
	if key == "" {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return nil
	}
	if time.Since(e.stored) > c.ttl {
		delete(c.entries, key)
		return nil
	}
	out := make([]thinkingBlock, len(e.blocks))
	copy(out, e.blocks)
	return out
}

// cacheThinkingForOutput 捕获点共用：以助手可见输出（拼接文本 + 工具调用）为键
// 存入本响应收集到的签名思考块。文本与工具调用皆空不存（无键可回放）。
func cacheThinkingForOutput(text string, usage *StreamUsage) {
	if usage == nil || len(usage.ThinkingBlocks) == 0 {
		return
	}
	if text == "" && len(usage.ToolCalls) == 0 {
		return
	}
	thinkingReplay.store(thinkingReplayKey(text, usage.ToolCalls), usage.ThinkingBlocks)
}

// assistantReplayKeyFromBlocks 从转换后的助手内容块（text + tool_use）构造缓存键，
// 与捕获侧的 thinkingReplayKey(拼接文本, 工具调用) 一致
func assistantReplayKeyFromBlocks(blocks []map[string]interface{}) string {
	var text strings.Builder
	var toolCalls []map[string]interface{}
	for _, b := range blocks {
		switch b["type"] {
		case "text":
			if t, ok := b["text"].(string); ok {
				text.WriteString(t)
			}
		case "tool_use":
			toolCalls = append(toolCalls, b)
		}
	}
	return thinkingReplayKey(text.String(), toolCalls)
}

// replayThinkingBlocks 查缓存并把签名思考块前缀回填进助手内容块；
// 未命中或已含 thinking 块时原样返回
func replayThinkingBlocks(blocks []map[string]interface{}) []map[string]interface{} {
	if len(blocks) == 0 {
		return blocks
	}
	for _, b := range blocks {
		if b["type"] == "thinking" {
			return blocks
		}
	}
	cached := thinkingReplay.lookup(assistantReplayKeyFromBlocks(blocks))
	if len(cached) == 0 {
		return blocks
	}
	injected := make([]map[string]interface{}, 0, len(cached)+len(blocks))
	for _, b := range cached {
		injected = append(injected, map[string]interface{}{
			"type": "thinking", "thinking": b.Text, "signature": b.Signature,
		})
	}
	return append(injected, blocks...)
}
