package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestCompletionsPromptExtraction prompt 提取与归一化矩阵
func TestCompletionsPromptExtraction(t *testing.T) {
	if s, err := completionsPrompt("hello"); err != nil || s != "hello" {
		t.Fatalf("string prompt: %q %v", s, err)
	}
	if s, err := completionsPrompt(nil); err != nil || s != "" {
		t.Fatalf("nil prompt: %q %v", s, err)
	}
	if s, err := completionsPrompt([]interface{}{"a", "b"}); err != nil || s != "a\n\nb" {
		t.Fatalf("array prompt: %q %v", s, err)
	}
	if _, err := completionsPrompt([]interface{}{"a", 3}); err == nil {
		t.Fatal("array with non-string must error")
	}
	if _, err := completionsPrompt(42); err == nil {
		t.Fatal("non-string non-array must error")
	}
}

// TestCompletionsFinishMapping legacy 端点无 tool_calls 语义
func TestCompletionsFinishMapping(t *testing.T) {
	cases := map[string]string{
		"max_tokens": "length",
		"tool_use":   "stop",
		"stop":       "stop",
		"end_turn":   "stop",
		"":           "stop",
	}
	for in, want := range cases {
		if got := completionsFinish(in); got != want {
			t.Errorf("completionsFinish(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestCompletionsResponseShape 非流式 text_completion 形状 + echo 前缀
func TestCompletionsResponseShape(t *testing.T) {
	u := &StreamUsage{InputTokens: 11, OutputTokens: 7, StopReason: "max_tokens"}
	resp := completionsResponse("my-model", " world", u, true, "hello")
	if resp["object"] != "text_completion" {
		t.Fatalf("object = %v", resp["object"])
	}
	if id, _ := resp["id"].(string); !strings.HasPrefix(id, "cmpl-") {
		t.Fatalf("id = %v", id)
	}
	choices, _ := resp["choices"].([]map[string]interface{})
	if len(choices) != 1 {
		t.Fatalf("choices len = %d", len(choices))
	}
	c := choices[0]
	if c["text"] != "hello world" {
		t.Fatalf("echo text = %v", c["text"])
	}
	if c["finish_reason"] != "length" {
		t.Fatalf("finish_reason = %v", c["finish_reason"])
	}
	if c["index"] != 0 || c["logprobs"] != nil {
		t.Fatalf("choice shape: %v", c)
	}
	usage, _ := resp["usage"].(map[string]interface{})
	if usage["prompt_tokens"] != 11 || usage["completion_tokens"] != 7 || usage["total_tokens"] != 18 {
		t.Fatalf("usage = %v", usage)
	}

	noEcho := completionsResponse("my-model", " world", u, false, "hello")
	nc := noEcho["choices"].([]map[string]interface{})[0]
	if nc["text"] != " world" {
		t.Fatalf("no-echo text = %v", nc["text"])
	}
}

func newCompletionsTestAPI(t *testing.T) *ZCodeAPI {
	t.Helper()
	db, _ := newVaultTestDB(t)
	return &ZCodeAPI{cfg: &FileConfig{Models: []string{"GLM-5.3", "glm-4.5-air"}}, db: db}
}

// newCompletionsTestDB 仅带 DB 的最小 ZCodeAPI（流式写回路径只需落 usage）
func newCompletionsTestDB(t *testing.T) *DB {
	t.Helper()
	db, _ := newVaultTestDB(t)
	return db
}

// TestHandleModelRetrieve 单模型查询：命中 / 未命中 / 方法 / 路径形态 / DB 覆盖
func TestHandleModelRetrieve(t *testing.T) {
	z := newCompletionsTestAPI(t)

	get := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		z.HandleModelRetrieve(w, httptest.NewRequest(http.MethodGet, path, nil))
		return w
	}

	w := get("/v1/models/GLM-5.3")
	if w.Code != http.StatusOK {
		t.Fatalf("found: code = %d body=%s", w.Code, w.Body.String())
	}
	var m map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["id"] != "GLM-5.3" || m["object"] != "model" || m["type"] != "model" {
		t.Fatalf("model object = %v", m)
	}

	if w := get("/v1/models/nope"); w.Code != http.StatusNotFound {
		t.Fatalf("missing model: code = %d", w.Code)
	}
	if w := get("/v1/models/"); w.Code != http.StatusNotFound {
		t.Fatalf("empty id: code = %d", w.Code)
	}
	if w := get("/v1/models/a/b"); w.Code != http.StatusNotFound {
		t.Fatalf("nested path: code = %d", w.Code)
	}

	w = httptest.NewRecorder()
	z.HandleModelRetrieve(w, httptest.NewRequest(http.MethodPost, "/v1/models/GLM-5.3", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST: code = %d", w.Code)
	}

	// gateway_models 覆盖优先于配置文件清单
	if err := z.db.SetSetting("gateway_models", "custom-a, custom-b"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	if w := get("/v1/models/custom-b"); w.Code != http.StatusOK {
		t.Fatalf("override model: code = %d body=%s", w.Code, w.Body.String())
	}
	if w := get("/v1/models/GLM-5.3"); w.Code != http.StatusNotFound {
		t.Fatalf("override hides config models: code = %d", w.Code)
	}
}

// completionsSSEUpstream 假上游：回放给定 Anthropic SSE 帧
func completionsSSEUpstream(t *testing.T, frames string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(frames))
	}))
}

// TestStreamCompletionsChunks 流式 text_completion.chunk：echo 前缀并入首个 delta、
// 结束帧、usage 帧、[DONE]
func TestStreamCompletionsChunks(t *testing.T) {
	frames := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":5,\"output_tokens\":1}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\" world\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	srv := completionsSSEUpstream(t, frames)
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("upstream get: %v", err)
	}
	defer resp.Body.Close()

	z := &ZCodeAPI{db: newCompletionsTestDB(t)}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/completions", nil)
	z.streamCompletions(w, nil, resp, "my-model", true, true, "PROMPT:",
		&Account{}, req, []byte(`{"model":"my-model"}`), time.Now())

	outFrames := strings.Split(strings.TrimSpace(w.Body.String()), "\n\n")
	if len(outFrames) != 5 || outFrames[4] != "data: [DONE]" {
		t.Fatalf("unexpected frame count/ending: %q", w.Body.String())
	}
	var chunks []map[string]interface{}
	for _, ln := range outFrames[:4] {
		if !strings.HasPrefix(ln, "data: ") {
			t.Fatalf("bad frame: %q", ln)
		}
		var ch map[string]interface{}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(ln, "data: ")), &ch); err != nil {
			t.Fatalf("unmarshal chunk %q: %v", ln, err)
		}
		if ch["object"] != "text_completion.chunk" {
			t.Fatalf("chunk object = %v", ch["object"])
		}
		chunks = append(chunks, ch)
	}
	textOf := func(i int) string {
		choice := chunks[i]["choices"].([]interface{})[0].(map[string]interface{})
		return choice["text"].(string)
	}
	if textOf(0) != "PROMPT:Hello" {
		t.Fatalf("chunk0 text = %v (echo prefix must merge into first delta)", textOf(0))
	}
	if textOf(1) != " world" {
		t.Fatalf("chunk1 text = %v", textOf(1))
	}
	last := chunks[2]["choices"].([]interface{})[0].(map[string]interface{})
	if last["text"] != "" || last["finish_reason"] != "stop" {
		t.Fatalf("final chunk = %v", last)
	}
	if got := chunks[3]["choices"].([]interface{}); len(got) != 0 {
		t.Fatalf("usage chunk must have empty choices, got %v", got)
	}
	u := chunks[3]["usage"].(map[string]interface{})
	if u["prompt_tokens"] != float64(5) || u["completion_tokens"] != float64(3) {
		t.Fatalf("usage chunk = %v", u)
	}
}

// TestStreamCompletionsUpstreamError 上游流内错误必须以 error 帧收尾，不得伪装成功
func TestStreamCompletionsUpstreamError(t *testing.T) {
	srv := completionsSSEUpstream(t, "data: {\"type\":\"error\",\"error\":{\"message\":\"overloaded_error\"}}\n\n")
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("upstream get: %v", err)
	}
	defer resp.Body.Close()

	z := &ZCodeAPI{db: newCompletionsTestDB(t)}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/completions", nil)
	z.streamCompletions(w, nil, resp, "my-model", false, false, "",
		&Account{}, req, []byte(`{}`), time.Now())

	out := w.Body.String()
	if !strings.Contains(out, `"error"`) || !strings.Contains(out, "overloaded_error") {
		t.Fatalf("error frame missing: %q", out)
	}
	if !strings.HasSuffix(strings.TrimSpace(out), "data: [DONE]") {
		t.Fatalf("must end with [DONE]: %q", out)
	}
	if strings.Contains(out, `"finish_reason"`) {
		t.Fatalf("error path must not emit a success finish frame: %q", out)
	}
}
