package main

import (
	"fmt"
	"testing"
	"time"
)

func TestThinkingCacheRoundtrip(t *testing.T) {
	c := newThinkingCache()
	tools := []map[string]interface{}{
		{"id": "call_1", "name": "get_weather", "input": map[string]interface{}{"city": "HK"}},
	}
	key := thinkingReplayKey("hello", tools)
	if key == "" {
		t.Fatal("key must not be empty")
	}
	c.store(key, []thinkingBlock{{Text: "think", Signature: "sig"}})
	got := c.lookup(key)
	if len(got) != 1 || got[0].Text != "think" || got[0].Signature != "sig" {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
	// 不同工具调用 → 不同键，必须 miss
	other := thinkingReplayKey("hello", []map[string]interface{}{
		{"id": "call_2", "name": "get_weather", "input": map[string]interface{}{"city": "HK"}},
	})
	if other == key {
		t.Fatal("different tool ids must produce different keys")
	}
	if got := c.lookup(other); got != nil {
		t.Fatalf("expected miss, got %+v", got)
	}
}

func TestThinkingCacheEmptySignatureNotStored(t *testing.T) {
	c := newThinkingCache()
	key := thinkingReplayKey("x", nil)
	c.store(key, []thinkingBlock{{Text: "unsigned"}})
	if got := c.lookup(key); got != nil {
		t.Fatalf("empty-signature block must not be stored, got %+v", got)
	}
	// 混合块：只保留有签名的
	c.store(key, []thinkingBlock{{Text: "unsigned"}, {Text: "signed", Signature: "s"}})
	got := c.lookup(key)
	if len(got) != 1 || got[0].Signature != "s" {
		t.Fatalf("only signed blocks should survive: %+v", got)
	}
	// 文本与工具调用皆空：不落缓存
	before := len(thinkingReplay.entries)
	cacheThinkingForOutput("", &StreamUsage{ThinkingBlocks: []thinkingBlock{{Text: "t", Signature: "s"}}})
	if len(thinkingReplay.entries) != before {
		t.Fatal("empty output must not be cached")
	}
}

func TestThinkingCacheEviction(t *testing.T) {
	c := newThinkingCache()
	c.max = 3
	for i := 0; i < 5; i++ {
		c.store(fmt.Sprintf("k%d", i), []thinkingBlock{{Text: "t", Signature: "s"}})
	}
	if len(c.entries) != 3 {
		t.Fatalf("cache size must be capped, got %d", len(c.entries))
	}
	for _, k := range []string{"k0", "k1"} {
		if _, ok := c.entries[k]; ok {
			t.Fatalf("oldest entry %s should be evicted", k)
		}
	}
	for _, k := range []string{"k2", "k3", "k4"} {
		if c.lookup(k) == nil {
			t.Fatalf("recent entry %s should hit", k)
		}
	}
}

func TestThinkingCacheTTL(t *testing.T) {
	c := newThinkingCache()
	c.store("k", []thinkingBlock{{Text: "t", Signature: "s"}})
	c.mu.Lock()
	e := c.entries["k"]
	e.stored = time.Now().Add(-2 * time.Hour)
	c.entries["k"] = e
	c.mu.Unlock()
	if got := c.lookup("k"); got != nil {
		t.Fatalf("expired entry must miss, got %+v", got)
	}
}

func TestThinkingReplayKeyStable(t *testing.T) {
	// 相同内容、不同 map 字面量插入序 → 同一键（json.Marshal 对 map 键排序）
	k1 := thinkingReplayKey("text", []map[string]interface{}{{
		"input": map[string]interface{}{"city": "HK", "zip": "000"},
		"name":  "get_weather",
		"id":    "call_1",
	}})
	k2 := thinkingReplayKey("text", []map[string]interface{}{{
		"id":    "call_1",
		"name":  "get_weather",
		"input": map[string]interface{}{"zip": "000", "city": "HK"},
	}})
	if k1 == "" || k1 != k2 {
		t.Fatalf("key unstable across map key order: %s vs %s", k1, k2)
	}
	// 工具输入不同 → 键不同
	k3 := thinkingReplayKey("text", []map[string]interface{}{{
		"id": "call_1", "name": "get_weather", "input": map[string]interface{}{"city": "TK"},
	}})
	if k3 == k1 {
		t.Fatal("different input must change the key")
	}
}

// TestOpenAIToAnthropicThinkingReplay 集成：预存签名块后，
// OpenAI assistant 回合（文本 + tool_call）转换时签名思考块被回填到 content 首位
func TestOpenAIToAnthropicThinkingReplay(t *testing.T) {
	text := "replay-integration-unique-text"
	input := map[string]interface{}{"city": "HK"}
	tools := []map[string]interface{}{{"id": "call_1", "name": "get_weather", "input": input}}
	key := thinkingReplayKey(text, tools)
	thinkingReplay.store(key, []thinkingBlock{{Text: "chain of thought", Signature: "sig123"}})

	body := map[string]interface{}{
		"model": "gpt-5",
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "hi"},
			map[string]interface{}{
				"role":    "assistant",
				"content": text,
				"tool_calls": []interface{}{
					map[string]interface{}{
						"id":   "call_1",
						"type": "function",
						"function": map[string]interface{}{
							"name":      "get_weather",
							"arguments": `{"city":"HK"}`,
						},
					},
				},
			},
		},
	}
	anth, err := openaiToAnthropic(body)
	if err != nil {
		t.Fatalf("openaiToAnthropic: %v", err)
	}
	msgs, _ := anth["messages"].([]interface{})
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
	asst, _ := msgs[1].(map[string]interface{})
	if asst["role"] != "assistant" {
		t.Fatalf("expected assistant message, got %+v", asst)
	}
	blocks, ok := asst["content"].([]map[string]interface{})
	if !ok || len(blocks) != 3 {
		t.Fatalf("expected 3 content blocks, got %+v", asst["content"])
	}
	first := blocks[0]
	if first["type"] != "thinking" || first["signature"] != "sig123" || first["thinking"] != "chain of thought" {
		t.Fatalf("thinking block not replayed first: %+v", first)
	}
	if blocks[1]["type"] != "text" || blocks[2]["type"] != "tool_use" {
		t.Fatalf("original blocks must follow: %+v", blocks[1:])
	}

	// 无缓存命中时不得注入/不改写
	body2 := map[string]interface{}{
		"model": "gpt-5",
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "plain"},
		},
	}
	anth2, err := openaiToAnthropic(body2)
	if err != nil {
		t.Fatalf("openaiToAnthropic: %v", err)
	}
	msgs2, _ := anth2["messages"].([]interface{})
	um, _ := msgs2[0].(map[string]interface{})
	ublocks, _ := um["content"].([]map[string]interface{})
	if len(ublocks) != 1 || ublocks[0]["type"] != "text" {
		t.Fatalf("non-tool-loop message must be untouched: %+v", ublocks)
	}
}
