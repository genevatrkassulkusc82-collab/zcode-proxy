package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEstimateTextTokens(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
	}{
		{"empty", "", 0},
		{"ascii 28 chars", "Hello world, this is a test!", 7}, // 28/4=7
		{"cjk 4 chars", "你好世界", 4},                            // 1 token/char
		{"cjk 100 chars", strings.Repeat("汉", 100), 105},      // 100 + 5% margin
		{"mixed", "abc你好", 2},                                 // ascii 3/4=0, cjk=2 → 2*21/20=2
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := estimateTextTokens(c.in); got != c.want {
				t.Errorf("estimateTextTokens(%q) = %d, want %d", c.in, got, c.want)
			}
		})
	}
}

func TestEstimateTextTokensCJKDominates(t *testing.T) {
	// 同字符数下 CJK 估算应显著高于 ASCII（CJK≈1字/令牌，ASCII≈4字符/令牌）
	ascii := estimateTextTokens(strings.Repeat("a", 100))
	cjk := estimateTextTokens(strings.Repeat("汉", 100))
	if cjk <= ascii {
		t.Errorf("cjk est %d should exceed ascii est %d for equal char counts", cjk, ascii)
	}
}

func TestEstimateContentTokens(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
	}{
		{"empty", ``, 0},
		{"plain string", `"Hello world, this is a test!"`, 7},
		{"text block", `[{"type":"text","text":"Hello world, this is a test!"}]`, 7},
		{"thinking block", `[{"type":"thinking","thinking":"你好世界"}]`, 4},
		{"image block fixed cost", `[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]`, estImageTokens},
		{"tool_use input", `[{"type":"tool_use","id":"t1","name":"bash","input":{"command":"ls -la"}}]`, 5},
		{"tool_result nested array", `[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"output here"}]}]`, 2},
		{"unknown block falls back", `[{"type":"weird_thing","foo":"bar"}]`, 4}, // 34字节raw/2兜底
		{"malformed falls back", `{{{not json`, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := estimateContentTokens(json.RawMessage(c.in))
			if got != c.want {
				t.Errorf("estimateContentTokens(%s) = %d, want %d", c.in, got, c.want)
			}
		})
	}
}

func TestEstimateMessagesTokens(t *testing.T) {
	// 2 条消息：每条 overhead 4 + 内容
	in := `[{"role":"user","content":"Hello world, this is a test!"},{"role":"assistant","content":"你好世界"}]`
	want := 2*4 + 7 + 4 // overhead*2 + 7 + 4
	if got := estimateMessagesTokens(json.RawMessage(in)); got != want {
		t.Errorf("estimateMessagesTokens = %d, want %d", got, want)
	}
	// 空与畸形
	if got := estimateMessagesTokens(nil); got != 0 {
		t.Errorf("nil messages = %d, want 0", got)
	}
	if got := estimateMessagesTokens(json.RawMessage(`not json`)); got <= 0 {
		t.Errorf("malformed messages should fall back to nonzero estimate, got %d", got)
	}
}

// 完整端点行为：请求形状不变（仅 input_tokens），估算随内容结构变化
func TestHandleCountTokens(t *testing.T) {
	z := &ZCodeAPI{}

	post := func(t *testing.T, body string) int {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(body))
		rec := httptest.NewRecorder()
		z.HandleCountTokens(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		var resp struct {
			InputTokens int `json:"input_tokens"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("bad json: %v (%s)", err, rec.Body.String())
		}
		return resp.InputTokens
	}

	t.Run("empty body counts base only", func(t *testing.T) {
		if got := post(t, `{}`); got != estBaseTokens {
			t.Errorf("empty = %d, want %d", got, estBaseTokens)
		}
	})

	t.Run("realistic claude code payload", func(t *testing.T) {
		body := `{
			"system": [{"type":"text","text":"You are Claude Code, a coding assistant."}],
			"messages": [
				{"role":"user","content":[{"type":"text","text":"Fix the failing test in main.go"}]},
				{"role":"assistant","content":[{"type":"tool_use","id":"tu_1","name":"bash","input":{"command":"go test ./..."}}]},
				{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_1","content":[{"type":"text","text":"ok"}]}]}
			],
			"tools": [{"name":"bash","description":"Run a shell command","input_schema":{"type":"object","properties":{"command":{"type":"string"}}}}]
		}`
		got := post(t, body)
		if got < 50 || got > 300 {
			t.Errorf("estimate %d outside plausible range [50,300] for small payload", got)
		}
	})

	t.Run("json structural overhead ignored", func(t *testing.T) {
		// 相同内容，一个塞满未知字段与结构噪音：估算应完全一致（只按内容计）
		compact := `{"messages":[{"role":"user","content":"Hello world, this is a test!"}]}`
		padded := `{"messages":[{"role":"user","content":"Hello world, this is a test!"}],` +
			`"pad":"` + strings.Repeat(" ", 400) + `","pad2":[[[[1]]]],"pad3":{"deep":{"deeper":[1,2,3]}}}`
		a, b := post(t, compact), post(t, padded)
		if a != b {
			t.Errorf("padded payload estimated %d vs compact %d — JSON overhead still leaking in", b, a)
		}
	})

	t.Run("cjk counted heavier than ascii", func(t *testing.T) {
		asciiBody := `{"messages":[{"role":"user","content":"` + strings.Repeat("a", 200) + `"}]}`
		cjkBody := `{"messages":[{"role":"user","content":"` + strings.Repeat("汉", 100) + `"}]}`
		a, b := post(t, asciiBody), post(t, cjkBody)
		if b <= a {
			t.Errorf("cjk est %d should exceed ascii est %d", b, a)
		}
	})

	t.Run("method guard", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/messages/count_tokens", nil)
		rec := httptest.NewRecorder()
		z.HandleCountTokens(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("GET status = %d, want 405", rec.Code)
		}
	})

	t.Run("invalid json rejected", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(`{bad`))
		rec := httptest.NewRecorder()
		z.HandleCountTokens(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("bad json status = %d, want 400", rec.Code)
		}
	})
}
