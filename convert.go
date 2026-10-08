package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ---- 协议转换：Anthropic Messages ↔ OpenAI Chat/Responses ----
// 移植 zcode2api gateway.py 的 _openai_to_anthropic / _responses_to_anthropic /
// _openai_sse / _responses_sse / _collect_anthropic。

// StreamUsage 流式嗅探到的用量
type StreamUsage struct {
	InputTokens         int
	OutputTokens        int
	CacheReadTokens     int // 上游缓存命中 token（cache_read_input_tokens）
	CacheCreationTokens int // 上游缓存写入 token（cache_creation_input_tokens）
	StopReason          string
	ToolCalls           []map[string]interface{}
	StreamError         string                   // 上游 SSE error 事件（overloaded_error 等）
	ToolTruncated       bool                     // 工具参数截断：不可解析的非空 partial_json（评审 F7，不得洗成空参成功）
	ThinkingBlocks      []thinkingBlock          // R6：本响应收集到的已签名思考块（重放缓存）
	sigByBlock          map[int]string           // content_block index → signature（逐块捕获态）
	thinkBufs           map[int]*strings.Builder // content_block index → thinking 文本缓冲
}

// initThinkState 惰性初始化思考块逐块捕获状态
func (u *StreamUsage) initThinkState() {
	if u.sigByBlock == nil {
		u.sigByBlock = map[int]string{}
	}
	if u.thinkBufs == nil {
		u.thinkBufs = map[int]*strings.Builder{}
	}
}

// finishThinkBlock 思考块结束：签名非空时收进 ThinkingBlocks，清理逐块状态
func (u *StreamUsage) finishThinkBlock(idx int) {
	if u.sigByBlock == nil && u.thinkBufs == nil {
		return
	}
	sig := u.sigByBlock[idx]
	if buf := u.thinkBufs[idx]; buf != nil && sig != "" {
		u.ThinkingBlocks = append(u.ThinkingBlocks, thinkingBlock{Text: buf.String(), Signature: sig})
	}
	delete(u.sigByBlock, idx)
	delete(u.thinkBufs, idx)
}

// sseEvent 一个完整的 SSE 事件
type sseEvent struct {
	Event string
	Data  map[string]interface{}
}

// maxSSEBuffer 单个流解析缓冲上限（16MB）
const maxSSEBuffer = 16 << 20

// errSSEOverflow 解析缓冲超限（上游持续不发空行分帧），流不可恢复
var errSSEOverflow = errors.New("sse buffer overflow")

// sseParser 增量 SSE 帧解析器（处理跨 chunk 断帧，兼容 LF/CRLF）
type sseParser struct {
	buf     strings.Builder
	bomDone bool
}

func (p *sseParser) feed(chunk []byte, fn func(sseEvent)) error {
	if !p.bomDone {
		p.bomDone = true
		// 首帧 BOM 不剥离会让 "data:" 前缀匹配失败，丢掉携带 usage 的
		// message_start（该流用量全记 0）
		if len(chunk) >= 3 && chunk[0] == 0xEF && chunk[1] == 0xBB && chunk[2] == 0xBF {
			chunk = chunk[3:]
		}
	}
	p.buf.Write(chunk)
	p.drain(fn)
	// 缓冲上限：上游持续不发空行分隔时防止无界增长（OOM 面），按流失败处理
	if p.buf.Len() > maxSSEBuffer {
		p.buf.Reset()
		return errSSEOverflow
	}
	return nil
}

func (p *sseParser) drain(fn func(sseEvent)) {
	for {
		s := p.buf.String()
		idxLF := strings.Index(s, "\n\n")
		idxCRLF := strings.Index(s, "\r\n\r\n")
		idx, width := -1, 0
		switch {
		case idxLF >= 0 && (idxCRLF < 0 || idxLF <= idxCRLF):
			idx, width = idxLF, 2
		case idxCRLF >= 0:
			idx, width = idxCRLF, 4
		}
		if idx < 0 {
			return
		}
		block := s[:idx]
		p.buf.Reset()
		p.buf.WriteString(s[idx+width:])
		if ev, ok := parseSSEBlock(block); ok {
			fn(ev)
		}
	}
}

func (p *sseParser) flush(fn func(sseEvent)) {
	if ev, ok := parseSSEBlock(p.buf.String()); ok {
		fn(ev)
	}
	p.buf.Reset()
}

func parseSSEBlock(block string) (sseEvent, bool) {
	var ev sseEvent
	var dataLines []string
	for _, line := range strings.Split(block, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(line, "event:") {
			ev.Event = strings.TrimSpace(line[6:])
		} else if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(line[5:]))
		}
	}
	dataStr := strings.Join(dataLines, "\n")
	if dataStr == "" || dataStr == "[DONE]" {
		return ev, false
	}
	if err := json.Unmarshal([]byte(dataStr), &ev.Data); err != nil {
		return ev, false
	}
	// 上游常省略 event: 行直接以 data 发错误帧，归一化为 error 事件，避免被各消费方忽略
	if ev.Event == "" && isDataErrorFrame(ev.Data) {
		ev.Event = "error"
	}
	return ev, ev.Event != "" || ev.Data != nil
}

// isDataErrorFrame data-only 帧内嵌错误：顶层 error 字段或 type=="error"
func isDataErrorFrame(data map[string]interface{}) bool {
	if t, _ := data["type"].(string); t == "error" {
		return true
	}
	e, ok := data["error"]
	return ok && e != nil
}

// parseAnthropicUsageJSON 非流式 Anthropic 响应提取 usage
func parseAnthropicUsageJSON(body []byte) *StreamUsage {
	var v struct {
		Usage struct {
			InputTokens         int `json:"input_tokens"`
			OutputTokens        int `json:"output_tokens"`
			CacheReadTokens     int `json:"cache_read_input_tokens"`
			CacheCreationTokens int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
		StopReason string `json:"stop_reason"`
	}
	if json.Unmarshal(body, &v) != nil {
		return nil
	}
	return &StreamUsage{InputTokens: v.Usage.InputTokens, OutputTokens: v.Usage.OutputTokens,
		CacheReadTokens: v.Usage.CacheReadTokens, CacheCreationTokens: v.Usage.CacheCreationTokens,
		StopReason: v.StopReason}
}

// applyEventToUsage 从单个事件累积 usage / stop_reason / tool_calls
func applyEventToUsage(ev sseEvent, usage *StreamUsage, activeTool *map[string]interface{}, textParts, thinkParts *[]string) {
	switch ev.Event {
	case "message_start":
		if msg, ok := ev.Data["message"].(map[string]interface{}); ok {
			if u, ok := msg["usage"].(map[string]interface{}); ok {
				if n := toInt(u["input_tokens"]); n > usage.InputTokens {
					usage.InputTokens = n
				}
				if n := toInt(u["output_tokens"]); n > usage.OutputTokens {
					usage.OutputTokens = n
				}
				// 缓存 token 计量（R3）：与 input_tokens 同取 max 语义
				if n := toInt(u["cache_read_input_tokens"]); n > usage.CacheReadTokens {
					usage.CacheReadTokens = n
				}
				if n := toInt(u["cache_creation_input_tokens"]); n > usage.CacheCreationTokens {
					usage.CacheCreationTokens = n
				}
			}
		}
	case "message_delta":
		if u, ok := ev.Data["usage"].(map[string]interface{}); ok {
			if v, ok := u["output_tokens"]; ok {
				usage.OutputTokens = toInt(v)
			}
			// api.z.ai 通道把最终 input_tokens 放在 message_delta（message_start 为 0）
			if v, ok := u["input_tokens"]; ok {
				if n := toInt(v); n > usage.InputTokens {
					usage.InputTokens = n
				}
			}
			if v, ok := u["cache_read_input_tokens"]; ok {
				if n := toInt(v); n > usage.CacheReadTokens {
					usage.CacheReadTokens = n
				}
			}
			if v, ok := u["cache_creation_input_tokens"]; ok {
				if n := toInt(v); n > usage.CacheCreationTokens {
					usage.CacheCreationTokens = n
				}
			}
		}
		if d, ok := ev.Data["delta"].(map[string]interface{}); ok {
			if sr, ok := d["stop_reason"].(string); ok && sr != "" {
				usage.StopReason = sr
			}
		}
	case "content_block_start":
		if block, ok := ev.Data["content_block"].(map[string]interface{}); ok {
			if block["type"] == "thinking" {
				// R6：记录思考块签名并开文本缓冲（签名也可能在 start 自带）
				usage.initThinkState()
				idx := toInt(ev.Data["index"])
				if sig, ok := block["signature"].(string); ok && sig != "" {
					usage.sigByBlock[idx] = sig
				}
				usage.thinkBufs[idx] = &strings.Builder{}
			}
			if block["type"] == "tool_use" {
				tool := map[string]interface{}{
					"id":    block["id"],
					"name":  block["name"],
					"_json": "",
				}
				if input, ok := block["input"].(map[string]interface{}); ok {
					tool["input"] = input
				} else {
					tool["input"] = map[string]interface{}{}
				}
				*activeTool = tool
				usage.ToolCalls = append(usage.ToolCalls, tool)
			}
		}
	case "content_block_delta":
		delta, _ := ev.Data["delta"].(map[string]interface{})
		if delta == nil {
			return
		}
		switch delta["type"] {
		case "text_delta":
			if t, ok := delta["text"].(string); ok {
				*textParts = append(*textParts, t)
			}
		case "signature_delta":
			// R6：签名增量追加到对应思考块
			if s, ok := delta["signature"].(string); ok && s != "" {
				usage.initThinkState()
				usage.sigByBlock[toInt(ev.Data["index"])] += s
			}
		case "thinking_delta":
			if t, ok := delta["thinking"].(string); ok {
				*thinkParts = append(*thinkParts, t)
				// R6：同步进逐块缓冲，供签名重放缓存取完整块文本
				if buf, ok := usage.thinkBufs[toInt(ev.Data["index"])]; ok {
					buf.WriteString(t)
				}
			}
		case "input_json_delta":
			if *activeTool != nil {
				if pj, ok := delta["partial_json"].(string); ok {
					(*activeTool)["_json"] = ((*activeTool)["_json"]).(string) + pj
				}
			}
		}
	case "content_block_stop":
		usage.finishThinkBlock(toInt(ev.Data["index"]))
		*activeTool = nil
	case "error":
		// 上游流内错误事件（overloaded_error / 风控中途拦截等），不得被吞掉
		msg := ""
		if e, ok := ev.Data["error"].(map[string]interface{}); ok {
			msg, _ = e["message"].(string)
			if msg == "" {
				msg, _ = e["type"].(string)
			}
		}
		if msg == "" {
			msg = "upstream stream error"
		}
		usage.StreamError = msg
	}
}

// finalizeToolCalls 把累积的 partial_json 解析为 input
func finalizeToolCalls(usage *StreamUsage) {
	for _, call := range usage.ToolCalls {
		raw, _ := call["_json"].(string)
		delete(call, "_json")
		if raw != "" {
			var parsed map[string]interface{}
			if json.Unmarshal([]byte(raw), &parsed) == nil {
				if parsed == nil {
					// 字面量 "null"：Unmarshal 成功但得 nil，客户端工具执行器
					// 期待对象（与非流式路径的 input 缺省修复同因）
					parsed = map[string]interface{}{}
				}
				call["input"] = parsed
			} else {
				// 不可解析的非空缓冲 = 上游在参数中途断流（评审 F7）：置位交由
				// 各路径按流失败处理。input 仍补 {} 保持结构合法，但响应绝不能
				// 以成功状态出门——客户端会拿空参去执行工具
				usage.ToolTruncated = true
			}
		}
		if _, ok := call["input"]; !ok {
			call["input"] = map[string]interface{}{}
		}
	}
}

func toInt(v interface{}) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	}
	return 0
}

// ---- 非流式响应写回 ----

// writeProtocolResponse 按客户端协议写回非流式响应
func writeProtocolResponse(w http.ResponseWriter, rc *relayCtx, status int, contentType string,
	body []byte, usage *StreamUsage) {

	proto := rc.proto
	if proto == protocolAnthropic {
		w.Header().Set("Content-Type", firstNonEmpty(contentType, "application/json"))
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(status)
		w.Write(body)
		return
	}
	// OpenAI / Responses：从 Anthropic JSON 提取文本/思考/工具调用
	var resp struct {
		Content []struct {
			Type      string          `json:"type"`
			Text      string          `json:"text"`
			Thinking  string          `json:"thinking"`
			Signature string          `json:"signature"`
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			Input     json.RawMessage `json:"input"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	json.Unmarshal(body, &resp)
	var text, thinking string
	var toolCalls []map[string]interface{}
	var thinkBlocks []thinkingBlock
	for _, c := range resp.Content {
		switch c.Type {
		case "text":
			text += c.Text
		case "thinking":
			thinking += c.Thinking
			// R6：非流式路径同样收集签名思考块供重放缓存
			if c.Signature != "" {
				thinkBlocks = append(thinkBlocks, thinkingBlock{Text: c.Thinking, Signature: c.Signature})
			}
		case "tool_use":
			var input map[string]interface{}
			json.Unmarshal(c.Input, &input)
			if input == nil {
				// input 缺失/字面量 null：客户端工具执行器期待对象，"null" 参数会砸坏它
				input = map[string]interface{}{}
			}
			toolCalls = append(toolCalls, map[string]interface{}{
				"id": c.ID, "name": c.Name, "input": input,
			})
		}
	}
	u := &StreamUsage{
		InputTokens:  resp.Usage.InputTokens,
		OutputTokens: resp.Usage.OutputTokens,
		StopReason:   resp.StopReason,
		ToolCalls:    toolCalls,
	}
	// 嗅探 usage 缺计数时从解析结果补齐；不能整体换掉 u——换掉会丢掉
	// 本地解析出的 ToolCalls（嗅探侧常为 nil），OpenAI 客户端就会收到
	// 没有 tool_calls 的助手回合
	if usage != nil {
		if u.InputTokens == 0 {
			u.InputTokens = usage.InputTokens
		}
		if u.OutputTokens == 0 {
			u.OutputTokens = usage.OutputTokens
		}
		if u.StopReason == "" {
			u.StopReason = usage.StopReason
		}
		if len(u.ToolCalls) == 0 {
			u.ToolCalls = usage.ToolCalls
		}
		if u.CacheReadTokens == 0 {
			u.CacheReadTokens = usage.CacheReadTokens
		}
		if u.CacheCreationTokens == 0 {
			u.CacheCreationTokens = usage.CacheCreationTokens
		}
	}
	u.ThinkingBlocks = append(u.ThinkingBlocks, thinkBlocks...)
	cacheThinkingForOutput(text, u)
	if proto == protocolOpenAI {
		writeJSON(w, status, openaiResponse(rc.clientModel, text, thinking, u))
	} else if proto == protocolResponses {
		writeJSON(w, status, responsesResponse(rc.clientModel, newResponseID(), text, thinking, u))
	} else {
		writeJSON(w, status, completionsResponse(rc.clientModel, text, u, rc.echo, rc.prompt))
	}
}

// ---- 流式响应写回 ----

// clientGone 中途读失败是否源于客户端主动断开：上游请求绑定 r.Context()（relay），
// 客户端先行关闭时其 Err() 为 context.Canceled。此类中断按 499（nginx 语义的
// "client closed request"，net/http 无该常量，用字面量）落库而非 502，避免客户端
// 主动放弃污染上游错误率面板
func clientGone(r *http.Request) bool {
	return r != nil && errors.Is(r.Context().Err(), context.Canceled)
}

// ssePingInterval 上游静默多久后向客户端注入一个协议合法的 ping 帧。
// zai-org/ZCode 按 chunk 间隔计流空闲超时（超限 abort + 全量重试且每次重试加罚
// 30s）；长思考/长工具调用间隙注入 ping 可避免误判。var 以便测试缩短间隔。
var ssePingInterval = 15 * time.Second

// upstreamSilenceBound 上游多少时间没有任何字节即按流失败终止（轮 10 红队 F2）。
// 保活帧会不停重置客户端的空闲超时，若无此上限，半死上游会让客户端与账号槽
// 永远挂着。10 分钟 > 客户端 600s 空闲超时，远大于合法思考间隙。var 便于测试缩短。
var upstreamSilenceBound = 10 * time.Minute

// ssePumpChunk 上游 SSE 泵送的单次读取结果
type ssePumpChunk struct {
	data []byte
	err  error
}

// pumpSSEWithKeepalive 阻塞读上游 SSE 并把字节交给 onChunk；上游静默超过
// interval 时调用 onKeepalive 注入保活帧（interval<=0 不注入，仅 200 透传体调用）。
// 读取在生产 goroutine 中进行 + buf 归还通道（Read 覆写消费端仍在解析数据的
// 竞态见 streamProtocolResponse 的 -race 实证）；onChunk 返回 false 终止泵送。
// 返回读取错误；io.EOF 折算为 nil（干净结束）。
func pumpSSEWithKeepalive(body io.ReadCloser, interval time.Duration,
	onChunk func([]byte) bool, onKeepalive func()) error {

	chunks := make(chan ssePumpChunk)
	bufs := make(chan []byte, 1)
	bufs <- make([]byte, 32*1024)
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			var b []byte
			select {
			case b = <-bufs:
			case <-done:
				return
			}
			n, err := body.Read(b)
			select {
			case chunks <- ssePumpChunk{data: b[:n], err: err}:
			case <-done:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	var pingC <-chan time.Time
	if interval > 0 {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		pingC = ticker.C
	}
	// 上游静默上限（轮 10 红队 F2）：保活帧会不停重置客户端的空闲超时，
	// 半死的上游（NAT 超时/中间盒静默丢流）原本会无限占住客户端与账号槽——
	// 心跳只该桥接合法的分钟级思考间隙，不允许把"客户端 600s 会放弃"变成"永远挂着"。
	// var 便于测试缩短。
	var silenceC <-chan time.Time
	silenceTimer := time.NewTimer(upstreamSilenceBound)
	defer silenceTimer.Stop()
	silenceC = silenceTimer.C
	for {
		select {
		case c := <-chunks:
			silenceTimer.Reset(upstreamSilenceBound)
			if len(c.data) > 0 {
				if !onChunk(c.data) {
					return nil
				}
			}
			if c.err != nil {
				if errors.Is(c.err, io.EOF) {
					return nil
				}
				return c.err
			}
			bufs <- c.data // 处理完毕归还；生产端复用后才发起下一次 Read
		case <-pingC:
			onKeepalive()
		case <-silenceC:
			return fmt.Errorf("upstream stream stalled: no upstream data for %v", upstreamSilenceBound)
		}
	}
}

// streamProtocolResponse 流式透传/转换 + usage 嗅探 + 用量落库
func streamProtocolResponse(w http.ResponseWriter, rc *relayCtx, resp *http.Response,
	a *Account, r *http.Request, payload []byte, z *ZCodeAPI, start time.Time) {

	proto := rc.proto
	clientStream := rc.clientStream
	clientModel := rc.clientModel
	includeUsage := rc.includeUsage
	defer resp.Body.Close()
	flusher, _ := w.(http.Flusher)

	switch {
	case proto == protocolAnthropic && clientStream:
		// 原生透传 + 嗅探
		w.Header().Set("Content-Type", firstNonEmpty(resp.Header.Get("Content-Type"), "text/event-stream"))
		w.Header().Set("Cache-Control", "no-cache")
		for _, k := range []string{"x-request-id", "request-id"} {
			if v := resp.Header.Get(k); v != "" {
				w.Header().Set(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		var usage StreamUsage
		var activeTool map[string]interface{}
		var texts, thinks []string
		parser := &sseParser{}
		ttft := 0
		buf := make([]byte, 32*1024)
		var readErr error
		// 上游静默时注入 ping 保活。仅对 200 透传体注入：错误体里混入 ping 会破坏协议。
		// 上游 Read 是阻塞的，读到单独 goroutine，主循环 select 兼顾 ping 定时器；
		// readDone 保证提前退出（解析溢出/错误）时生产 goroutine 不永久阻塞在 channel 上
		pingInterval := time.Duration(0)
		if resp.StatusCode == http.StatusOK {
			pingInterval = ssePingInterval
		}
		type readResult struct {
			buf []byte
			n   int
			err error
		}
		chunks := make(chan readResult)
		// buf 归还通道：生产端取回处理完的 buf 才发起下一次 Read，否则上游
		// Read 会覆写消费端仍在解析的上一块数据（-race 实证）
		bufs := make(chan []byte, 1)
		bufs <- buf
		readDone := make(chan struct{})
		// defer 而非循环后裸 close：中途 panic 时生产 goroutine 才不至于
		// 永久阻塞在发送/等待上（评审轮 3 红队 F7）
		defer close(readDone)
		go func() {
			for {
				var b []byte
				select {
				case b = <-bufs:
				case <-readDone:
					return
				}
				n, err := resp.Body.Read(b)
				select {
				case chunks <- readResult{buf: b, n: n, err: err}:
				case <-readDone:
					return
				}
				if err != nil {
					return
				}
			}
		}()
		var pingC <-chan time.Time
		if pingInterval > 0 {
			ticker := time.NewTicker(pingInterval)
			defer ticker.Stop()
			pingC = ticker.C
		}
		// 上游静默上限（轮 10 红队 F2，与 pumpSSEWithKeepalive 同规）：
		// ping 帧会不停重置客户端空闲超时，无上限时半死上游永远挂着
		silenceTimer := time.NewTimer(upstreamSilenceBound)
		defer silenceTimer.Stop()
	readLoop:
		for {
			select {
			case c := <-chunks:
				if c.n > 0 {
					if ttft == 0 {
						ttft = int(time.Since(start).Milliseconds())
					}
					w.Write(c.buf[:c.n])
					if flusher != nil {
						flusher.Flush()
					}
					if ferr := parser.feed(c.buf[:c.n], func(ev sseEvent) {
						applyEventToUsage(ev, &usage, &activeTool, &texts, &thinks)
					}); ferr != nil {
						readErr = ferr
						break readLoop
					}
				}
				if c.err != nil {
					readErr = c.err
					break readLoop
				}
				bufs <- c.buf // 处理完毕归还；生产端复用后才发起下一次 Read
				silenceTimer.Reset(upstreamSilenceBound)
			case <-silenceTimer.C:
				readErr = fmt.Errorf("upstream stream stalled: no upstream data for %v", upstreamSilenceBound)
				break readLoop
			case <-pingC:
				w.Write([]byte("event: ping\ndata: {\"type\":\"ping\"}\n\n"))
				if flusher != nil {
					flusher.Flush()
				}
			}
		}
		parser.flush(func(ev sseEvent) { applyEventToUsage(ev, &usage, &activeTool, &texts, &thinks) })
		finalizeToolCalls(&usage)
		cacheThinkingForOutput(strings.Join(texts, ""), &usage)
		if readErr != nil && readErr != io.EOF {
			// 中途断流/解析溢出：客户端流会被截断，补一个协议正确的 error 帧，
			// 让 SDK 能区分"干净结束"与"上游中断"
			writeSSEErrorEvent(w, fmt.Sprintf("upstream stream interrupted: %v", readErr))
		}
		if usage.ToolTruncated {
			// 参数中途断流（评审 F7）：透传体已含半截 tool_use，补错误帧让 SDK
			// 不把半截流当干净结束、拿空参去执行工具
			writeSSEErrorEvent(w, "upstream tool arguments truncated")
		}
		// 透传路径错误事件已原样转发给客户端；此处仅修正用量记录语义并告警
		recStatus := resp.StatusCode
		if usage.ToolTruncated {
			recStatus = 502
			log.Printf("[relay] upstream tool arguments truncated (passthrough)")
		} else if usage.StreamError != "" {
			recStatus = 502
			log.Printf("[relay] upstream stream error event (passthrough): %s", usage.StreamError)
		} else if readErr != nil && readErr != io.EOF {
			if clientGone(r) {
				// 客户端主动断开：499 落库，不计入上游错误
				recStatus = 499
				log.Printf("[relay] client closed request mid-stream (passthrough)")
			} else {
				recStatus = 502
				log.Printf("[relay] upstream stream interrupted (passthrough): %v", readErr)
			}
		}
		z.recordUsage(a, r, payload, recStatus, start, ttft, &usage, rc.clientStream)

	case proto == protocolOpenAI && clientStream:
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(resp.StatusCode)
		z.streamOpenAI(w, flusher, resp, clientModel, includeUsage, a, r, payload, start)

	case proto == protocolResponses && clientStream:
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(resp.StatusCode)
		z.streamResponses(w, flusher, resp, clientModel, a, r, payload, start)

	case proto == protocolCompletions && clientStream:
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(resp.StatusCode)
		z.streamCompletions(w, flusher, resp, clientModel, includeUsage, rc.echo, rc.prompt, a, r, payload, start)

	default:
		// 客户端要非流式，但上游是流式：聚合后写单个 JSON。
		// 聚合读取同样受总时限约束（轮 10 红队 F1：客户端对非流式无超时，
		// 慢滴/挂死上游原本无限占住账号并发槽——轮 5 只包了上游 JSON 分支）
		resp.Body = newTotalDeadlineBody(resp.Body, nonStreamBodyReadDeadline)
		if proto == protocolAnthropic {
			// Anthropic 客户端：聚合回完整 message（与闲时通道同一聚合器）。
			// 64MB 上限与 relay 非流式路径同规：多读 1 字节判定超限防静默截断
			aggregated, aggUsage, aerr := aggregateAnthropicStream(io.LimitReader(resp.Body, (64<<20)+1))
			if aerr == nil && len(aggregated) > 64<<20 {
				aerr = fmt.Errorf("上游响应超过 64MB 上限")
			}
			if aerr != nil {
				z.recordUsage(a, r, payload, 502, start, 0, aggUsage, false)
				// Anthropic 原生客户端：错误体须带顶层 type:"error" 信封（SDK
				// schema 要求，否则只显示 statusText），与 writeAPIError 同规
				writeJSON(w, http.StatusBadGateway, map[string]interface{}{
					"type":  "error",
					"error": map[string]string{"message": "上游响应聚合失败: " + truncate(aerr.Error(), 200), "type": "upstream_error"},
				})
				return
			}
			z.recordUsage(a, r, payload, resp.StatusCode, start, 0, aggUsage, false)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(resp.StatusCode)
			w.Write(aggregated)
			return
		}
		all, readErr := io.ReadAll(io.LimitReader(resp.Body, (64<<20)+1))
		var usage StreamUsage
		var activeTool map[string]interface{}
		var texts, thinks []string
		sawStart := false
		parser := &sseParser{}
		sawMessageDelta := false // message_delta 到达过：干净完整流的标志（缺失 = 中途截断）
		feedFn := func(ev sseEvent) {
			if ev.Event == "message_start" {
				sawStart = true
			}
			if ev.Event == "message_delta" {
				sawMessageDelta = true
			}
			applyEventToUsage(ev, &usage, &activeTool, &texts, &thinks)
		}
		if ferr := parser.feed(all, feedFn); ferr != nil {
			readErr = ferr
		}
		parser.flush(feedFn)
		finalizeToolCalls(&usage)
		cacheThinkingForOutput(strings.Join(texts, ""), &usage)
		// 上游流内错误、中途断流或工具参数截断：不得伪装成成功空响应
		if usage.StreamError != "" || usage.ToolTruncated || (readErr != nil && readErr != io.EOF) {
			msg := usage.StreamError
			if msg == "" && usage.ToolTruncated {
				msg = "upstream tool arguments truncated"
			}
			if msg == "" {
				msg = fmt.Sprintf("upstream stream interrupted: %v", readErr)
			}
			z.recordUsage(a, r, payload, 502, start, 0, &usage, rc.clientStream)
			writeJSON(w, http.StatusBadGateway, map[string]interface{}{
				"error": map[string]string{"message": msg, "type": "upstream_error"},
			})
			return
		}
		// 零事件干净 EOF（裸 keepalive 注释后直接关闭）：与 Anthropic 聚合
		// 路径同规，不得合成 200 空助手回合
		if !sawStart && len(texts) == 0 && len(usage.ToolCalls) == 0 {
			z.recordUsage(a, r, payload, 502, start, 0, &usage, rc.clientStream)
			writeJSON(w, http.StatusBadGateway, map[string]interface{}{
				"error": map[string]string{"message": "upstream stream ended without any events", "type": "upstream_error"},
			})
			return
		}
		// 内容已出但没等到 message_delta（干净截断）：合成完整 message 会把
		// 半截回答/截断 tool_use 伪装成成功——与聚合路径同规按失败处理
		// （流式路径保留对缺 message_delta 的容忍，见 openaiFinish 契约）
		if !sawMessageDelta && usage.StopReason == "" {
			z.recordUsage(a, r, payload, 502, start, 0, &usage, rc.clientStream)
			writeJSON(w, http.StatusBadGateway, map[string]interface{}{
				"error": map[string]string{"message": "upstream stream truncated before message_delta", "type": "upstream_error"},
			})
			return
		}
		text := strings.Join(texts, "")
		thinking := strings.Join(thinks, "")
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(resp.StatusCode)
		if proto == protocolOpenAI {
			json.NewEncoder(w).Encode(openaiResponse(clientModel, text, thinking, &usage))
		} else if proto == protocolResponses {
			json.NewEncoder(w).Encode(responsesResponse(clientModel, newResponseID(), text, thinking, &usage))
		} else {
			json.NewEncoder(w).Encode(completionsResponse(clientModel, text, &usage, rc.echo, rc.prompt))
		}
		z.recordUsage(a, r, payload, resp.StatusCode, start, 0, &usage, rc.clientStream)
	}
}

// ---- Anthropic SSE → OpenAI chat.completion.chunk ----

func (z *ZCodeAPI) streamOpenAI(w http.ResponseWriter, flusher http.Flusher, resp *http.Response,
	model string, includeUsage bool, a *Account, r *http.Request, payload []byte, start time.Time) {

	now := time.Now().Unix()
	cid := "chatcmpl-" + randomHex(12)
	var usage StreamUsage
	var activeTool map[string]interface{}
	var texts, thinks []string
	first := true
	sawStart := false // 见过 message_start（与聚合路径 sawStart 同义，零事件判定用）
	toolIndices := map[int]int{}
	toolArgsSeen := map[int]bool{}    // 该工具块是否已收到过 input_json_delta
	toolStartArgs := map[int]string{} // content_block_start 自带的完整 input（无 delta 时补发用）
	nextToolIndex := 0
	ttft := 0

	writeChunk := func(delta map[string]interface{}, finish interface{}, chunkUsage interface{}) {
		payload := map[string]interface{}{
			"id": cid, "object": "chat.completion.chunk", "created": now, "model": model,
			"choices": []map[string]interface{}{{"index": 0, "delta": delta, "finish_reason": finish}},
		}
		if includeUsage {
			payload["usage"] = chunkUsage
		}
		b, _ := json.Marshal(payload)
		fmt.Fprintf(w, "data: %s\n\n", b)
		if flusher != nil {
			flusher.Flush()
		}
	}

	parser := &sseParser{}
	handle := func(ev sseEvent) {
		if ttft == 0 {
			ttft = int(time.Since(start).Milliseconds())
		}
		if ev.Event == "message_start" {
			sawStart = true
		}
		applyEventToUsage(ev, &usage, &activeTool, &texts, &thinks)
		switch ev.Event {
		case "content_block_start":
			block, _ := ev.Data["content_block"].(map[string]interface{})
			if block != nil && block["type"] == "tool_use" {
				srcIdx := toInt(ev.Data["index"])
				toolIdx := nextToolIndex
				nextToolIndex++
				toolIndices[srcIdx] = toolIdx
				// 上游偶尔把完整参数放在 start 而不发任何 delta：先留存，
				// stop 时补发，避免客户端累计的 arguments 停留在 ""
				if raw, merr := json.Marshal(block["input"]); merr == nil && len(raw) > 0 && string(raw) != "null" {
					toolStartArgs[srcIdx] = string(raw)
				}
				if first {
					first = false
					writeChunk(map[string]interface{}{"role": "assistant", "content": ""}, nil, nil)
				}
				id, _ := block["id"].(string)
				if id == "" {
					id = "call_" + randomHex(8)
				}
				name, _ := block["name"].(string)
				writeChunk(map[string]interface{}{
					"tool_calls": []map[string]interface{}{{
						"index": toolIdx, "id": id, "type": "function",
						"function": map[string]interface{}{"name": name, "arguments": ""},
					}},
				}, nil, nil)
			}
		case "content_block_delta":
			delta, _ := ev.Data["delta"].(map[string]interface{})
			if delta == nil {
				return
			}
			switch delta["type"] {
			case "text_delta":
				if first {
					first = false
					writeChunk(map[string]interface{}{"role": "assistant", "content": ""}, nil, nil)
				}
				t, _ := delta["text"].(string)
				writeChunk(map[string]interface{}{"content": t}, nil, nil)
			case "thinking_delta":
				if first {
					first = false
					writeChunk(map[string]interface{}{"role": "assistant", "content": ""}, nil, nil)
				}
				t, _ := delta["thinking"].(string)
				writeChunk(map[string]interface{}{"reasoning_content": t}, nil, nil)
			case "input_json_delta":
				srcIdx := toInt(ev.Data["index"])
				// 未开过工具块的 index 收到参数增量（上游协议违例）：忽略本事件，
				// 否则按零值落 index 0、污染第一个工具的累计参数
				toolIdx, isTool := toolIndices[srcIdx]
				if !isTool {
					return
				}
				toolArgsSeen[srcIdx] = true
				pj, _ := delta["partial_json"].(string)
				writeChunk(map[string]interface{}{
					"tool_calls": []map[string]interface{}{{
						"index":    toolIdx,
						"function": map[string]interface{}{"arguments": pj},
					}},
				}, nil, nil)
			}
		case "content_block_stop":
			// 无任何 delta 的工具块：补发 start 携带的完整 input（无则 "{}"）终结参数，
			// 否则客户端累计的 arguments 停留在 ""，json.loads 会炸
			srcIdx := toInt(ev.Data["index"])
			if toolIdx, ok := toolIndices[srcIdx]; ok && !toolArgsSeen[srcIdx] {
				toolArgsSeen[srcIdx] = true
				args := toolStartArgs[srcIdx]
				if args == "" {
					args = "{}"
				}
				writeChunk(map[string]interface{}{
					"tool_calls": []map[string]interface{}{{
						"index":    toolIdx,
						"function": map[string]interface{}{"arguments": args},
					}},
				}, nil, nil)
			}
		}
	}

	// 上游静默期注入空 choices 心跳 chunk（schema 合法且被 openai-compatible
	// 解析端忽略），防客户端流空闲超时 abort+重试（与 Anthropic ping 同动机）
	keepalive := time.Duration(0)
	if resp.StatusCode == http.StatusOK {
		keepalive = ssePingInterval
	}
	var readErr error
	readErr = pumpSSEWithKeepalive(resp.Body, keepalive,
		func(b []byte) bool {
			if ferr := parser.feed(b, handle); ferr != nil {
				readErr = ferr
				return false
			}
			return true
		},
		func() {
			hb, _ := json.Marshal(map[string]interface{}{
				"id": cid, "object": "chat.completion.chunk", "created": now, "model": model,
				"choices": []interface{}{},
			})
			fmt.Fprintf(w, "data: %s\n\n", hb)
			if flusher != nil {
				flusher.Flush()
			}
		})
	parser.flush(handle)
	finalizeToolCalls(&usage)
	cacheThinkingForOutput(strings.Join(texts, ""), &usage)

	// 上游流内错误、中途断流或零事件干净 EOF：发 OpenAI 错误 chunk 而非伪装成功
	//（零事件判定与聚合路径同规：不得合成 200 空助手回合）
	interrupted := readErr != nil
	zeroEvents := !sawStart && len(texts) == 0 && len(usage.ToolCalls) == 0
	if usage.StreamError != "" || interrupted || zeroEvents || usage.ToolTruncated {
		msg := usage.StreamError
		if msg == "" && interrupted {
			msg = fmt.Sprintf("upstream stream interrupted: %v", readErr)
		}
		if msg == "" && usage.ToolTruncated {
			// 参数中途断流（评审 F7）：不得以 tool_calls + 空参数收尾洗成成功
			msg = "upstream tool arguments truncated"
		}
		if msg == "" {
			msg = "upstream stream ended without any events"
		}
		ep, _ := json.Marshal(map[string]interface{}{
			"error": map[string]interface{}{"message": msg, "type": "api_error", "code": "stream_error"},
		})
		fmt.Fprintf(w, "data: %s\n\n", ep)
		fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		recStatus := 502
		if interrupted && usage.StreamError == "" && clientGone(r) {
			recStatus = 499
		}
		z.recordUsage(a, r, payload, recStatus, start, ttft, &usage, true)
		return
	}

	if first {
		writeChunk(map[string]interface{}{"role": "assistant", "content": ""}, nil, nil)
	}
	// 干净 EOF 但缺 content_block_stop（上游违例）：补发 start 自带的完整参数，
	// 否则客户端拿到 arguments:"" 且 finish_reason:tool_calls，json.loads 直接炸
	pending := make([]int, 0, len(toolIndices))
	for srcIdx := range toolIndices {
		if !toolArgsSeen[srcIdx] {
			pending = append(pending, srcIdx)
		}
	}
	sort.Ints(pending)
	for _, srcIdx := range pending {
		toolArgsSeen[srcIdx] = true
		args := toolStartArgs[srcIdx]
		if args == "" {
			args = "{}"
		}
		writeChunk(map[string]interface{}{
			"tool_calls": []map[string]interface{}{{
				"index":    toolIndices[srcIdx],
				"function": map[string]interface{}{"arguments": args},
			}},
		}, nil, nil)
	}
	writeChunk(map[string]interface{}{}, openaiFinish(usage.StopReason, len(usage.ToolCalls) > 0), nil)
	if includeUsage {
		finalUsage := map[string]interface{}{
			"prompt_tokens":     usage.InputTokens,
			"completion_tokens": usage.OutputTokens,
			"total_tokens":      usage.InputTokens + usage.OutputTokens,
		}
		// 缓存命中明细（round-4 闭集：openai-compatible 客户端读取该键）
		if usage.CacheReadTokens > 0 {
			finalUsage["prompt_tokens_details"] = map[string]interface{}{"cached_tokens": usage.CacheReadTokens}
		}
		p, _ := json.Marshal(map[string]interface{}{
			"id": cid, "object": "chat.completion.chunk", "created": now, "model": model,
			"choices": []interface{}{}, "usage": finalUsage,
		})
		fmt.Fprintf(w, "data: %s\n\n", p)
		if flusher != nil {
			flusher.Flush()
		}
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
	z.recordUsage(a, r, payload, resp.StatusCode, start, ttft, &usage, true)
}

// ---- Anthropic SSE → OpenAI Responses SSE ----

func (z *ZCodeAPI) streamResponses(w http.ResponseWriter, flusher http.Flusher, resp *http.Response,
	model string, a *Account, r *http.Request, payload []byte, start time.Time) {

	responseID := newResponseID()
	createdAt := time.Now().Unix()
	var usage StreamUsage
	var activeTool map[string]interface{}
	var texts, thinks []string
	sawStart := false // 见过 message_start（与聚合路径 sawStart 同义，零事件判定用）
	sequence := 0
	nextOutputIndex := 0
	ttft := 0

	type blockState struct {
		kind        string // text | thinking | tool
		itemID      string
		callID      string
		name        string
		jsonBuf     string
		startArgs   string // content_block_start 自带的完整 input（无 delta 时用作终结参数）
		outputIndex int
		texts       []string
	}
	blocks := map[int]*blockState{}
	var outputItems []map[string]interface{} // 已流式发出的最终输出项，completed 复用其 ID

	writeEvent := func(name string, evPayload map[string]interface{}) {
		evPayload["sequence_number"] = sequence
		sequence++
		if _, ok := evPayload["type"]; !ok {
			evPayload["type"] = name
		}
		b, _ := json.Marshal(evPayload)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, b)
		if flusher != nil {
			flusher.Flush()
		}
	}

	openMessageEvents := func(idx int) {
		blk := blocks[idx]
		blk.itemID = "msg_" + randomHex(8)
		writeEvent("response.output_item.added", map[string]interface{}{
			"output_index": blk.outputIndex,
			"item": map[string]interface{}{
				"id": blk.itemID, "type": "message", "status": "in_progress",
				"role": "assistant", "content": []interface{}{},
			},
		})
		writeEvent("response.content_part.added", map[string]interface{}{
			"item_id": blk.itemID, "output_index": blk.outputIndex, "content_index": 0,
			"part": map[string]interface{}{"type": "output_text", "text": "", "annotations": []interface{}{}},
		})
	}

	closeMessageEvents := func(idx int) {
		blk, ok := blocks[idx]
		if !ok || blk.itemID == "" || blk.kind != "text" {
			return
		}
		text := strings.Join(blk.texts, "")
		texts = append(texts, blk.texts...)
		part := map[string]interface{}{"type": "output_text", "text": text, "annotations": []interface{}{}}
		writeEvent("response.output_text.done", map[string]interface{}{
			"item_id": blk.itemID, "output_index": blk.outputIndex, "content_index": 0, "text": text,
		})
		writeEvent("response.content_part.done", map[string]interface{}{
			"item_id": blk.itemID, "output_index": blk.outputIndex, "content_index": 0, "part": part,
		})
		item := map[string]interface{}{
			"id": blk.itemID, "type": "message", "status": "completed",
			"role": "assistant", "content": []interface{}{part},
		}
		writeEvent("response.output_item.done", map[string]interface{}{
			"output_index": blk.outputIndex,
			"item":         item,
		})
		outputItems = append(outputItems, item)
		delete(blocks, idx)
	}

	openReasoningEvents := func(idx int) {
		blk := blocks[idx]
		blk.itemID = "rs_" + randomHex(8)
		writeEvent("response.output_item.added", map[string]interface{}{
			"output_index": blk.outputIndex,
			"item": map[string]interface{}{
				"id": blk.itemID, "type": "reasoning", "summary": []interface{}{},
			},
		})
		writeEvent("response.reasoning_summary_part.added", map[string]interface{}{
			"item_id": blk.itemID, "output_index": blk.outputIndex, "summary_index": 0,
			"part": map[string]interface{}{"type": "summary_text", "text": ""},
		})
	}

	closeReasoningEvents := func(idx int) {
		blk, ok := blocks[idx]
		if !ok || blk.itemID == "" || blk.kind != "thinking" {
			return
		}
		text := strings.Join(blk.texts, "")
		part := map[string]interface{}{"type": "summary_text", "text": text}
		writeEvent("response.reasoning_summary_text.done", map[string]interface{}{
			"item_id": blk.itemID, "output_index": blk.outputIndex, "summary_index": 0, "text": text,
		})
		writeEvent("response.reasoning_summary_part.done", map[string]interface{}{
			"item_id": blk.itemID, "output_index": blk.outputIndex, "summary_index": 0, "part": part,
		})
		item := map[string]interface{}{
			"id": blk.itemID, "type": "reasoning",
			"summary": []map[string]interface{}{part},
		}
		writeEvent("response.output_item.done", map[string]interface{}{
			"output_index": blk.outputIndex,
			"item":         item,
		})
		outputItems = append(outputItems, item)
		delete(blocks, idx)
	}

	openToolEvents := func(idx int, block map[string]interface{}) {
		blk := blocks[idx]
		callID, _ := block["id"].(string)
		if callID == "" {
			callID = "call_" + randomHex(8)
		}
		blk.itemID = "fc_" + randomHex(8)
		blk.callID = callID
		blk.name, _ = block["name"].(string)
		if blk.name == "" {
			blk.name = "tool"
		}
		// start 自带的完整参数：无 input_json_delta 时由 closeToolEvents 补发
		if raw, merr := json.Marshal(block["input"]); merr == nil && len(raw) > 0 && string(raw) != "null" {
			blk.startArgs = string(raw)
		}
		blk.outputIndex = nextOutputIndex
		nextOutputIndex++
		writeEvent("response.output_item.added", map[string]interface{}{
			"output_index": blk.outputIndex,
			"item": map[string]interface{}{
				"id": blk.itemID, "type": "function_call", "status": "in_progress",
				"call_id": callID, "name": blk.name, "arguments": "",
			},
		})
	}

	closeToolEvents := func(idx int) {
		blk, ok := blocks[idx]
		if !ok || blk.kind != "tool" {
			return
		}
		arguments := blk.jsonBuf
		if arguments == "" {
			arguments = blk.startArgs
		}
		if arguments == "" {
			arguments = "{}"
		}
		var parsed map[string]interface{}
		if err := json.Unmarshal([]byte(arguments), &parsed); err != nil {
			usage.ToolTruncated = true
			delete(blocks, idx)
			return // Do not emit successful tool terminal events for invalid arguments.
		}
		if parsed == nil {
			// Preserve normalization of valid JSON null to an empty object.
			parsed = map[string]interface{}{}
		}
		usage.ToolCalls = append(usage.ToolCalls, map[string]interface{}{
			"id": blk.callID, "name": blk.name, "input": parsed,
		})
		writeEvent("response.function_call_arguments.done", map[string]interface{}{
			"item_id": blk.itemID, "output_index": blk.outputIndex, "arguments": arguments,
		})
		item := map[string]interface{}{
			"id": blk.itemID, "type": "function_call", "status": "completed",
			"call_id": blk.callID, "name": blk.name, "arguments": arguments,
		}
		writeEvent("response.output_item.done", map[string]interface{}{
			"output_index": blk.outputIndex,
			"item":         item,
		})
		outputItems = append(outputItems, item)
		delete(blocks, idx)
	}

	writeEvent("response.created", map[string]interface{}{
		"response": map[string]interface{}{
			"id": responseID, "object": "response", "created_at": createdAt,
			"status": "in_progress", "model": model, "output": []interface{}{},
		},
	})

	parser := &sseParser{}
	handle := func(ev sseEvent) {
		if ttft == 0 {
			ttft = int(time.Since(start).Milliseconds())
		}
		switch ev.Event {
		case "message_start", "message_delta", "error":
			if ev.Event == "message_start" {
				sawStart = true
			}
			applyEventToUsage(ev, &usage, &activeTool, &texts, &thinks)
			return
		}
		idx := toInt(ev.Data["index"])
		switch ev.Event {
		case "content_block_start":
			block, _ := ev.Data["content_block"].(map[string]interface{})
			kind, _ := block["type"].(string)
			if kind == "thinking" {
				// R6：Responses 流式路径同样捕获签名思考块，否则重放缓存
				// 在该端点上恒为空，下一轮工具循环因缺签名思考块而劣化
				usage.initThinkState()
				if sig, ok := block["signature"].(string); ok && sig != "" {
					usage.sigByBlock[idx] = sig
				}
				usage.thinkBufs[idx] = &strings.Builder{}
			}
			if kind == "tool_use" {
				// 未正常关闭的 text/thinking 块先收尾，保证事件序列的 output_index
				// 单调（上游违例交错的兜底）；map 遍历无序，按 index 排序
				stale := make([]int, 0, len(blocks))
				for i := range blocks {
					stale = append(stale, i)
				}
				sort.Ints(stale)
				for _, i := range stale {
					if blocks[i].kind == "text" && blocks[i].itemID != "" {
						closeMessageEvents(i)
					} else if blocks[i].kind == "thinking" && blocks[i].itemID != "" {
						closeReasoningEvents(i)
					}
				}
				blocks[idx] = &blockState{kind: "tool"}
				openToolEvents(idx, block)
			} else if kind == "text" {
				blocks[idx] = &blockState{kind: "text"}
			} else {
				blocks[idx] = &blockState{kind: kind}
			}
		case "content_block_stop":
			usage.finishThinkBlock(idx)
			blk, ok := blocks[idx]
			if !ok {
				return
			}
			if blk.kind == "tool" {
				closeToolEvents(idx)
			} else if blk.kind == "text" {
				closeMessageEvents(idx)
			} else if blk.kind == "thinking" {
				closeReasoningEvents(idx)
			}
		case "content_block_delta":
			delta, _ := ev.Data["delta"].(map[string]interface{})
			if delta == nil {
				return
			}
			blk, ok := blocks[idx]
			if !ok {
				blk = &blockState{kind: "text"}
				blocks[idx] = blk
			}
			switch delta["type"] {
			case "signature_delta":
				// R6：签名增量追加到对应思考块（与 applyEventToUsage 同语义）
				if s, ok := delta["signature"].(string); ok && s != "" {
					usage.initThinkState()
					usage.sigByBlock[idx] += s
				}
			case "thinking_delta":
				if t, ok := delta["thinking"].(string); ok && t != "" {
					if blk.kind == "" {
						blk.kind = "thinking"
					}
					if blk.kind != "thinking" {
						// 非思考块上的 thinking_delta（上游协议违例）：不入 thinks，
						// 否则 completed 会合成事件序列里从未出现过的 reasoning 项
						return
					}
					thinks = append(thinks, t)
					// R6：同步进逐块缓冲，供签名重放缓存取完整块文本
					if buf, ok := usage.thinkBufs[idx]; ok {
						buf.WriteString(t)
					}
					// 思考作为 reasoning 输出项流式发出：客户端事件序列可重放出
					// 与 response.completed.output 一致的状态
					if blk.itemID == "" {
						blk.outputIndex = nextOutputIndex
						nextOutputIndex++
						openReasoningEvents(idx)
					}
					blk.texts = append(blk.texts, t)
					writeEvent("response.reasoning_summary_text.delta", map[string]interface{}{
						"item_id": blk.itemID, "output_index": blk.outputIndex, "summary_index": 0, "delta": t,
					})
				}
			case "input_json_delta":
				if blk.kind == "tool" {
					pj, _ := delta["partial_json"].(string)
					blk.jsonBuf += pj
					writeEvent("response.function_call_arguments.delta", map[string]interface{}{
						"item_id": blk.itemID, "output_index": blk.outputIndex, "delta": pj,
					})
				}
			case "text_delta":
				t, _ := delta["text"].(string)
				if t == "" {
					// 空 delta 不得开启一个空 message 项（会与补空响应的合成项重复）
					return
				}
				if blk.kind == "" {
					blk.kind = "text"
				}
				if blk.kind != "text" {
					return
				}
				if blk.itemID == "" {
					blk.outputIndex = nextOutputIndex
					nextOutputIndex++
					openMessageEvents(idx)
				}
				blk.texts = append(blk.texts, t)
				writeEvent("response.output_text.delta", map[string]interface{}{
					"item_id": blk.itemID, "output_index": blk.outputIndex, "content_index": 0, "delta": t,
				})
			}
		}
	}

	// 上游静默期注入 response.in_progress 心跳（客户端解析联合不含该类型 →
	// 静默忽略），防流空闲超时误判
	keepalive := time.Duration(0)
	if resp.StatusCode == http.StatusOK {
		keepalive = ssePingInterval
	}
	var readErr error
	readErr = pumpSSEWithKeepalive(resp.Body, keepalive,
		func(b []byte) bool {
			if ferr := parser.feed(b, handle); ferr != nil {
				readErr = ferr
				return false
			}
			return true
		},
		func() {
			writeEvent("response.in_progress", map[string]interface{}{})
		})
	parser.flush(handle)

	// 关闭未完成的块（上游异常中断兜底）；map 遍历无序，按 index 排序保证
	// output_index 单调（与正常路径的补收尾一致，客户端事件序列才可重放）
	pending := make([]int, 0, len(blocks))
	for idx := range blocks {
		pending = append(pending, idx)
	}
	sort.Ints(pending)
	for _, idx := range pending {
		blk := blocks[idx]
		if blk.kind == "tool" {
			closeToolEvents(idx)
		} else if blk.kind == "text" && blk.itemID != "" {
			closeMessageEvents(idx)
		} else if blk.kind == "thinking" && blk.itemID != "" {
			closeReasoningEvents(idx)
		}
	}

	// 上游流内错误、中途断流或零事件干净 EOF：发 response.failed 而非伪装 completed
	//（零事件判定与聚合路径同规：不得合成空 message 项 + completed）
	interrupted := readErr != nil && readErr != io.EOF
	zeroEvents := !sawStart && len(texts) == 0 && len(usage.ToolCalls) == 0
	if usage.StreamError != "" || interrupted || zeroEvents || usage.ToolTruncated {
		msg := usage.StreamError
		if msg == "" && interrupted {
			msg = fmt.Sprintf("upstream stream interrupted: %v", readErr)
		}
		if msg == "" && usage.ToolTruncated {
			// 参数中途断流（评审 F7）：不得以 tool_calls + 空参数收尾洗成成功
			msg = "upstream tool arguments truncated"
		}
		if msg == "" {
			msg = "upstream stream ended without any events"
		}
		writeEvent("response.failed", map[string]interface{}{
			"response": map[string]interface{}{
				"id": responseID, "object": "response", "status": "failed", "model": model,
				"error": map[string]string{"message": msg, "code": "stream_error"},
			},
		})
		if flusher != nil {
			flusher.Flush()
		}
		finalizeToolCalls(&usage)
		recStatus := 502
		if interrupted && usage.StreamError == "" && clientGone(r) {
			recStatus = 499
		}
		z.recordUsage(a, r, payload, recStatus, start, ttft, &usage, true)
		return
	}

	fullText := strings.Join(texts, "")
	fullThinking := strings.Join(thinks, "")
	// 上游无任何输出时补空 message 项保持事件序列完整
	if len(usage.ToolCalls) == 0 && fullThinking == "" && fullText == "" {
		emptyIdx := 0
		blocks[emptyIdx] = &blockState{kind: "text", outputIndex: 0}
		openMessageEvents(emptyIdx)
		closeMessageEvents(emptyIdx)
	}
	// max_tokens 截断 → response.incomplete（协议语义同上），其余 completed
	responseObj := responsesResponseWithItems(model, responseID, outputItems, fullText, fullThinking, &usage)
	terminal := "response.completed"
	if responseObj["status"] == "incomplete" {
		terminal = "response.incomplete"
	}
	writeEvent(terminal, map[string]interface{}{"response": responseObj})
	if flusher != nil {
		flusher.Flush()
	}
	finalizeToolCalls(&usage)
	cacheThinkingForOutput(fullText, &usage)
	z.recordUsage(a, r, payload, resp.StatusCode, start, ttft, &usage, true)
}

// ---- OpenAI 响应构造 ----

// openaiFinish stop_reason → OpenAI finish_reason。空 reason 但携带工具调用
// 时按 tool_calls 收尾（上游缺 message_delta 的违例流）：Agents 类客户端以
// finish_reason 驱动工具循环，错报 "stop" 会让会话无错卡死
func openaiFinish(stopReason string, hasToolCalls bool) string {
	switch stopReason {
	case "max_tokens":
		return "length"
	case "refusal":
		return "content_filter"
	case "tool_use":
		return "tool_calls"
	}
	if hasToolCalls {
		return "tool_calls"
	}
	return "stop"
}

func openaiResponse(model, text, thinking string, usage *StreamUsage) map[string]interface{} {
	message := map[string]interface{}{"role": "assistant", "content": text}
	if thinking != "" {
		message["reasoning_content"] = thinking
	}
	if usage != nil && len(usage.ToolCalls) > 0 {
		if text == "" {
			message["content"] = nil
		}
		calls := make([]map[string]interface{}, 0, len(usage.ToolCalls))
		for _, c := range usage.ToolCalls {
			id, _ := c["id"].(string)
			if id == "" {
				id = "call_" + randomHex(8)
			}
			name, _ := c["name"].(string)
			args, _ := json.Marshal(c["input"])
			calls = append(calls, map[string]interface{}{
				"id": id, "type": "function",
				"function": map[string]interface{}{"name": name, "arguments": string(args)},
			})
		}
		message["tool_calls"] = calls
	}
	in, out := 0, 0
	stop := ""
	hasTools := false
	var usageObj map[string]interface{}
	if usage != nil {
		in, out, stop = usage.InputTokens, usage.OutputTokens, usage.StopReason
		hasTools = len(usage.ToolCalls) > 0
		usageObj = map[string]interface{}{
			"prompt_tokens": in, "completion_tokens": out, "total_tokens": in + out,
		}
		// openai-compatible 客户端从 prompt_tokens_details.cached_tokens 读缓存命中
		//（round-4 闭集）；无缓存时不带该键（nullish 合法）
		if usage.CacheReadTokens > 0 {
			usageObj["prompt_tokens_details"] = map[string]interface{}{"cached_tokens": usage.CacheReadTokens}
		}
	}
	if usageObj == nil {
		usageObj = map[string]interface{}{
			"prompt_tokens": in, "completion_tokens": out, "total_tokens": in + out,
		}
	}
	return map[string]interface{}{
		"id":      "chatcmpl-" + randomHex(12),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]interface{}{{
			"index": 0, "message": message, "finish_reason": openaiFinish(stop, hasTools),
		}},
		"usage": usageObj,
	}
}

func newResponseID() string { return "resp_" + randomHex(12) }

func responsesResponse(model, responseID, text, thinking string, usage *StreamUsage) map[string]interface{} {
	return responsesResponseWithItems(model, responseID, nil, text, thinking, usage)
}

// responsesResponseWithItems 构造 Response 对象；items 为流式阶段已发出的最终输出项时
// 直接复用（保留 msg_/fc_/rs_ ID 供客户端关联事件序列），仅当流中没有出现过
// reasoning 项时才补合成（非流式聚合路径的 items 为 nil，走合成）
func responsesResponseWithItems(model, responseID string, items []map[string]interface{}, text, thinking string, usage *StreamUsage) map[string]interface{} {
	var output []map[string]interface{}
	hasReasoning := false
	for _, it := range items {
		if it["type"] == "reasoning" {
			hasReasoning = true
			break
		}
	}
	if thinking != "" && !hasReasoning {
		output = append(output, map[string]interface{}{
			"id": "rs_" + randomHex(8), "type": "reasoning",
			"summary": []map[string]interface{}{{"type": "summary_text", "text": thinking}},
		})
	}
	if len(items) > 0 {
		output = append(output, items...)
	} else {
		if usage != nil {
			for _, c := range usage.ToolCalls {
				id, _ := c["id"].(string)
				if id == "" {
					id = "call_" + randomHex(8)
				}
				name, _ := c["name"].(string)
				args, _ := json.Marshal(c["input"])
				output = append(output, map[string]interface{}{
					"id": "fc_" + randomHex(8), "type": "function_call",
					"call_id": id, "name": name, "arguments": string(args),
				})
			}
		}
		if text != "" || len(output) == 0 {
			output = append(output, map[string]interface{}{
				"id": "msg_" + randomHex(8), "type": "message", "status": "completed",
				"role": "assistant",
				"content": []map[string]interface{}{
					{"type": "output_text", "text": text, "annotations": []interface{}{}},
				},
			})
		}
	}
	in, out := 0, 0
	if usage != nil {
		in, out = usage.InputTokens, usage.OutputTokens
	}
	// max_tokens 截断按 Responses 协议语义回 status:"incomplete" +
	// incomplete_details.max_output_tokens（客户端据此映射 finish=length）；
	// 恒发 completed 会把截断谎报成正常 stop（轮 8）
	status := "completed"
	if usage != nil && usage.StopReason == "max_tokens" {
		status = "incomplete"
	}
	// Responses 客户端从 input_tokens_details.cached_tokens 读缓存命中（round-4 闭集）
	usageObj := map[string]interface{}{
		"input_tokens": in, "output_tokens": out, "total_tokens": in + out,
	}
	if usage != nil && usage.CacheReadTokens > 0 {
		usageObj["input_tokens_details"] = map[string]interface{}{"cached_tokens": usage.CacheReadTokens}
	}
	out2 := map[string]interface{}{
		"id": responseID, "object": "response", "created_at": time.Now().Unix(),
		"status": status, "model": model,
		"output":      output,
		"output_text": text,
		"usage":       usageObj,
	}
	if status == "incomplete" {
		out2["incomplete_details"] = map[string]interface{}{"reason": "max_output_tokens"}
	}
	return out2
}

// ---- OpenAI legacy text_completion 构造（/v1/completions shim）----

// completionsFinish legacy text_completion 的 finish_reason：无 tool_calls 语义，
// 工具调用回合只能回 stop
func completionsFinish(stopReason string) string {
	if stopReason == "max_tokens" {
		return "length"
	}
	return "stop"
}

// completionsResponse 构造非流式 text_completion 响应；echo=true 时 text 前缀原 prompt
func completionsResponse(model, text string, usage *StreamUsage, echo bool, prompt string) map[string]interface{} {
	if echo {
		text = prompt + text
	}
	in, out := 0, 0
	stop := ""
	if usage != nil {
		in, out, stop = usage.InputTokens, usage.OutputTokens, usage.StopReason
	}
	return map[string]interface{}{
		"id":      "cmpl-" + randomHex(12),
		"object":  "text_completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]interface{}{{
			"index": 0, "text": text, "logprobs": nil, "finish_reason": completionsFinish(stop),
		}},
		"usage": map[string]interface{}{
			"prompt_tokens": in, "completion_tokens": out, "total_tokens": in + out,
		},
	}
}

// streamCompletions Anthropic SSE → OpenAI legacy text_completion.chunk。
// 思考/工具块无 legacy 槽位：只计 usage 与思考重放缓存，不进输出流。
func (z *ZCodeAPI) streamCompletions(w http.ResponseWriter, flusher http.Flusher, resp *http.Response,
	model string, includeUsage, echo bool, prompt string, a *Account, r *http.Request, payload []byte, start time.Time) {

	now := time.Now().Unix()
	cid := "cmpl-" + randomHex(12)
	var usage StreamUsage
	var activeTool map[string]interface{}
	var texts, thinks []string
	sawStart := false // 见过 message_start（与聚合路径 sawStart 同义，零事件判定用）
	ttft := 0
	echoPending := echo

	writeChunk := func(text string, finish interface{}, chunkUsage interface{}) {
		p := map[string]interface{}{
			"id": cid, "object": "text_completion", "created": now, "model": model,
			"choices": []map[string]interface{}{{
				"index": 0, "text": text, "logprobs": nil, "finish_reason": finish,
			}},
		}
		if chunkUsage != nil {
			p["usage"] = chunkUsage
		}
		b, _ := json.Marshal(p)
		fmt.Fprintf(w, "data: %s\n\n", b)
		if flusher != nil {
			flusher.Flush()
		}
	}

	parser := &sseParser{}
	handle := func(ev sseEvent) {
		if ttft == 0 {
			ttft = int(time.Since(start).Milliseconds())
		}
		if ev.Event == "message_start" {
			sawStart = true
		}
		applyEventToUsage(ev, &usage, &activeTool, &texts, &thinks)
		if ev.Event != "content_block_delta" {
			return
		}
		delta, _ := ev.Data["delta"].(map[string]interface{})
		if delta == nil || delta["type"] != "text_delta" {
			return
		}
		t, _ := delta["text"].(string)
		if t == "" {
			return
		}
		if echoPending {
			echoPending = false
			t = prompt + t
		}
		writeChunk(t, nil, nil)
	}

	// 上游静默期注入空 choices 心跳（与 streamOpenAI 同动机，text_completion 形状）
	keepalive := time.Duration(0)
	if resp.StatusCode == http.StatusOK {
		keepalive = ssePingInterval
	}
	var readErr error
	readErr = pumpSSEWithKeepalive(resp.Body, keepalive,
		func(b []byte) bool {
			if ferr := parser.feed(b, handle); ferr != nil {
				readErr = ferr
				return false
			}
			return true
		},
		func() {
			hb, _ := json.Marshal(map[string]interface{}{
				"id": cid, "object": "text_completion", "created": now, "model": model,
				"choices": []interface{}{},
			})
			fmt.Fprintf(w, "data: %s\n\n", hb)
			if flusher != nil {
				flusher.Flush()
			}
		})
	parser.flush(handle)
	finalizeToolCalls(&usage)
	cacheThinkingForOutput(strings.Join(texts, ""), &usage)

	// 上游流内错误、中途断流或零事件干净 EOF：发 OpenAI 错误 chunk 而非伪装成功
	//（零事件判定与聚合路径同规：不得合成 200 空助手回合）
	interrupted := readErr != nil && readErr != io.EOF
	zeroEvents := !sawStart && len(texts) == 0 && len(usage.ToolCalls) == 0
	if usage.StreamError != "" || interrupted || zeroEvents || usage.ToolTruncated {
		msg := usage.StreamError
		if msg == "" && interrupted {
			msg = fmt.Sprintf("upstream stream interrupted: %v", readErr)
		}
		if msg == "" && usage.ToolTruncated {
			// 参数中途断流（评审 F7）：不得以 tool_calls + 空参数收尾洗成成功
			msg = "upstream tool arguments truncated"
		}
		if msg == "" {
			msg = "upstream stream ended without any events"
		}
		ep, _ := json.Marshal(map[string]interface{}{
			"error": map[string]interface{}{"message": msg, "type": "api_error", "code": "stream_error"},
		})
		fmt.Fprintf(w, "data: %s\n\n", ep)
		fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		recStatus := 502
		if interrupted && usage.StreamError == "" && clientGone(r) {
			recStatus = 499
		}
		z.recordUsage(a, r, payload, recStatus, start, ttft, &usage, true)
		return
	}

	// echo 且上游零输出：prompt 前缀仍须发出，不能静默丢掉
	if echoPending {
		writeChunk(prompt, nil, nil)
	}
	writeChunk("", completionsFinish(usage.StopReason), nil)
	if includeUsage {
		finalUsage := map[string]interface{}{
			"prompt_tokens":     usage.InputTokens,
			"completion_tokens": usage.OutputTokens,
			"total_tokens":      usage.InputTokens + usage.OutputTokens,
		}
		// 缓存命中明细（round-4 闭集：openai-compatible 客户端读取该键）
		if usage.CacheReadTokens > 0 {
			finalUsage["prompt_tokens_details"] = map[string]interface{}{"cached_tokens": usage.CacheReadTokens}
		}
		p, _ := json.Marshal(map[string]interface{}{
			"id": cid, "object": "text_completion", "created": now, "model": model,
			"choices": []interface{}{}, "usage": finalUsage,
		})
		fmt.Fprintf(w, "data: %s\n\n", p)
		if flusher != nil {
			flusher.Flush()
		}
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
	z.recordUsage(a, r, payload, resp.StatusCode, start, ttft, &usage, true)
}

func randomHex(n int) string {
	return strings.ReplaceAll(uuid.NewString(), "-", "")[:n]
}
