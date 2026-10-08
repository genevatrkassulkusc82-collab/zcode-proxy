package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestReviewOpenAIConversionDoesNotReplayUnrelatedThinking(t *testing.T) {
	// Serial: response conversion uses a process-global cache. Restore it even on failure.
	previous := thinkingReplay
	thinkingReplay = newThinkingCache()
	t.Cleanup(func() { thinkingReplay = previous })
	for _, tc := range []struct {
		name    string
		visible map[string]interface{}
	}{
		{"text", map[string]interface{}{"type": "text", "text": "A common answer"}},
		{"tool", map[string]interface{}{"type": "tool_use", "id": "call_review_private", "name": "lookup", "input": map[string]interface{}{"a": float64(1)}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Request A's upstream response contains private signed thinking.
			upstream, err := json.Marshal(map[string]interface{}{
				"content": []interface{}{
					map[string]interface{}{"type": "thinking", "thinking": "request A private context", "signature": "request-A-signature"},
					tc.visible,
				},
				"stop_reason": "end_turn",
			})
			if err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			writeProtocolResponse(w, &relayCtx{proto: protocolOpenAI, clientModel: "GLM-5.3"}, http.StatusOK, "application/json", upstream, nil)
			var response struct {
				Choices []struct {
					Message map[string]interface{} `json:"message"`
				} `json:"choices"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if len(response.Choices) != 1 {
				t.Fatalf("response conversion produced no single assistant choice: %s", w.Body.String())
			}
			converted := response.Choices[0].Message
			// Request B supplies only matching public output, not A's reasoning/signature.
			assistant := map[string]interface{}{"role": "assistant", "content": converted["content"]}
			if tools, ok := converted["tool_calls"]; ok {
				assistant["tool_calls"] = tools
			}
			request, err := openaiToAnthropic(map[string]interface{}{
				"model": "GLM-5.3",
				"messages": []interface{}{
					map[string]interface{}{"role": "user", "content": "An unrelated user's conversation"},
					assistant,
					map[string]interface{}{"role": "user", "content": "Continue"},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			// Decode wire form to avoid coupling assertions to slice representation.
			wire, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			var anthropic struct {
				Messages []struct {
					Role    string                   `json:"role"`
					Content []map[string]interface{} `json:"content"`
				} `json:"messages"`
			}
			if err := json.Unmarshal(wire, &anthropic); err != nil {
				t.Fatal(err)
			}
			if len(anthropic.Messages) != 3 || anthropic.Messages[1].Role != "assistant" {
				t.Fatalf("conversation changed unexpectedly: %s", wire)
			}
			want := []map[string]interface{}{tc.visible}
			if got := anthropic.Messages[1].Content; !reflect.DeepEqual(got, want) {
				t.Fatalf("unrelated request must retain public output only, without cached private thinking: got %#v, want %#v", got, want)
			}
		})
	}
}

func TestReviewExplicitSignedThinkingSurvivesNativeNormalization(t *testing.T) {
	// OpenAI content parts do not support signed thinking. Native Anthropic does;
	// disabling implicit replay must not strip thinking explicitly supplied there.
	_, db := newPaidTestPool(t)
	body := map[string]interface{}{
		"model": "GLM-5.3",
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "hello"},
			map[string]interface{}{"role": "assistant", "content": []interface{}{
				map[string]interface{}{"type": "thinking", "thinking": "explicit private context", "signature": "explicit-signature"},
				map[string]interface{}{"type": "text", "text": "A common answer"},
			}},
			map[string]interface{}{"role": "user", "content": "continue"},
		},
	}
	if err := normalizeBody(body, &ZCodeAPI{cfg: &FileConfig{}, db: db}); err != nil {
		t.Fatal(err)
	}
	if err := validateMessagesBody(context.Background(), body); err != nil {
		t.Fatal(err)
	}
	messages := body["messages"].([]interface{})
	content := messages[1].(map[string]interface{})["content"].([]interface{})
	want := map[string]interface{}{"type": "thinking", "thinking": "explicit private context", "signature": "explicit-signature"}
	if len(content) != 2 || !reflect.DeepEqual(content[0], want) {
		t.Fatalf("explicit signed thinking must survive unchanged: got %#v", content)
	}
}
