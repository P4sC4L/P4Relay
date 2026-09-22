package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
)

var (
	imageMediaTypeRe = regexp.MustCompile(`^image/(jpeg|png|gif|webp)$`)
	httpURLRe        = regexp.MustCompile(`^https?://`)
)

func invalid(message string) error {
	return apiError(400, message)
}

func upstreamInvalid(message string) error {
	return apiError(502, message, "upstream_error")
}

func textOf(value any, name string) (string, error) {
	s, ok := value.(string)
	if !ok {
		return "", invalid(fmt.Sprintf("%s doit être du texte.", name))
	}
	return s, nil
}

// blocks mirrors blocks(content)
func blocks(content any) ([]any, error) {
	switch v := content.(type) {
	case string:
		return []any{map[string]any{"type": "text", "text": v}}, nil
	case []any:
		return v, nil
	}
	return nil, invalid("content doit être du texte ou une liste de blocs Anthropic.")
}

// validateMessages mirrors validateMessages(input, countOnly)
func validateMessages(input map[string]any, countOnly bool) error {
	msgs, ok := asArray(input["messages"])
	if !ok || len(msgs) == 0 {
		return invalid("messages doit être une liste non vide.")
	}
	for _, m := range msgs {
		obj, ok := m.(map[string]any)
		if !ok {
			return invalid("Les messages Anthropic doivent avoir le rôle user, assistant ou system (bêta).")
		}
		role, _ := obj["role"].(string)
		if role != "user" && role != "assistant" && role != "system" {
			return invalid("Les messages Anthropic doivent avoir le rôle user, assistant ou system (bêta).")
		}
		b, err := blocks(obj["content"])
		if err != nil {
			return err
		}
		for _, block := range b {
			obj, ok := block.(map[string]any)
			if !ok {
				return invalid("Bloc de contenu Anthropic invalide.")
			}
			if _, ok := obj["type"].(string); !ok {
				return invalid("Bloc de contenu Anthropic invalide.")
			}
		}
	}
	if !countOnly {
		mt, ok := asInt(input["max_tokens"])
		if !ok || mt < 0 {
			return invalid("max_tokens doit être un entier positif ou nul.")
		}
	}
	if v, present := input["stream"]; present {
		if _, ok := v.(bool); !ok {
			return invalid("stream doit être un booléen.")
		}
	}
	return nil
}

func imagePart(block map[string]any) (map[string]any, error) {
	source, _ := block["source"].(map[string]any)
	if source == nil {
		return nil, invalid("Source d’image non prise en charge : utilisez base64 ou une URL HTTP(S).")
	}
	switch source["type"] {
	case "base64":
		data, ok := source["data"].(string)
		if !ok {
			return nil, invalid("Source d’image non prise en charge : utilisez base64 ou une URL HTTP(S).")
		}
		media, _ := source["media_type"].(string)
		if imageMediaTypeRe.MatchString(media) {
			return map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:" + media + ";base64," + data}}, nil
		}
	case "url":
		u, ok := source["url"].(string)
		if ok && httpURLRe.MatchString(u) {
			return map[string]any{"type": "image_url", "image_url": map[string]any{"url": u}}, nil
		}
	}
	return nil, invalid("Source d’image non prise en charge : utilisez base64 ou une URL HTTP(S).")
}

func contentParts(items []any) ([]any, error) {
	var out []any
	for _, item := range items {
		b, ok := item.(map[string]any)
		if !ok {
			return nil, invalid("Bloc de contenu Anthropic invalide.")
		}
		switch b["type"] {
		case "text":
			t, err := textOf(b["text"], "text")
			if err != nil {
				return nil, err
			}
			out = append(out, map[string]any{"type": "text", "text": t})
		case "image":
			part, err := imagePart(b)
			if err != nil {
				return nil, err
			}
			out = append(out, part)
		case "document":
			source, _ := b["source"].(map[string]any)
			if source == nil || source["type"] != "text" {
				return nil, invalid("Bloc document non pris en charge pour ce fournisseur. Les fonctions natives avancées nécessitent un fournisseur Anthropic.")
			}
			data, err := textOf(source["data"], "document.source.data")
			if err != nil {
				return nil, err
			}
			var parts []string
			if title, ok := b["title"].(string); ok && title != "" {
				parts = append(parts, title)
			}
			parts = append(parts, data)
			out = append(out, map[string]any{"type": "text", "text": strings.Join(parts, "\n")})
		default:
			typ, _ := b["type"].(string)
			return nil, invalid(fmt.Sprintf("Bloc %s non pris en charge pour ce fournisseur. Les fonctions natives avancées nécessitent un fournisseur Anthropic.", typ))
		}
	}
	return out, nil
}

func compact(parts []any) any {
	allText := true
	var texts []string
	for _, p := range parts {
		obj, _ := p.(map[string]any)
		if obj == nil || obj["type"] != "text" {
			allText = false
			break
		}
		texts = append(texts, obj["text"].(string))
	}
	if allText {
		return strings.Join(texts, "\n")
	}
	return parts
}

// toChatRequest mirrors toChatRequest(input, targetModel, kind)
func toChatRequest(input map[string]any, targetModel, kind string) (map[string]any, error) {
	messages := []any{}
	if sys, present := input["system"]; present {
		system, err := blocks(sys)
		if err != nil {
			return nil, err
		}
		for _, b := range system {
			obj, _ := b.(map[string]any)
			if obj == nil || obj["type"] != "text" {
				return nil, invalid("Le prompt système doit contenir uniquement du texte.")
			}
		}
		var texts []string
		for _, b := range system {
			obj := b.(map[string]any)
			t, err := textOf(obj["text"], "system.text")
			if err != nil {
				return nil, err
			}
			texts = append(texts, t)
		}
		messages = append(messages, map[string]any{"role": "system", "content": strings.Join(texts, "\n")})
	}

	msgs, _ := asArray(input["messages"])
	for _, m := range msgs {
		obj := m.(map[string]any)
		content, err := blocks(obj["content"])
		if err != nil {
			return nil, err
		}
		role, _ := obj["role"].(string)
		switch role {
		case "system":
			var texts []string
			for _, b := range content {
				bobj, _ := b.(map[string]any)
				if bobj == nil || bobj["type"] != "text" {
					return nil, invalid("Un message système doit contenir uniquement du texte.")
				}
				t, err := textOf(bobj["text"], "system.text")
				if err != nil {
					return nil, err
				}
				texts = append(texts, t)
			}
			messages = append(messages, map[string]any{"role": "system", "content": strings.Join(texts, "\n")})
		case "assistant":
			var calls []any
			var kept []any
			for _, b := range content {
				bobj, _ := b.(map[string]any)
				if bobj == nil {
					kept = append(kept, b)
					continue
				}
				switch bobj["type"] {
				case "tool_use":
					id, _ := bobj["id"].(string)
					name, _ := bobj["name"].(string)
					if id == "" || name == "" || !isPlainObject(bobj["input"]) {
						return nil, invalid("Bloc tool_use invalide : id, name et input objet requis.")
					}
					calls = append(calls, map[string]any{
						"id":     id,
						"type":   "function",
						"function": map[string]any{
							"name":      name,
							"arguments": mustJSON(bobj["input"]),
						},
					})
				case "thinking", "redacted_thinking":
					// skipped: native thinking signatures cannot be replayed
				default:
					kept = append(kept, b)
				}
			}
			parts, err := contentParts(kept)
			if err != nil {
				return nil, err
			}
			for _, p := range parts {
				obj, _ := p.(map[string]any)
				if obj == nil || obj["type"] != "text" {
					return nil, invalid("Images dans un message assistant non prises en charge par Chat Completions.")
				}
			}
			msg := map[string]any{"role": "assistant", "content": compact(parts)}
			if msg["content"] == "" {
				msg["content"] = nil
			}
			if len(calls) > 0 {
				msg["tool_calls"] = calls
			}
			messages = append(messages, msg)
		default: // user
			var images []any
			var userParts []any
			for _, b := range content {
				bobj, _ := b.(map[string]any)
				if bobj == nil || bobj["type"] != "tool_result" {
					userParts = append(userParts, b)
					continue
				}
				toolUseID, _ := bobj["tool_use_id"].(string)
				if toolUseID == "" {
					return nil, invalid("tool_result.tool_use_id est requis.")
				}
				parts, err := contentParts(mustBlocks(bobj["content"]))
				if err != nil {
					return nil, err
				}
				var toolImages []any
				var texts []string
				for _, p := range parts {
				 pObj, _ := p.(map[string]any)
					if pObj == nil {
						continue
					}
					if pObj["type"] == "image_url" {
						toolImages = append(toolImages, p)
					} else if t, ok := pObj["text"].(string); ok {
						texts = append(texts, t)
					}
				}
				if len(toolImages) > 0 {
					images = append(images, map[string]any{"type": "text", "text": "Images du résultat de l’outil " + toolUseID + " :"})
					images = append(images, toolImages...)
				}
				var errPrefix string
				if isErr, _ := bobj["is_error"].(bool); isErr {
					errPrefix = "Erreur de l’outil :\n"
				}
				messages = append(messages, map[string]any{
					"role":         "tool",
					"tool_call_id": toolUseID,
					"content":      errPrefix + strings.Join(texts, "\n"),
				})
			}
			parts, err := contentParts(userParts)
			if err != nil {
				return nil, err
			}
			combined := append(images, parts...)
			if len(combined) > 0 {
				messages = append(messages, map[string]any{"role": "user", "content": compact(combined)})
			}
		}
	}

	stream, _ := input["stream"].(bool)
	outgoing := map[string]any{"model": targetModel, "messages": messages, "stream": stream}
	if mt, present := input["max_tokens"]; present {
		n, _ := asInt(mt)
		if n == 0 {
			return nil, invalid("Le préremplissage de cache (max_tokens=0) nécessite un fournisseur Anthropic.")
		}
		if kind == "openai" {
			outgoing["max_completion_tokens"] = n
		} else {
			outgoing["max_tokens"] = n
		}
	}
	for _, field := range []string{"temperature", "top_p"} {
		if v, present := input[field]; present {
			outgoing[field] = v
		}
	}
	if stop, present := input["stop_sequences"]; present {
		if arr, ok := asArray(stop); ok && len(arr) > 0 {
			outgoing["stop"] = arr
		}
	}
	if tools, present := input["tools"]; present {
		arr, ok := asArray(tools)
		if !ok {
			return nil, invalid("tools doit être une liste.")
		}
		var out []any
		for _, t := range arr {
			tool, ok := t.(map[string]any)
			if !ok {
				return nil, invalid("Seuls les outils clients avec name et input_schema sont traduisibles. Les outils serveur Anthropic nécessitent un fournisseur Anthropic.")
			}
			if typ, present := tool["type"]; present && typ != "custom" {
				return nil, invalid("Seuls les outils clients avec name et input_schema sont traduisibles. Les outils serveur Anthropic nécessitent un fournisseur Anthropic.")
			}
			name, ok := tool["name"].(string)
			if !ok || name == "" || !isPlainObject(tool["input_schema"]) {
				return nil, invalid("Seuls les outils clients avec name et input_schema sont traduisibles. Les outils serveur Anthropic nécessitent un fournisseur Anthropic.")
			}
			fn := map[string]any{"name": name, "parameters": tool["input_schema"]}
			if desc, ok := tool["description"].(string); ok {
				fn["description"] = desc
			}
			out = append(out, map[string]any{"type": "function", "function": fn})
		}
		if len(out) > 0 {
			outgoing["tools"] = out
		}
	}
	if choice, present := input["tool_choice"]; present {
		c, _ := choice.(map[string]any)
		if c == nil {
			return nil, invalid("tool_choice Anthropic invalide.")
		}
		typ, _ := c["type"].(string)
		switch {
		case typ == "tool":
			name, ok := c["name"].(string)
			if !ok || name == "" {
				return nil, invalid("tool_choice Anthropic invalide.")
			}
			outgoing["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": name}}
		case typ == "auto", typ == "none", typ == "any":
			if typ == "any" {
				outgoing["tool_choice"] = "required"
			} else {
				outgoing["tool_choice"] = typ
			}
		default:
			return nil, invalid("tool_choice Anthropic invalide.")
		}
		if v, present := c["disable_parallel_tool_use"]; present {
			if b, ok := v.(bool); ok {
				outgoing["parallel_tool_calls"] = !b
			}
		}
	}
	var format any
	if oc, present := input["output_config"]; present {
		if ocObj, ok := oc.(map[string]any); ok {
			format = ocObj["format"]
		}
	}
	if format == nil {
		format = input["output_format"]
	}
	if format != nil {
		f, _ := format.(map[string]any)
		if f == nil || f["type"] != "json_schema" || !isPlainObject(f["schema"]) {
			return nil, invalid("Format de sortie non pris en charge.")
		}
		outgoing["response_format"] = map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name":   "response",
				"strict": true,
				"schema": f["schema"],
			},
		}
	}
	if cm, present := input["context_management"]; present {
		cmObj, _ := cm.(map[string]any)
		edits, _ := asArray(cmObj["edits"])
		for _, e := range edits {
			eObj, _ := e.(map[string]any)
			typ, _ := eObj["type"].(string)
			if typ != "clear_thinking_20251015" && typ != "clear_tool_uses_20250919" {
				return nil, invalid("Cette opération context_management nécessite un fournisseur Anthropic natif.")
			}
		}
	}
	if _, present := input["container"]; present {
		return nil, invalid("Cette fonction Anthropic native ne peut pas être traduite pour ce fournisseur.")
	}
	if _, present := input["mcp_servers"]; present {
		return nil, invalid("Cette fonction Anthropic native ne peut pas être traduite pour ce fournisseur.")
	}
	if stream {
		outgoing["stream_options"] = map[string]any{"include_usage": true}
	}
	return outgoing, nil
}

func mustBlocks(content any) []any {
	b, err := blocks(content)
	if err != nil {
		// content ?? '' — a tool_result with invalid content falls back to empty text
		return []any{map[string]any{"type": "text", "text": ""}}
	}
	return b
}

func mustJSON(v any) string {
	data, _ := json.Marshal(v)
	return string(data)
}

// usageFromChat mirrors usageFromChat(usage)
func usageFromChat(usage map[string]any) map[string]any {
	cached := int64(0)
	if details, ok := usage["prompt_tokens_details"].(map[string]any); ok {
		if c, ok := asInt(details["cached_tokens"]); ok {
			cached = c
		}
	}
	prompt := int64(0)
	if p, ok := asInt(usage["prompt_tokens"]); ok {
		prompt = p
	}
	completion := int64(0)
	if c, ok := asInt(usage["completion_tokens"]); ok {
		completion = c
	}
	input := prompt - cached
	if input < 0 {
		input = 0
	}
	return map[string]any{
		"input_tokens":                 input,
		"output_tokens":                completion,
		"cache_creation_input_tokens":  0,
		"cache_read_input_tokens":      cached,
	}
}

func stopReason(reason string, hasTools bool) string {
	switch {
	case reason == "length":
		return "max_tokens"
	case reason == "content_filter":
		return "refusal"
	case hasTools || reason == "tool_calls":
		return "tool_use"
	}
	return "end_turn"
}

// toolBlock mirrors toolBlock(call)
func toolBlock(call map[string]any) (map[string]any, error) {
	id, ok := call["id"].(string)
	if !ok || id == "" {
		return nil, upstreamInvalid("Appel d’outil du fournisseur incomplet.")
	}
	fn, _ := call["function"].(map[string]any)
	if fn == nil {
		return nil, upstreamInvalid("Appel d’outil du fournisseur incomplet.")
	}
	name, ok := fn["name"].(string)
	if !ok || name == "" {
		return nil, upstreamInvalid("Appel d’outil du fournisseur incomplet.")
	}
	args, _ := fn["arguments"].(string)
	var input any
	if err := json.Unmarshal([]byte(args), &input); err != nil {
		return nil, upstreamInvalid("Arguments JSON invalides dans l’appel d’outil du fournisseur.")
	}
	if !isPlainObject(input) {
		return nil, upstreamInvalid("Les arguments d’un outil doivent être un objet JSON.")
	}
	return map[string]any{"type": "tool_use", "id": id, "name": name, "input": input}, nil
}

// fromChatResponse mirrors fromChatResponse(result, model)
func fromChatResponse(result map[string]any, model string) (map[string]any, error) {
	choices, _ := asArray(result["choices"])
	if len(choices) == 0 {
		return nil, upstreamInvalid("Réponse Chat Completions sans message.")
	}
	choice, _ := choices[0].(map[string]any)
	if choice == nil {
		return nil, upstreamInvalid("Réponse Chat Completions sans message.")
	}
	message, _ := choice["message"].(map[string]any)
	if message == nil {
		return nil, upstreamInvalid("Réponse Chat Completions sans message.")
	}
	content := []any{}
	if c, ok := message["content"].(string); ok && c != "" {
		content = append(content, map[string]any{"type": "text", "text": c})
	}
	if r, ok := message["refusal"].(string); ok && r != "" {
		content = append(content, map[string]any{"type": "text", "text": r})
	}
	var calls []any
	if tc, ok := asArray(message["tool_calls"]); ok {
		for _, t := range tc {
			tObj, _ := t.(map[string]any)
			block, err := toolBlock(tObj)
			if err != nil {
				return nil, err
			}
			calls = append(calls, block)
		}
	}
	content = append(content, calls...)
	usage := map[string]any{}
	if u, ok := result["usage"].(map[string]any); ok {
		usage = u
	}
	finish, _ := choice["finish_reason"].(string)
	return map[string]any{
		"id":            "msg_" + randomID(),
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   stopReason(finish, len(calls) > 0),
		"stop_sequence": nil,
		"usage":         usageFromChat(usage),
	}, nil
}

func sseEncode(value map[string]any) string {
	data, _ := json.Marshal(value)
	return fmt.Sprintf("event: %s\ndata: %s\n\n", value["type"], string(data))
}

// readSSE parses SSE from a stream at event boundaries (mirrors readSSE).
// It yields the joined data lines of each event.
type sseReader struct {
	scanner *bufio.Scanner
	buf     strings.Builder
	data    []string
	size    int
}

const sseLineLimit = 16 * 1024 * 1024

// nextEvent returns the next joined data payload, or io.EOF at end of stream.
func (r *sseReader) nextEvent() (string, error) {
	for {
		if !r.scanner.Scan() {
			if err := r.scanner.Err(); err != nil {
				return "", err
			}
			// flush trailing line without newline
			if r.buf.Len() > 0 {
				line := strings.TrimSuffix(r.buf.String(), "\r")
				r.buf.Reset()
				if joined := r.line(line); joined != nil {
					return *joined, nil
				}
			}
			if len(r.data) > 0 {
				joined := strings.Join(r.data, "\n")
				return joined, nil
			}
			return "", io.EOF
		}
		line := strings.TrimSuffix(r.scanner.Text(), "\r")
		if joined := r.line(line); joined != nil {
			return *joined, nil
		}
	}
}

// line mirrors line(value): returns joined data on blank line.
func (r *sseReader) line(value string) *string {
	if value == "" {
		var joined *string
		if len(r.data) > 0 {
			j := strings.Join(r.data, "\n")
			joined = &j
		}
		r.data = nil
		r.size = 0
		return joined
	}
	if strings.HasPrefix(value, "data:") {
		part := value[5:]
		if strings.HasPrefix(part, " ") {
			part = part[1:]
		}
		r.size += len(part)
		if r.size > sseLineLimit {
			panic(apiError(502, "Événement SSE trop volumineux.", "upstream_error"))
		}
		r.data = append(r.data, part)
	}
	return nil
}

func newSSEReader(body io.Reader) *sseReader {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), sseLineLimit+1)
	return &sseReader{scanner: scanner}
}

func parseEvent(raw string) (map[string]any, error) {
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return nil, upstreamInvalid("JSON invalide dans le flux du fournisseur.")
	}
	obj, ok := value.(map[string]any)
	if !ok {
		return nil, upstreamInvalid("Événement SSE invalide.")
	}
	if e, present := obj["error"]; present && e != nil {
		msg := "Erreur du fournisseur pendant le streaming."
		if eObj, ok := e.(map[string]any); ok {
			if m, ok := eObj["message"].(string); ok && m != "" {
				msg = m
			}
		}
		return nil, upstreamInvalid(msg)
	}
	return obj, nil
}

// writeSSE writes a chunk to the response, flushing immediately.
type sseWriter struct {
	w   http.ResponseWriter
	err error
}

func (w *sseWriter) write(data []byte) {
	if w.err != nil {
		return
	}
	_, w.err = w.w.Write(data)
	if w.err == nil {
		w.w.(interface{ Flush() }).Flush()
	}
}

// nativeMessageStream mirrors nativeMessageStream(stream, model).
func nativeMessageStream(body io.Reader, model string, out *sseWriter) error {
	reader := newSSEReader(body)
	started := false
	stopped := false
	for {
		raw, err := reader.nextEvent()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		event, perr := parseEvent(raw)
		if perr != nil {
			return perr
		}
		if event["type"] == "message_start" {
			msg, ok := event["message"].(map[string]any)
			if !ok {
				return upstreamInvalid("message_start invalide.")
			}
			msg["model"] = model
			started = true
		}
		if event["type"] == "message_stop" {
			stopped = true
		}
		out.write([]byte(sseEncode(event)))
	}
	if !started || !stopped {
		return upstreamInvalid("Le flux Anthropic s’est interrompu avant sa fin.")
	}
	return nil
}

type toolCallAcc struct {
	id     string
	name   string
	args   string
	pieces []string
}

// chatToMessageStream mirrors chatToMessageStream(stream, model).
func chatToMessageStream(body io.Reader, model string, out *sseWriter) error {
	reader := newSSEReader(body)
	started := false
	done := false
	finish := ""
	usage := map[string]any{}
	textIndex := -1
	nextIndex := 0
	calls := map[int]*toolCallAcc{}
	callOrder := []int{}
	argumentSize := 0

	for {
		raw, err := reader.nextEvent()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if raw == "[DONE]" {
			done = true
			break
		}
		chunk, perr := parseEvent(raw)
		if perr != nil {
			return perr
		}
		if u, ok := chunk["usage"].(map[string]any); ok {
			usage = u
		}
		if !started {
			out.write([]byte(sseEncode(map[string]any{
				"type": "message_start",
				"message": map[string]any{
					"id":            "msg_" + randomID(),
					"type":          "message",
					"role":          "assistant",
					"model":         model,
					"content":       []any{},
					"stop_reason":   nil,
					"stop_sequence": nil,
					"usage":         usageFromChat(usage),
				},
			})))
			started = true
		}
		choices, _ := asArray(chunk["choices"])
		var choice map[string]any
		for _, c := range choices {
			cObj, _ := c.(map[string]any)
			if cObj == nil {
				continue
			}
			if idx, ok := cObj["index"]; ok {
				if n, ok := asInt(idx); ok && n == 0 {
					choice = cObj
					break
				}
			}
			if choice == nil {
				choice = cObj
			}
		}
		if choice == nil {
			continue
		}
		delta, _ := choice["delta"].(map[string]any)
		if delta == nil {
			delta = map[string]any{}
		}
		var value any
		if v, ok := delta["content"]; ok {
			value = v
		}
		if v, ok := delta["refusal"]; ok {
			value = v
		}
		if s, ok := value.(string); ok && s != "" {
			if textIndex == -1 {
				textIndex = nextIndex
				nextIndex++
				out.write([]byte(sseEncode(map[string]any{
					"type":          "content_block_start",
					"index":         textIndex,
					"content_block": map[string]any{"type": "text", "text": ""},
				})))
			}
			out.write([]byte(sseEncode(map[string]any{
				"type":  "content_block_delta",
				"index": textIndex,
				"delta": map[string]any{"type": "text_delta", "text": s},
			})))
		}
		if tc, ok := asArray(delta["tool_calls"]); ok {
			for _, piece := range tc {
				p, _ := piece.(map[string]any)
				idx, ok := asInt(p["index"])
				if !ok || idx < 0 {
					return upstreamInvalid("Index d’appel d’outil invalide.")
				}
				i := int(idx)
				call := calls[i]
				if call == nil {
					call = &toolCallAcc{}
					calls[i] = call
					callOrder = append(callOrder, i)
				}
				if pid, ok := p["id"].(string); ok && pid != "" && pid != call.id {
					call.id += pid
				}
				if fn, ok := p["function"].(map[string]any); ok {
					if name, ok := fn["name"].(string); ok && name != "" && name != call.name {
						call.name += name
					}
					if args, ok := fn["arguments"].(string); ok && len(args) > 0 {
						call.args += args
						call.pieces = append(call.pieces, args)
						argumentSize += len(args)
					}
				}
				if argumentSize > sseLineLimit || len(calls) > 1024 {
					return upstreamInvalid("Appels d’outils trop volumineux.")
				}
			}
		}
		if fr, ok := choice["finish_reason"].(string); ok && fr != "" {
			finish = fr
		}
	}
	if !started || !done || finish == "" {
		return upstreamInvalid("Le flux Chat Completions s’est interrompu avant sa fin.")
	}
	if textIndex != -1 {
		out.write([]byte(sseEncode(map[string]any{"type": "content_block_stop", "index": textIndex})))
	}
	// Buffer interleaved tool calls to emit complete, sequential Anthropic blocks.
	for _, i := range callOrder {
		call := calls[i]
		block, err := toolBlock(map[string]any{
			"id":       call.id,
			"type":     "function",
			"function": map[string]any{"name": call.name, "arguments": call.args},
		})
		if err != nil {
			return err
		}
		index := nextIndex
		nextIndex++
		start := map[string]any{
			"type":          "content_block_start",
			"index":         index,
			"content_block": map[string]any{"type": "tool_use", "id": block["id"], "name": block["name"], "input": map[string]any{}},
		}
		out.write([]byte(sseEncode(start)))
		if len(call.pieces) == 0 {
			call.pieces = []string{"{}"}
		}
		for _, pj := range call.pieces {
			out.write([]byte(sseEncode(map[string]any{
				"type":  "content_block_delta",
				"index": index,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": pj},
			})))
		}
		out.write([]byte(sseEncode(map[string]any{"type": "content_block_stop", "index": index})))
	}
	out.write([]byte(sseEncode(map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason(finish, len(calls) > 0), "stop_sequence": nil},
		"usage": usageFromChat(usage),
	})))
	out.write([]byte(sseEncode(map[string]any{"type": "message_stop"})))
	return nil
}

// estimateInputTokens mirrors estimateInputTokens(request).
func estimateInputTokens(request map[string]any) int {
	images := 0
	replacer := func(v any) any {
		if obj, ok := v.(map[string]any); ok {
			if obj["type"] == "image_url" {
				images++
				return map[string]any{"type": "image"}
			}
		}
		return v
	}
	payload := map[string]any{"messages": request["messages"], "tools": request["tools"]}
	data, _ := marshalWithReplacer(payload, replacer)
	if len(data) == 0 {
		return 1
	}
	est := (len(data) + 2) / 3 + images*1600
	if est < 1 {
		est = 1
	}
	return est
}

// marshalWithReplacer marshals while replacing image_url nodes (mirrors the JS replacer).
func marshalWithReplacer(v any, replacer func(any) any) ([]byte, error) {
	switch t := v.(type) {
	case map[string]any:
		if t["type"] == "image_url" {
			return json.Marshal(replacer(v))
		}
		m := make(map[string]any, len(t))
		for k, val := range t {
			m[k] = val
		}
		buf, err := json.Marshal(marshalReplaced(m, replacer))
		return buf, err
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = e
		}
		return json.Marshal(marshalReplaced(out, replacer))
	default:
		return json.Marshal(v)
	}
}

func marshalReplaced(v any, replacer func(any) any) any {
	switch t := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, val := range t {
			m[k] = marshalReplaced(val, replacer)
		}
		if m["type"] == "image_url" {
			return replacer(m)
		}
		return m
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = marshalReplaced(e, replacer)
		}
		return out
	default:
		return v
	}
}

var _ = bytes.MinRead
