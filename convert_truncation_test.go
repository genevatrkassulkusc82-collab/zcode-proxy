package main

// F7 契约测试：截断的工具参数（不可解析的非空 partial_json）必须按流失败处理，
// 不得洗成 input:{} 的"成功空参 tool_use"。空缓冲容忍保留（上游 start 帧
// 自带完整 input、不发 delta 是合法形状）；字面量 "null" 容忍保留。

import (
	"strings"
	"testing"
)

func TestFinalizeToolCallsMarksTruncation(t *testing.T) {
	// 不可解析的非空缓冲 = 截断：必须置位 ToolTruncated
	u := &StreamUsage{ToolCalls: []map[string]interface{}{
		{"type": "tool_use", "id": "t1", "name": "f", "_json": `{"a":`},
	}}
	finalizeToolCalls(u)
	if !u.ToolTruncated {
		t.Fatal("unparseable non-empty tool args must set ToolTruncated")
	}

	// 完整 JSON：不误报，正常解析
	u2 := &StreamUsage{ToolCalls: []map[string]interface{}{
		{"type": "tool_use", "_json": `{"a":1}`},
	}}
	finalizeToolCalls(u2)
	if u2.ToolTruncated {
		t.Fatal("valid tool JSON must not be marked truncated")
	}
	in, _ := u2.ToolCalls[0]["input"].(map[string]interface{})
	if in == nil || in["a"] != float64(1) {
		t.Fatalf("valid args mis-parsed: %v", u2.ToolCalls[0]["input"])
	}

	// 空 raw + start 帧自带 input：合法，不置位
	u3 := &StreamUsage{ToolCalls: []map[string]interface{}{
		{"type": "tool_use", "input": map[string]interface{}{"b": float64(2)}},
	}}
	finalizeToolCalls(u3)
	if u3.ToolTruncated {
		t.Fatal("empty buffer with start-frame input must stay tolerated")
	}

	// 字面量 "null"：既有容忍（Unmarshal 成功得 nil → {}），不置位
	u4 := &StreamUsage{ToolCalls: []map[string]interface{}{{"_json": "null"}}}
	finalizeToolCalls(u4)
	if u4.ToolTruncated {
		t.Fatal("null literal must stay tolerated")
	}
}

func TestAggregateAnthropicStreamToolTruncationFails(t *testing.T) {
	// message_start + tool_use start + 半截 input_json_delta + 干净 EOF
	// （无 content_block_stop）：聚合必须报错，不得产出 input:{} 的 tool_use
	body := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"m1","model":"glm","usage":{"input_tokens":1,"output_tokens":1}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t1","name":"f","input":{}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"a\":"}}` + "\n\n"
	_, _, err := aggregateAnthropicStream(strings.NewReader(body))
	if err == nil || !strings.Contains(err.Error(), "truncat") {
		t.Fatalf("truncated tool args must fail aggregation, got err=%v", err)
	}
}

func TestAggregateAnthropicStreamCompleteToolCallOK(t *testing.T) {
	// 完整 tool_use（stop 帧收尾）：照常聚合成功
	body := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"m1","model":"glm","usage":{"input_tokens":1,"output_tokens":1}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t1","name":"f","input":{}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"a\":1}"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}` + "\n\n"
	aggregated, _, err := aggregateAnthropicStream(strings.NewReader(body))
	if err != nil {
		t.Fatalf("complete tool call must aggregate, got %v", err)
	}
	if !strings.Contains(string(aggregated), `"a":1`) {
		t.Fatalf("tool input lost in aggregation: %s", aggregated)
	}
}
