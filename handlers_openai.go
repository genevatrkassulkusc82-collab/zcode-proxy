package main

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
)

// ---- OpenAI 兼容端点 ----
// POST /v1/chat/completions 与 POST /v1/responses：
// 请求体转 Anthropic Messages 格式，内部一律流式请求上游，按客户端需求聚合或转 SSE。

// HandleChatCompletions POST /v1/chat/completions
func (z *ZCodeAPI) HandleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	body, errResp := readJSONBody(r)
	if errResp != nil {
		errResp.Write(w)
		return
	}
	if s, ok := body["stream"]; ok {
		if _, isBool := s.(bool); !isBool {
			writeAPIError(w, http.StatusBadRequest, "stream must be a boolean")
			return
		}
	}
	provider := detectProvider(body, r.Header)
	anth, err := openaiToAnthropic(body)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	anth["stream"] = true // 内部一律流式，按需聚合
	if err := normalizeBody(anth, z); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateMessagesBody(r.Context(), anth); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	clientStream, _ := body["stream"].(bool)
	clientModel, _ := body["model"].(string)
	if clientModel == "" {
		clientModel = "gpt-4o"
	}
	includeUsage := false
	if so, ok := body["stream_options"].(map[string]interface{}); ok {
		includeUsage, _ = so["include_usage"].(bool)
	}
	rc := &relayCtx{
		body: anth, provider: provider, group: r.Header.Get("x-zcode-group"),
		proto: protocolOpenAI, clientStream: clientStream,
		clientModel: clientModel, includeUsage: includeUsage,
	}
	z.relay(w, r, rc)
}

// HandleResponses POST /v1/responses（Codex / 新版 OpenAI SDK）
func (z *ZCodeAPI) HandleResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	body, errResp := readJSONBody(r)
	if errResp != nil {
		errResp.Write(w)
		return
	}
	// 网关无状态：拒绝依赖服务端会话的 previous_response_id（须整段回传 input）
	if prid, _ := body["previous_response_id"].(string); prid != "" {
		writeAPIError(w, http.StatusBadRequest,
			"this gateway is stateless: previous_response_id is not supported; send the full conversation input each time")
		return
	}
	if s, ok := body["stream"]; ok {
		if _, isBool := s.(bool); !isBool {
			writeAPIError(w, http.StatusBadRequest, "stream must be a boolean")
			return
		}
	}
	provider := detectProvider(body, r.Header)
	anth, err := responsesToAnthropic(body)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	anth["stream"] = true
	if err := normalizeBody(anth, z); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateMessagesBody(r.Context(), anth); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	clientStream, _ := body["stream"].(bool)
	clientModel, _ := body["model"].(string)
	if clientModel == "" {
		clientModel = "GLM-5.3"
	}
	rc := &relayCtx{
		body: anth, provider: provider, group: r.Header.Get("x-zcode-group"),
		proto: protocolResponses, clientStream: clientStream, clientModel: clientModel,
	}
	z.relay(w, r, rc)
}

// HandleCompletions POST /v1/completions — legacy text completion（Bifrost 等
// 网关的 Text Completion 请求类型）。shim：prompt 转单条 chat 消息走同一
// relay 管道，响应按 text_completion / text_completion.chunk 形状回写。
func (z *ZCodeAPI) HandleCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	body, errResp := readJSONBody(r)
	if errResp != nil {
		errResp.Write(w)
		return
	}
	if s, ok := body["stream"]; ok {
		if _, isBool := s.(bool); !isBool {
			writeAPIError(w, http.StatusBadRequest, "stream must be a boolean")
			return
		}
	}
	prompt, err := completionsPrompt(body["prompt"])
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	if prompt == "" {
		writeAPIError(w, http.StatusBadRequest, "prompt must not be empty")
		return
	}

	chat := map[string]interface{}{
		"model": body["model"],
		"messages": []interface{}{
			// 补一句系统引导：指令模型收到裸 prompt 时倾向"回答"而非"续写"
			map[string]interface{}{"role": "system", "content": "You are a text completion engine. Continue the user's text seamlessly; output only the continuation, never repeat the prompt and add no commentary."},
			map[string]interface{}{"role": "user", "content": prompt},
		},
	}
	for _, k := range []string{"max_tokens", "temperature", "top_p", "stop", "stream", "stream_options"} {
		if v, ok := body[k]; ok {
			chat[k] = v
		}
	}
	// suffix / n / logprobs / 各类 penalty 上游无法兑现，静默忽略

	provider := detectProvider(chat, r.Header)
	anth, err := openaiToAnthropic(chat)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	anth["stream"] = true // 内部一律流式，按需聚合
	if err := normalizeBody(anth, z); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateMessagesBody(r.Context(), anth); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	clientStream, _ := chat["stream"].(bool)
	clientModel, _ := body["model"].(string)
	if clientModel == "" {
		clientModel = "gpt-3.5-turbo-instruct"
	}
	includeUsage := false
	if so, ok := chat["stream_options"].(map[string]interface{}); ok {
		includeUsage, _ = so["include_usage"].(bool)
	}
	echo, _ := body["echo"].(bool)
	rc := &relayCtx{
		body: anth, provider: provider, group: r.Header.Get("x-zcode-group"),
		proto: protocolCompletions, clientStream: clientStream,
		clientModel: clientModel, includeUsage: includeUsage,
		echo: echo, prompt: prompt,
	}
	z.relay(w, r, rc)
}

// completionsPrompt 提取并归一化 prompt：字符串原样；多元素数组按多采样语义
// 上游无法批量生成，降级为空行拼接的单一 prompt
func completionsPrompt(v interface{}) (string, error) {
	switch p := v.(type) {
	case nil:
		return "", nil
	case string:
		return p, nil
	case []interface{}:
		var parts []string
		for _, item := range p {
			s, ok := item.(string)
			if !ok {
				return "", errString("prompt array elements must be strings")
			}
			parts = append(parts, s)
		}
		return strings.Join(parts, "\n\n"), nil
	default:
		return "", errString("prompt must be a string or array of strings")
	}
}

// asIfaceSlice 将 []interface{} 或 []map[string]interface{} 等切片统一为 []interface{}
func asIfaceSlice(v interface{}) []interface{} {
	switch s := v.(type) {
	case []interface{}:
		return s
	case nil:
		return nil
	default:
		rv := reflect.ValueOf(v)
		if rv.Kind() != reflect.Slice {
			return nil
		}
		out := make([]interface{}, rv.Len())
		for i := 0; i < rv.Len(); i++ {
			out[i] = rv.Index(i).Interface()
		}
		return out
	}
}

// ---- 请求体转换：OpenAI Chat → Anthropic Messages ----

func openaiToAnthropic(body map[string]interface{}) (map[string]interface{}, error) {
	model, _ := body["model"].(string)
	if model == "" {
		model = "GLM-5.3"
	}
	var messages []map[string]interface{}
	var systemParts []string

	rawMsgs, _ := body["messages"].([]interface{})
	for _, m := range rawMsgs {
		msg, ok := m.(map[string]interface{})
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		content := msg["content"]

		if role == "system" || role == "developer" {
			switch c := content.(type) {
			case string:
				systemParts = append(systemParts, c)
			case []interface{}:
				for _, part := range c {
					if pm, ok := part.(map[string]interface{}); ok && pm["type"] == "text" {
						if t, ok := pm["text"].(string); ok {
							systemParts = append(systemParts, t)
						}
					}
				}
			}
			continue
		}
		if role == "tool" {
			toolID, _ := msg["tool_call_id"].(string)
			if toolID == "" {
				continue
			}
			var resultContent interface{}
			switch c := content.(type) {
			case string, []interface{}:
				resultContent = c
			default:
				resultContent = ""
			}
			messages = append(messages, map[string]interface{}{
				"role": "user",
				"content": []map[string]interface{}{{
					"type": "tool_result", "tool_use_id": toolID, "content": resultContent,
				}},
			})
			continue
		}
		if role != "user" && role != "assistant" {
			continue
		}

		var blocks []map[string]interface{}
		switch c := content.(type) {
		case string:
			// 空串 text 块会被 Anthropic schema 拒绝（"at least 1 character"）；
			// 工具调用回合常带 content:""，必须跳过而不是转发整单 400
			if c != "" {
				blocks = append(blocks, map[string]interface{}{"type": "text", "text": c})
			}
		case []interface{}:
			for _, part := range c {
				pm, ok := part.(map[string]interface{})
				if !ok {
					continue
				}
				switch pm["type"] {
				case "text":
					// 与字符串形态同理：空 text 块会被 Anthropic schema 拒绝（"at least 1 character"），
					// 数组形态的 text:""/null 也必须跳过而不是转发整单 400
					t, _ := pm["text"].(string)
					if t != "" {
						blocks = append(blocks, map[string]interface{}{"type": "text", "text": t})
					}
				case "image_url":
					iu, _ := pm["image_url"].(map[string]interface{})
					u, _ := iu["url"].(string)
					if strings.HasPrefix(u, "data:") && strings.Contains(u, ";base64,") {
						idx := strings.Index(u, ";base64,")
						mime := u[5:idx]
						if mime == "" {
							mime = "image/png"
						}
						data := u[idx+8:]
						blocks = append(blocks, map[string]interface{}{
							"type": "image",
							"source": map[string]interface{}{
								"type": "base64", "media_type": mime, "data": data,
							},
						})
					} else {
						// 不支持的图片形态（http(s) URL、字符串形态、null/空 url）一律
						// 显式报错：静默丢弃会让纯图片消息整体消失，模型看到的对话
						// 与客户端发送的不一致
						return nil, errString("image_url must be an object with a data: base64 URL; other image forms are not supported by the upstream")
					}
				default:
					// 未知 part 类型显式报错（与 image_url fail-closed 同理）：
					// 静默跳过 = 纯该类消息整体消失，多轮对话模型看到缺块对话
					pt, _ := pm["type"].(string)
					return nil, errString("unsupported content part type: " + pt + "; the upstream supports only text and image_url parts")
				}
			}
		}
		if role == "assistant" {
			for _, c := range asIfaceSlice(msg["tool_calls"]) {
				cm, ok := c.(map[string]interface{})
				if !ok {
					continue
				}
				fn, _ := cm["function"].(map[string]interface{})
				if fn == nil {
					continue
				}
				name, _ := fn["name"].(string)
				if name == "" {
					continue
				}
				var toolInput map[string]interface{}
				switch args := fn["arguments"].(type) {
				case string:
					if json.Unmarshal([]byte(args), &toolInput) != nil {
						toolInput = map[string]interface{}{"_raw_arguments": args}
					}
				case map[string]interface{}:
					toolInput = args
				default:
					toolInput = map[string]interface{}{}
				}
				if toolInput == nil {
					// 字面量 "null"：Unmarshal 成功但得到 nil，input 必须是对象
					toolInput = map[string]interface{}{}
				}
				id, _ := cm["id"].(string)
				if id == "" {
					id = "call_" + randomHex(8)
				}
				blocks = append(blocks, map[string]interface{}{
					"type": "tool_use", "id": id, "name": name, "input": toolInput,
				})
			}
			// No authenticated caller/conversation scope here: matching public
			// output must not recover another request's private signed thinking.
		}
		if len(blocks) > 0 {
			// 不转发 OpenAI 的 message.name：Anthropic messages schema 只有 role/content，
			// 未知字段会被上游整单拒绝
			messages = append(messages, map[string]interface{}{"role": role, "content": blocks})
		}
	}

	// Anthropic 要求 user/assistant 严格交替：OpenAI 并行工具调用会产生
	// 连续多条 user(tool_result)/assistant(tool_use) 消息，合并之
	messages = mergeSameRoleMessages(messages)
	out := map[string]interface{}{"model": model, "messages": toIfaceSlice(messages)}
	if len(systemParts) > 0 {
		out["system"] = strings.Join(systemParts, "\n\n")
	}
	if mct, ok := body["max_completion_tokens"]; ok && mct != nil {
		// 新字段优先（OpenAI 语义）：请求模板残留的旧 max_tokens 不得覆盖调用方显式设置
		out["max_tokens"] = mct
	} else if mt, ok := body["max_tokens"]; ok && mt != nil {
		out["max_tokens"] = mt
	} else if mct == nil && mt == nil {
		// 显式 null 等同未提供：交给 normalizeBody 补默认值，而不是 400
		out["max_tokens"] = float64(4096)
	}
	if t, ok := body["temperature"]; ok && t != nil {
		out["temperature"] = clampOpenAITemperature(t)
	}
	if tp, ok := body["top_p"]; ok && tp != nil {
		out["top_p"] = tp
	}
	if stop, ok := body["stop"]; ok && stop != nil {
		switch s := stop.(type) {
		case []interface{}:
			out["stop_sequences"] = s
		case string:
			out["stop_sequences"] = []interface{}{s}
		}
	}
	// 推理控制透传：zai-org/ZCode 的 option-map 会给 openai-compat 请求注入
	// thinking / enable_thinking / reasoning.effort / reasoning_effort 多种形状，
	// 全部归一到 Anthropic 侧字段；随后 normalizeBody→fixThinking 按模型再归一化
	// （丢弃会导致 GLM-5.3 恒定以 high 档运行，low/max 请求被静默降级/升级）
	if e, ok := body["reasoning_effort"]; ok && e != nil {
		out["reasoning_effort"] = e
	}
	if re, ok := body["reasoning"].(map[string]interface{}); ok {
		if e, ok := re["effort"].(string); ok && e != "" {
			if _, exists := out["reasoning_effort"]; !exists {
				out["reasoning_effort"] = e
			}
		}
	}
	if th, ok := body["thinking"].(map[string]interface{}); ok {
		out["thinking"] = th
	} else if et, ok := body["enable_thinking"].(bool); ok {
		if et {
			out["thinking"] = map[string]interface{}{"type": "enabled"}
		} else {
			out["thinking"] = map[string]interface{}{"type": "disabled"}
		}
	}

	// tools 转换
	if rawTools, ok := body["tools"].([]interface{}); ok {
		var tools []map[string]interface{}
		for _, t := range rawTools {
			tm, ok := t.(map[string]interface{})
			if !ok {
				continue
			}
			fn := tm
			if tm["type"] == "function" {
				if f, ok := tm["function"].(map[string]interface{}); ok {
					fn = f
				}
			}
			name, _ := fn["name"].(string)
			if name == "" {
				continue
			}
			desc, _ := fn["description"].(string)
			var schema map[string]interface{}
			for _, k := range []string{"parameters", "input_schema", "schema"} {
				if s, ok := fn[k].(map[string]interface{}); ok {
					schema = s
					break
				}
			}
			if schema == nil {
				schema = map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
			}
			tools = append(tools, map[string]interface{}{
				"name": name, "description": desc, "input_schema": schema,
			})
		}
		if len(tools) > 0 {
			out["tools"] = toIfaceSlice(tools)
		}
	}

	// tool_choice 转换；Anthropic 无 "none"：连同 tools 一起从上游请求中省略
	switch tc := body["tool_choice"].(type) {
	case string:
		if tc == "none" {
			delete(out, "tools")
		} else if tc == "auto" {
			out["tool_choice"] = map[string]interface{}{"type": "auto"}
		} else if tc == "required" {
			out["tool_choice"] = map[string]interface{}{"type": "any"}
		}
	case map[string]interface{}:
		// Chat 形态 {"type":"function","function":{"name":...}} 与
		// Responses 扁平形态 {"type":"function","name":...} 都要识别，
		// 否则指定函数调用的 tool_choice 被静默丢弃
		name := ""
		if fn, ok := tc["function"].(map[string]interface{}); ok {
			name, _ = fn["name"].(string)
		}
		if name == "" {
			if tcType, _ := tc["type"].(string); tcType == "function" {
				name, _ = tc["name"].(string)
			}
		}
		if name != "" {
			out["tool_choice"] = map[string]interface{}{"type": "tool", "name": name}
		}
	}
	return out, nil
}

// mergeSameRoleMessages 合并相邻同角色消息（内容块拼接），保证 user/assistant 交替
func mergeSameRoleMessages(msgs []map[string]interface{}) []map[string]interface{} {
	var out []map[string]interface{}
	for _, m := range msgs {
		if n := len(out); n > 0 && out[n-1]["role"] == m["role"] {
			prevBlocks := asBlockList(out[n-1]["content"])
			curBlocks := asBlockList(m["content"])
			if prevBlocks != nil && curBlocks != nil {
				out[n-1]["content"] = append(prevBlocks, curBlocks...)
				continue
			}
		}
		out = append(out, m)
	}
	return out
}

// asBlockList 内容统一为块数组；无法归一化（nil 以外的非块形态）返回 nil 表示不合并
func asBlockList(v interface{}) []map[string]interface{} {
	switch c := v.(type) {
	case []map[string]interface{}:
		return c
	case string:
		return []map[string]interface{}{{"type": "text", "text": c}}
	case nil:
		return []map[string]interface{}{}
	}
	return nil
}

// ---- 请求体转换：OpenAI Responses → Anthropic Messages ----

// clampOpenAITemperature OpenAI 规格允许 [0,2]，Anthropic/Z.ai 上游只收 0..1：
// 超范围值是确定性 400，且会记到健康账号头上（MarkFailed 计失败）。
// 夹紧而非拒绝，保持 OpenAI 客户端兼容
func clampOpenAITemperature(v interface{}) interface{} {
	tv, ok := v.(float64)
	if !ok {
		return v
	}
	if tv > 1 {
		return float64(1)
	}
	if tv < 0 {
		return float64(0)
	}
	return tv
}

// responsesContentToText 提取文本部分；图片 part（input_image）显式报错而非静默丢弃——
// 否则纯图片消息整体消失（"input must contain at least one message"），多轮对话里
// 模型看到的是缺图的对话（与 chat 路径对不支持图片形态的 fail-closed 处理一致）
func responsesContentToText(content interface{}) (string, error) {
	switch c := content.(type) {
	case string:
		return c, nil
	case []interface{}:
		var parts []string
		for _, p := range c {
			pm, ok := p.(map[string]interface{})
			if !ok {
				continue
			}
			if pm["type"] == "input_image" {
				return "", errString("input_image parts are not supported on /v1/responses by the upstream; use /v1/chat/completions with a data: base64 image_url instead")
			}
			for _, k := range []string{"text", "input_text", "output_text"} {
				if s, ok := pm[k].(string); ok && s != "" {
					parts = append(parts, s)
				}
			}
		}
		return strings.Join(parts, "\n"), nil
	}
	return "", nil
}

func responsesToAnthropic(body map[string]interface{}) (map[string]interface{}, error) {
	model, _ := body["model"].(string)
	if model == "" {
		model = "GLM-5.3"
	}
	var messages []map[string]interface{}
	var systemParts []string

	if instructions, ok := body["instructions"].(string); ok && strings.TrimSpace(instructions) != "" {
		systemParts = append(systemParts, strings.TrimSpace(instructions))
	}

	switch input := body["input"].(type) {
	case string:
		messages = append(messages, map[string]interface{}{"role": "user", "content": input})
	case []interface{}:
		for _, item := range input {
			im, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			itemType, _ := im["type"].(string)
			role, _ := im["role"].(string)
			switch {
			case itemType == "message" || role == "user" || role == "assistant" || role == "system" || role == "developer":
				text, err := responsesContentToText(im["content"])
				if err != nil {
					return nil, err
				}
				if text == "" {
					if s, ok := im["content"].(string); ok {
						text = s
					}
				}
				if role == "system" || role == "developer" {
					if text != "" {
						systemParts = append(systemParts, text)
					}
				} else if (role == "user" || role == "assistant") && text != "" {
					messages = append(messages, map[string]interface{}{"role": role, "content": text})
				}
			case itemType == "function_call_output":
				callID := firstNonEmpty(jsonStr(im, "call_id"), jsonStr(im, "tool_call_id"))
				output, err := responsesContentToText(im["output"])
				if err != nil {
					return nil, err
				}
				if callID != "" {
					messages = append(messages, map[string]interface{}{
						"role": "tool", "tool_call_id": callID, "content": output,
					})
				}
			case itemType == "function_call":
				name := firstNonEmpty(jsonStr(im, "name"), "tool")
				callID := firstNonEmpty(jsonStr(im, "call_id"), jsonStr(im, "id"), "call_"+randomHex(8))
				arguments := jsonStr(im, "arguments")
				if arguments == "" {
					arguments = "{}"
				}
				messages = append(messages, map[string]interface{}{
					"role":    "assistant",
					"content": nil,
					"tool_calls": []interface{}{map[string]interface{}{
						"id": callID, "type": "function",
						"function": map[string]interface{}{"name": name, "arguments": arguments},
					}},
				})
			}
		}
	default:
		return nil, errString("input must be a string or array")
	}
	if len(messages) == 0 {
		return nil, errString("input must contain at least one message")
	}

	chatBody := map[string]interface{}{
		"model":    model,
		"messages": toIfaceSlice(messages),
		"stream":   boolOf(body["stream"]),
	}
	if len(systemParts) > 0 {
		sysMsg := map[string]interface{}{"role": "system", "content": strings.Join(systemParts, "\n\n")}
		chatBody["messages"] = append([]interface{}{sysMsg}, chatBody["messages"].([]interface{})...)
	}
	if mot, ok := body["max_output_tokens"]; ok && mot != nil {
		chatBody["max_tokens"] = mot
	} else if mt, ok := body["max_tokens"]; ok && mt != nil {
		chatBody["max_tokens"] = mt
	}
	if t, ok := body["temperature"]; ok && t != nil {
		chatBody["temperature"] = clampOpenAITemperature(t)
	}
	if tp, ok := body["top_p"]; ok && tp != nil {
		chatBody["top_p"] = tp
	}
	if tools, ok := body["tools"]; ok {
		chatBody["tools"] = tools
	}
	if tc, ok := body["tool_choice"]; ok {
		chatBody["tool_choice"] = tc
	}
	if reasoning, ok := body["reasoning"].(map[string]interface{}); ok {
		if effort, ok := reasoning["effort"]; ok && effort != nil {
			chatBody["reasoning_effort"] = effort
		}
	}
	return openaiToAnthropic(chatBody)
}

// ---- 小工具 ----

func toIfaceSlice(in []map[string]interface{}) []interface{} {
	out := make([]interface{}, len(in))
	for i, v := range in {
		out[i] = v
	}
	return out
}

func boolOf(v interface{}) bool {
	b, _ := v.(bool)
	return b
}

type plainError string

func (e plainError) Error() string { return string(e) }

func errString(s string) error { return plainError(s) }
