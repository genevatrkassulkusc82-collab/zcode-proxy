package main

import (
	"testing"
)

// TestIsErrorEnvelope 锁死 2xx 内联错误信封识别的形状矩阵——
// 漏检会把上游错误洗成"成功空响应"，误检会毁掉正常 message。
func TestIsErrorEnvelope(t *testing.T) {
	cases := []struct {
		body string
		want bool
	}{
		{`{"type":"error","error":{"message":"x"}}`, true},            // Anthropic 风格
		{`{"type":"error","error":"rate limited"}`, true},             // 字符串 error（曾漏检）
		{`{"error":{"message":"x","type":"api_error"}}`, true},        // 顶层 error 对象
		{`{"error":"oops"}`, true},                                    // 字符串 error
		{`{"error":null}`, false},                                     // 显式 null 不算
		{`{"code":500,"message":"boom"}`, true},                       // 数值非零 code
		{`{"code":"500","message":"boom"}`, true},                     // 字符串 code
		{`{"code":0,"data":{}}`, false},                               // 成功信封
		{`{"id":"msg_1","type":"message","role":"assistant"}`, false}, // 正常 message
		{`{"content":[{"type":"text","text":"hi"}]}`, false},          // 无信封字段
		{`not json at all`, false},                                    // 非 JSON（由 CT 守卫负责）
		{`{"type":"message","error":{}}`, true},                       // type 不是 error 但带 error 字段
	}
	for _, tc := range cases {
		if got := isErrorEnvelope([]byte(tc.body)); got != tc.want {
			t.Errorf("isErrorEnvelope(%s): got %v want %v", tc.body, got, tc.want)
		}
	}
}

// TestParseJA3Lists 锁死 JA3 列表解析的严格性：非法 token 必须报错
// （静默跳过会产出残缺 ClientHello，握手必败且难排查）。
func TestParseJA3Lists(t *testing.T) {
	if v, err := parseU16List("771-4865-0"); err != nil || len(v) != 3 {
		t.Fatalf("parseU16List valid: v=%v err=%v", v, err)
	}
	if v, err := parseU16List(""); err != nil || v != nil {
		t.Fatalf("parseU16List empty: v=%v err=%v", v, err)
	}
	if _, err := parseU16List("4865-abc"); err == nil {
		t.Fatal("parseU16List: invalid token must error")
	}
	if _, err := parseU16List("70000"); err == nil {
		t.Fatal("parseU16List: >65535 must error")
	}
	if v, err := parseU8List("0-23"); err != nil || len(v) != 2 {
		t.Fatalf("parseU8List valid: v=%v err=%v", v, err)
	}
	if _, err := parseU8List("256"); err == nil {
		t.Fatal("parseU8List: >255 must error")
	}
	// 空 cipher 套件的 spec 必须被拒绝（ja3ToClientHelloSpec 层）
	if _, err := ja3ToClientHelloSpec("771,,0-23,29-23-24,0"); err == nil {
		t.Fatal("ja3ToClientHelloSpec: empty cipher list must error")
	}
	if _, err := ja3ToClientHelloSpec("771,abc-4865,0-23,29-23-24,0"); err == nil {
		t.Fatal("ja3ToClientHelloSpec: invalid token must error")
	}
}

// TestMergeSameRoleMessages 钉住 OpenAI 并行工具调用 → Anthropic 交替语义的合并
func TestMergeSameRoleMessages(t *testing.T) {
	mk := func(role string, blockType string) map[string]interface{} {
		return map[string]interface{}{
			"role":    role,
			"content": []map[string]interface{}{{"type": blockType}},
		}
	}
	msgs := []map[string]interface{}{
		mk("user", "tool_result"),
		mk("user", "tool_result"),
		mk("user", "text"), // 第三条连续 user：同样并入
		mk("assistant", "text"),
	}
	merged := mergeSameRoleMessages(msgs)
	if len(merged) != 2 {
		t.Fatalf("expected 2 messages after merge, got %d", len(merged))
	}
	userBlocks := merged[0]["content"].([]map[string]interface{})
	if len(userBlocks) != 3 {
		t.Fatalf("expected 3 merged user blocks, got %d", len(userBlocks))
	}
	// 字符串 content 归一化为块后同样参与合并
	stringMsgs := []map[string]interface{}{
		{"role": "user", "content": "a"},
		{"role": "user", "content": "b"},
	}
	merged2 := mergeSameRoleMessages(stringMsgs)
	if len(merged2) != 1 {
		t.Fatalf("string contents should merge, got %d messages", len(merged2))
	}
	// 交替的 user/assistant 不得合并
	alt := []map[string]interface{}{mk("user", "text"), mk("assistant", "text")}
	if got := mergeSameRoleMessages(alt); len(got) != 2 {
		t.Fatalf("alternating roles must not merge, got %d", len(got))
	}
}
