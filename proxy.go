package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

func headersFor(p *Provider, models bool) map[string]string {
	headers := map[string]string{"Content-Type": "application/json"}
	if p.APIKey != "" {
		headers["Authorization"] = "Bearer " + p.APIKey
	}
	if p.Kind == "openrouter" {
		headers["X-Title"] = "P4Relay"
	}
	if p.Kind == "anthropic" {
		if models {
			delete(headers, "Authorization")
			headers["x-api-key"] = p.APIKey
			headers["anthropic-version"] = "2023-06-01"
		}
		if p.WorkspaceID != "" {
			headers["anthropic-workspace-id"] = p.WorkspaceID
		}
	}
	return headers
}

func (g *gateway) upstreamError(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var message string
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err == nil {
		if e, ok := data["error"].(map[string]any); ok {
			if m, ok := e["message"].(string); ok {
				message = m
			}
		}
		if message == "" {
			if m, ok := data["message"].(string); ok {
				message = m
			}
		}
	}
	status := resp.StatusCode
	if status < 400 || status > 599 {
		status = 502
	}
	if len(message) == 0 {
		message = fmt.Sprintf("erreur HTTP %d", resp.StatusCode)
	} else if len(message) > 1500 {
		message = message[:1500]
	}
	return apiError(status, g.redact("Fournisseur : "+message), "upstream_error")
}

// lineScanner scans SSE lines with a 16 MiB buffer.
type lineScanner struct {
	sc *bufio.Scanner
}

func newLineScanner(body io.Reader) *lineScanner {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64*1024), sseLineLimit+1)
	return &lineScanner{sc: sc}
}

func (s *lineScanner) Scan() bool { return s.sc.Scan() }
func (s *lineScanner) Text() string {
	return s.sc.Text()
}

// aliasStream mirrors aliasStream(stream, model): rewrites the model field.
func aliasStream(body io.Reader, model string, out *sseWriter) error {
	scanner := newLineScanner(body)
	doneSeen := false
	rewrite := func(line string) string {
		if !strings.HasPrefix(line, "data:") {
			return line
		}
		data := strings.TrimSpace(line[5:])
		if data == "" {
			return line
		}
		if data == "[DONE]" {
			doneSeen = true
			return line
		}
		var chunk any
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			panic(apiError(502, "Événement JSON invalide dans le flux du fournisseur.", "upstream_error"))
		}
		obj, _ := chunk.(map[string]any)
		if obj != nil {
			if e, present := obj["error"]; present && e != nil {
				msg := "Erreur dans le flux du fournisseur."
				if eObj, ok := e.(map[string]any); ok {
					if m, ok := eObj["message"].(string); ok {
						msg = m
					}
				}
				panic(apiError(502, msg, "upstream_error"))
			}
			if _, present := obj["model"]; present {
				obj["model"] = model
			}
		}
		return "data: " + mustJSON(chunk)
	}
	defer func() {
		if p := recover(); p != nil {
			if apiErr, ok := p.(*ApiError); ok {
				out.err = apiErr
			}
		}
	}()
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		out.write([]byte(rewrite(line) + "\n"))
	}
	if !doneSeen {
		return apiError(502, "Le flux du fournisseur s’est interrompu avant sa fin.", "upstream_error")
	}
	return nil
}

func (g *gateway) proxy(w http.ResponseWriter, r *http.Request, protocol string, countOnly bool) {
	started := time.Now()
	var aliasName, providerName, targetModel string
	status := 500
	streaming := false

	ctx, cancel := context.WithTimeout(r.Context(), timeoutMs)
	defer cancel()

	var proxyErr error
	defer func() {
		var apiErr *ApiError
		if errors.As(proxyErr, &apiErr) {
			status = apiErr.Status
		} else if proxyErr == nil && status >= 400 {
			// already handled (e.g. streaming error)
		} else if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			status = 504
		} else if errors.Is(r.Context().Err(), context.Canceled) {
			status = 499
		} else if proxyErr != nil {
			status = 502
		}
		g.record(logEntry{
			ID:         randomID(),
			At:         time.Now().UTC().Format(time.RFC3339Nano),
			Alias:      orDash(aliasName),
			Provider:   orDash(providerName),
			Target:     orDash(targetModel),
			Status:     status,
			Stream:     streaming,
			Endpoint:   endpointName(protocol, countOnly),
			DurationMs: time.Since(started).Milliseconds(),
		})
	}()

	input, err := readJSONBody(r)
	if err != nil {
		proxyErr = err
		return
	}
	if s, ok := input["stream"].(bool); ok {
		streaming = s
	}
	var name string
	if name, err = required(input["model"], "Nom du modèle", 64); err != nil {
		proxyErr = err
		return
	}
	msgs, ok := asArray(input["messages"])
	if !ok || len(msgs) == 0 {
		proxyErr = apiError(400, "messages doit être une liste non vide de messages avec un rôle.")
		return
	}
	for _, m := range msgs {
		obj, ok := m.(map[string]any)
		if !ok {
			proxyErr = apiError(400, "messages doit être une liste non vide de messages avec un rôle.")
			return
		}
		if _, ok := obj["role"].(string); !ok {
			proxyErr = apiError(400, "messages doit être une liste non vide de messages avec un rôle.")
			return
		}
	}
	if v, present := input["stream"]; present {
		if _, ok := v.(bool); !ok {
			proxyErr = apiError(400, "stream doit être un booléen.")
			return
		}
	}
	if protocol == "anthropic" {
		if err = validateMessages(input, countOnly); err != nil {
			proxyErr = err
			return
		}
	}

	cfg := g.store.get()
	found := false
	for i := range cfg.Aliases {
		a := &cfg.Aliases[i]
		if a.Name == name && a.Enabled {
			found = true
			aliasName = a.Name
			targetModel = a.TargetModel
			provider, perr := g.providerFor(a.ProviderID)
			if perr != nil {
				proxyErr = perr
				return
			}
			providerName = provider.Name
			if _, present := input["models"]; present {
				proxyErr = apiError(400, "Les champs models et route ne sont pas acceptés : utilisez un alias local.")
				return
			}
			if _, present := input["route"]; present {
				proxyErr = apiError(400, "Les champs models et route ne sont pas acceptés : utilisez un alias local.")
				return
			}
			status = 200
			proxyErr = g.doProxy(w, r, ctx, provider, aliasName, targetModel, protocol, countOnly, streaming, input)
			return
		}
	}
	if !found {
		proxyErr = apiError(404, fmt.Sprintf("Alias inconnu ou désactivé : %s. Ajoutez-le dans l’interface.", name), "model_not_found")
	}
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func endpointName(protocol string, countOnly bool) string {
	if countOnly {
		return "count_tokens"
	}
	if protocol == "anthropic" {
		return "messages"
	}
	return "chat/completions"
}

// doProxy performs the upstream call and writes the response.
func (g *gateway) doProxy(w http.ResponseWriter, r *http.Request, ctx context.Context, provider *Provider, aliasName, targetModel, protocol string, countOnly, streaming bool, input map[string]any) error {
	native := protocol == "anthropic" && provider.Kind == "anthropic"
	var outgoing map[string]any
	var err error
	if protocol == "anthropic" && !native {
		outgoing, err = toChatRequest(input, targetModel, provider.Kind)
		if err != nil {
			return err
		}
	} else {
		outgoing = make(map[string]any, len(input)+1)
		for k, v := range input {
			outgoing[k] = v
		}
		outgoing["model"] = targetModel
	}
	if countOnly && !native {
		w.Header().Set("X-P4-Token-Count", "estimated")
		writeJSON(w, 200, map[string]any{"input_tokens": estimateInputTokens(outgoing)})
		return nil
	}
	headers := headersFor(provider, native)
	if native {
		headers["anthropic-version"] = r.Header.Get("anthropic-version")
		if headers["anthropic-version"] == "" {
			headers["anthropic-version"] = "2023-06-01"
		}
		if beta := r.Header.Get("anthropic-beta"); beta != "" {
			headers["anthropic-beta"] = beta
		}
	}
	endpoint := "/chat/completions"
	if native {
		endpoint = "/messages"
		if countOnly {
			endpoint = "/messages/count_tokens"
		}
	}
	target := provider.BaseURL + endpoint
	if native {
		if r.URL.Query().Get("beta") == "true" {
			target += "?beta=true"
		}
	}
	body, _ := json.Marshal(outgoing)
	req, err := http.NewRequestWithContext(ctx, "POST", target, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	upstream, err := g.client.Do(req)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return apiError(504, "Le fournisseur a dépassé le délai de réponse (180 s par défaut).", "upstream_timeout")
		}
		if errors.Is(r.Context().Err(), context.Canceled) {
			return apiError(499, "Requête annulée.", "upstream_timeout")
		}
		return apiError(502, "Impossible de joindre le fournisseur. Vérifiez son URL et votre connexion.", "upstream_connection_error")
	}
	defer upstream.Body.Close()
	if upstream.StatusCode < 200 || upstream.StatusCode >= 300 {
		if ra := upstream.Header.Get("Retry-After"); ra != "" {
			w.Header().Set("Retry-After", ra)
		}
		return g.upstreamError(upstream)
	}
	if countOnly {
		var result map[string]any
		if err := json.NewDecoder(upstream.Body).Decode(&result); err != nil {
			return apiError(502, "Comptage Anthropic invalide.", "upstream_error")
		}
		n, ok := asInt(result["input_tokens"])
		if !ok || n < 0 {
			return apiError(502, "Comptage Anthropic invalide.", "upstream_error")
		}
		writeJSON(w, 200, map[string]any{"input_tokens": n})
		return nil
	}
	if streaming {
		ct := upstream.Header.Get("Content-Type")
		if !strings.Contains(ct, "text/event-stream") {
			return apiError(502, "Le fournisseur n’a pas renvoyé de flux SSE.", "upstream_error")
		}
		if _, ok := w.(http.Flusher); !ok {
			return apiError(500, "Streaming non disponible.")
		}
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache, no-transform")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(200)
		out := &sseWriter{w: w}
		var streamErr error
		switch {
		case protocol == "anthropic" && native:
			streamErr = nativeMessageStream(upstream.Body, aliasName, out)
		case protocol == "anthropic":
			streamErr = chatToMessageStream(upstream.Body, aliasName, out)
		default:
			streamErr = aliasStream(upstream.Body, aliasName, out)
		}
		if streamErr != nil {
			// Headers already sent: emit an error event and close.
			st := 502
			var apiErr *ApiError
			if errors.As(streamErr, &apiErr) {
				st = apiErr.Status
			}
			msg := g.redact(apiErrorMessage(streamErr))
			if protocol == "anthropic" {
				out.write([]byte(sseEncode(anthropicError(st, msg))))
			} else {
				out.write([]byte("data: " + mustJSON(map[string]any{"error": map[string]any{"message": msg, "type": "upstream_error"}}) + "\n\n"))
			}
		}
		return streamErr
	}
	// Non-streaming.
	raw, err := io.ReadAll(io.LimitReader(upstream.Body, maxBody+1))
	if err != nil {
		return apiError(502, "Réponse JSON invalide du fournisseur.", "upstream_error")
	}
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		return apiError(502, "Réponse JSON invalide du fournisseur.", "upstream_error")
	}
	if native {
		if result["type"] != "message" {
			return apiError(502, "Réponse incompatible avec Anthropic Messages.", "upstream_error")
		}
		if _, ok := asArray(result["content"]); !ok {
			return apiError(502, "Réponse incompatible avec Anthropic Messages.", "upstream_error")
		}
		result["model"] = aliasName
		writeJSON(w, 200, result)
		return nil
	}
	if !isPlainObject(result) {
		return apiError(502, "Réponse incompatible avec Chat Completions.", "upstream_error")
	}
	if _, ok := asArray(result["choices"]); !ok {
		return apiError(502, "Réponse incompatible avec Chat Completions.", "upstream_error")
	}
	if protocol == "anthropic" {
		converted, err := fromChatResponse(result, aliasName)
		if err != nil {
			return err
		}
		writeJSON(w, 200, converted)
		return nil
	}
	result["model"] = aliasName
	writeJSON(w, 200, result)
	return nil
}

func apiErrorMessage(err error) string {
	var apiErr *ApiError
	if errors.As(err, &apiErr) {
		return apiErr.Message
	}
	return "Connexion au fournisseur interrompue."
}
