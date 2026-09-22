package main

import (
	"bufio"
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed public
var publicFS embed.FS

var (
	staticFiles = map[string][2]string{
		"/":            {"index.html", "text/html"},
		"/app.js":      {"app.js", "text/javascript"},
		"/style.css":   {"style.css", "text/css"},
		"/logo.png":    {"logo.png", "image/png"},
		"/icon.png":    {"icon.png", "image/png"},
		"/favicon.svg": {"favicon.svg", "image/svg+xml"},
		"/fournisseurs/openrouter.png": {"fournisseurs/openrouter.png", "image/png"},
		"/fournisseurs/openai.png":    {"fournisseurs/openai.png", "image/png"},
		"/fournisseurs/anthropic.png": {"fournisseurs/anthropic.png", "image/png"},
		"/fournisseurs/custom.png":    {"fournisseurs/custom.png", "image/png"},
	}
	cspHeader = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'"
)

const (
	version         = "1.1.0"
	timeoutMs       = 180 * time.Second
	journalPageSize = 100
)

type logEntry struct {
	ID         string `json:"id"`
	At         string `json:"at"`
	Alias      string `json:"alias"`
	Provider   string `json:"provider"`
	Target     string `json:"target"`
	Status     int    `json:"status"`
	Stream     bool   `json:"stream"`
	Endpoint   string `json:"endpoint"`
	DurationMs int64  `json:"durationMs"`
}

type gateway struct {
	store        *store
	dataDir      string
	logsMu       sync.Mutex
	logs         []logEntry
	logTotal     int
	statsMu      sync.Mutex
	requests     int
	successful   int
	failed       int
	totalMs      int64
	startedAt    int64
	shutdownMu   sync.Mutex
	shuttingDown bool
	stopUpstream context.CancelFunc
	server       *http.Server
	client       *http.Client
	portOverride *int
	host         string
	port         int
}

func newGateway(dataDir string) (*gateway, error) {
	masterKey, err := loadMasterKey(dataDir)
	if err != nil {
		return nil, err
	}
	cfg, err := loadConfig(dataDir, masterKey)
	if err != nil {
		return nil, err
	}
	store := &store{file: filepath.Join(dataDir, "config.json"), config: cfg, key: masterKey}
	g := &gateway{
		store:     store,
		dataDir:   dataDir,
		logs:      []logEntry{},
		startedAt: time.Now().UnixMilli(),
		client:    &http.Client{CheckRedirect: func(r *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }},
	}
	if p := os.Getenv("PORT"); p != "" {
		var n int
		if _, err := fmt.Sscanf(p, "%d", &n); err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("PORT doit être un entier entre 1 et 65535.")
		}
		g.portOverride = &n
	}
	if cfg.Network.LanEnabled {
		g.host = "0.0.0.0"
	} else {
		g.host = "127.0.0.1"
	}
	if g.portOverride != nil {
		g.port = *g.portOverride
	} else {
		g.port = cfg.Network.Port
	}
	return g, nil
}

func (g *gateway) journalPath() string {
	return filepath.Join(g.dataDir, "journal.log")
}

func (g *gateway) loadJournal() {
	data, err := os.ReadFile(g.journalPath())
	if err != nil || len(data) == 0 {
		return
	}
	if len(data) >= 3 && data[0] == 0xEF && data[1] == 0xBB && data[2] == 0xBF {
		data = data[3:]
	}
	var entries []logEntry
	if json.Unmarshal(data, &entries) != nil {
		return
	}
	total := len(entries)
	g.logsMu.Lock()
	g.logs = entries
	g.logTotal = total
	g.logsMu.Unlock()
	log.Printf("journal: %d entrées rechargées depuis %s", total, g.journalPath())
}

func (g *gateway) saveJournal() {
	g.logsMu.Lock()
	entries := make([]logEntry, len(g.logs))
	copy(entries, g.logs)
	total := g.logTotal
	g.logsMu.Unlock()
	data, err := json.Marshal(entries)
	if err != nil {
		return
	}
	if err := os.WriteFile(g.journalPath(), data, 0o644); err != nil {
		log.Printf("journal: %v", err)
	}
	log.Printf("journal: %d entrées conservées dans %s", total, g.journalPath())
}

func (g *gateway) record(entry logEntry) {
	g.logsMu.Lock()
	g.logs = append([]logEntry{entry}, g.logs...)
	g.logTotal++
	g.logsMu.Unlock()
	g.saveJournal()
	g.statsMu.Lock()
	g.requests++
	g.totalMs += entry.DurationMs
	if entry.Status >= 200 && entry.Status < 300 {
		g.successful++
	} else {
		g.failed++
	}
	g.statsMu.Unlock()
}

func (g *gateway) clearLogs() {
	g.logsMu.Lock()
	g.logs = []logEntry{}
	g.logTotal = 0
	g.logsMu.Unlock()
	os.Remove(g.journalPath())
}

func (g *gateway) redact(value string) string {
	cfg := g.store.get()
	text := value
	keys := []string{cfg.LocalToken}
	for _, p := range cfg.Providers {
		if p.APIKey != "" {
			keys = append(keys, p.APIKey)
		}
	}
	for _, key := range keys {
		if key == "" {
			continue
		}
		text = strings.ReplaceAll(text, key, "[clé masquée]")
	}
	return text
}

func (g *gateway) publicConfig() map[string]any {
	cfg := g.store.get()
	providers := make([]map[string]any, 0, len(cfg.Providers))
	for _, p := range cfg.Providers {
		providers = append(providers, map[string]any{
			"id":          p.ID,
			"name":        p.Name,
			"baseUrl":     p.BaseURL,
			"kind":        p.Kind,
			"workspaceId": p.WorkspaceID,
			"enabled":     p.Enabled,
			"hasKey":      p.APIKey != "",
		})
	}
	aliases := make([]map[string]any, 0, len(cfg.Aliases))
	for _, a := range cfg.Aliases {
		aliases = append(aliases, map[string]any{
			"id":          a.ID,
			"name":        a.Name,
			"providerId":  a.ProviderID,
			"targetModel": a.TargetModel,
			"enabled":     a.Enabled,
		})
	}
	return map[string]any{
		"version":    cfg.Version,
		"localToken": cfg.LocalToken,
		"providers":  providers,
		"aliases":    aliases,
		"network":    map[string]any{"lanEnabled": cfg.Network.LanEnabled, "port": cfg.Network.Port},
	}
}

func (g *gateway) providerFor(id string) (*Provider, error) {
	cfg := g.store.get()
	for i := range cfg.Providers {
		p := &cfg.Providers[i]
		if p.ID == id {
			if !p.Enabled {
				return nil, apiError(400, "Ce fournisseur est désactivé.")
			}
			if p.APIKey == "" && p.Kind != "custom" {
				return nil, apiError(400, "Ajoutez la clé API de ce fournisseur dans l’interface.", "provider_not_configured")
			}
			return p, nil
		}
	}
	return nil, apiError(404, "Fournisseur introuvable.")
}

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

func (g *gateway) authorizedLocal(r *http.Request) bool {
	cfg := g.store.get()
	token := cfg.LocalToken
	match := func(value string) bool {
		return len(value) == len(token) && subtle.ConstantTimeCompare([]byte(value), []byte(token)) == 1
	}
	auth := r.Header.Get("Authorization")
	if len(auth) == len("Bearer "+token) && subtle.ConstantTimeCompare([]byte(auth), []byte("Bearer "+token)) == 1 {
		return true
	}
	return match(r.Header.Get("X-Api-Key"))
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
		flusher, ok := w.(http.Flusher)
		if !ok {
			return apiError(500, "Streaming non disponible.")
		}
		_ = flusher
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

// handle is the single entry point for every request.
func (g *gateway) handle(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", cspHeader)

	var failed bool
	var apiErr *ApiError
	defer func() {
		if p := recover(); p != nil {
			var ok bool
			switch e := p.(type) {
			case *ApiError:
				apiErr = e
				ok = true
			case error:
				failed = true
			default:
				failed = true
			}
			if ok {
				g.writeError(w, r, apiErr)
			} else {
				g.writeError(w, r, apiError(500, "Erreur interne.", "internal"))
			}
		} else if failed {
			g.writeError(w, r, apiError(500, "Erreur interne.", "internal"))
		}
	}()

	// Host / origin checks.
	localHosts := []string{fmt.Sprintf("127.0.0.1:%d", g.port), fmt.Sprintf("localhost:%d", g.port)}
	if g.port == 80 {
		localHosts = append(localHosts, "127.0.0.1", "localhost")
	}
	addresses := []string{}
	if g.host == "0.0.0.0" {
		addresses = lanAddresses()
	}
	allowedHosts := append([]string{}, localHosts...)
	for _, ip := range addresses {
		if g.port == 80 {
			allowedHosts = append(allowedHosts, ip)
		} else {
			allowedHosts = append(allowedHosts, fmt.Sprintf("%s:%d", ip, g.port))
		}
	}
	host := r.Host
	if !contains(allowedHosts, host) {
		panic(apiError(403, "Hôte non autorisé."))
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		okOrigin := false
		for _, h := range allowedHosts {
			if origin == "http://"+h {
				okOrigin = true
				break
			}
		}
		if !okOrigin {
			panic(apiError(403, "Origine non autorisée."))
		}
	}
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		panic(apiError(403, "Accès intersite refusé."))
	}
	g.shutdownMu.Lock()
	shutting := g.shuttingDown
	g.shutdownMu.Unlock()
	if shutting {
		panic(apiError(503, "La passerelle est en cours de fermeture."))
	}
	route := r.URL.Path
	localAdmin := loopback(clientIP(r)) && contains(localHosts, host)
	if !localAdmin && route != "/health" && !strings.HasPrefix(route, "/v1") {
		panic(apiError(403, "L’interface et l’administration sont réservées à ce PC. Utilisez l’API /v1 avec votre clé locale."))
	}

	if route == "/health" && r.Method == "GET" {
		writeJSON(w, 200, map[string]any{"status": "ok", "name": "P4Relay", "version": version, "protocols": []string{"openai-chat", "anthropic-messages"}})
		return
	}

	if strings.HasPrefix(route, "/api/") {
		g.handleAdmin(w, r, route, addresses)
		return
	}

	if route == "/v1" || strings.HasPrefix(route, "/v1") {
		if !g.authorizedLocal(r) {
			panic(apiError(401, "Clé locale absente ou invalide. Copiez-la depuis l’interface.", "invalid_api_key"))
		}
		if route == "/v1/models" && r.Method == "GET" {
			cfg := g.store.get()
			data := []map[string]any{}
			for _, a := range cfg.Aliases {
				if !a.Enabled {
					continue
				}
				found := false
				for _, p := range cfg.Providers {
					if p.ID == a.ProviderID && p.Enabled {
						found = true
						break
					}
				}
				if found {
					data = append(data, map[string]any{"id": a.Name, "object": "model", "created": g.startedAt / 1000, "owned_by": "p4-local"})
				}
			}
			writeJSON(w, 200, map[string]any{"object": "list", "data": data})
			return
		}
		if route == "/v1/chat/completions" && r.Method == "POST" {
			g.proxy(w, r, "openai", false)
			return
		}
		if route == "/v1/messages" && r.Method == "POST" {
			g.proxy(w, r, "anthropic", false)
			return
		}
		if route == "/v1/messages/count_tokens" && r.Method == "POST" {
			g.proxy(w, r, "anthropic", true)
			return
		}
		panic(apiError(404, "Routes disponibles : GET /v1/models, POST /v1/chat/completions, POST /v1/messages et POST /v1/messages/count_tokens.", "unsupported_endpoint"))
	}

	if r.Method == "GET" {
		if sf, ok := staticFiles[route]; ok {
			data, err := publicFS.ReadFile("public/" + sf[0])
			if err != nil {
				panic(apiError(404, "Page introuvable.", "not_found"))
			}
			w.Header().Set("Content-Type", sf[1]+"; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			w.WriteHeader(200)
			_, _ = w.Write(data)
			return
		}
	}
	panic(apiError(404, "Page introuvable.", "not_found"))
}

func (g *gateway) writeError(w http.ResponseWriter, r *http.Request, apiErr *ApiError) {
	if apiErr == nil {
		return
	}
	isMessages := regexp.MustCompile(`^/v1/messages(?:/count_tokens)?`).MatchString(r.URL.Path)
	body := any(openaiError(apiErr.Status, g.redact(apiErr.Message), apiErr.Code))
	if isMessages {
		body = anthropicError(apiErr.Status, g.redact(apiErr.Message))
	}
	writeJSON(w, apiErr.Status, body)
}

func (g *gateway) handleAdmin(w http.ResponseWriter, r *http.Request, route string, addresses []string) {
	if r.Header.Get("X-P4-Local") != "1" {
		panic(apiError(403, "Ouvrez l’interface locale pour administrer la passerelle."))
	}
	switch {
	case route == "/api/shutdown" && r.Method == "POST":
		if _, err := readJSONBody(r); err != nil {
			panic(err)
		}
		g.shutdownMu.Lock()
		g.shuttingDown = true
		g.shutdownMu.Unlock()
		writeJSON(w, 200, map[string]any{"ok": true})
		go g.finishShutdown()
	case route == "/api/state" && r.Method == "GET":
		cfg := g.store.get()
		g.logsMu.Lock()
		start := 0
		if len(g.logs) > journalPageSize {
			start = len(g.logs) - journalPageSize
		}
		logsCopy := make([]logEntry, len(g.logs)-start)
		copy(logsCopy, g.logs[start:])
		g.logsMu.Unlock()
		g.statsMu.Lock()
		stats := map[string]any{"requests": g.requests, "successful": g.successful, "failed": g.failed, "totalMs": g.totalMs}
		g.statsMu.Unlock()
		lanEnabled := g.host == "0.0.0.0"
		lanUrls := []string{}
		for _, ip := range addresses {
			lanUrls = append(lanUrls, fmt.Sprintf("http://%s:%d/v1", ip, g.port))
		}
		configuredPort := cfg.Network.Port
		if g.portOverride != nil {
			configuredPort = *g.portOverride
		}
		state := g.publicConfig()
		state["baseUrl"] = fmt.Sprintf("http://127.0.0.1:%d/v1", g.port)
		state["startedAt"] = g.startedAt
		state["stats"] = stats
		state["logs"] = logsCopy
		state["networkStatus"] = map[string]any{
			"host":            g.host,
			"port":            g.port,
			"lanEnabled":      lanEnabled,
			"portOverride":    g.portOverride,
			"lanUrls":         lanUrls,
			"restartRequired": lanEnabled != cfg.Network.LanEnabled || g.port != configuredPort,
			"nextLocalUrl":    fmt.Sprintf("http://127.0.0.1:%d", configuredPort),
		}
		writeJSON(w, 200, state)
	case route == "/api/network" && r.Method == "POST":
		input, err := readJSONBody(r)
		if err != nil {
			panic(err)
		}
		nw, err := networkSettings(input)
		if err != nil {
			panic(err)
		}
		if _, err := g.store.mutate(func(next *Config) (any, error) {
			next.Network = nw
			return nil, nil
		}); err != nil {
			panic(err)
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	case route == "/api/logs" && r.Method == "GET":
		page := 1
		if p, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && p > 0 {
			page = p
		}
		g.logsMu.Lock()
		total := g.logTotal
		start := (page - 1) * journalPageSize
		var slice []logEntry
		if start < len(g.logs) {
			slice = g.logs[start:]
		}
		g.logsMu.Unlock()
		writeJSON(w, 200, map[string]any{"logs": slice, "total": total, "page": page, "pageSize": journalPageSize})
	case route == "/api/logs" && r.Method == "DELETE":
		g.clearLogs()
		writeJSON(w, 200, map[string]any{"ok": true})
	case route == "/api/providers" && r.Method == "POST":
		input, err := readJSONBody(r)
		if err != nil {
			panic(err)
		}
		result, err := g.store.mutate(func(next *Config) (any, error) {
			var existing *Provider
			if id, ok := input["id"].(string); ok {
				for i := range next.Providers {
					if next.Providers[i].ID == id {
						existing = &next.Providers[i]
						break
					}
				}
				if existing == nil {
					return nil, apiError(404, "Fournisseur introuvable.")
				}
			}
			kind, _ := input["kind"].(string)
			if kind == "" {
				kind = "custom"
			}
			if kind != "openrouter" && kind != "openai" && kind != "anthropic" && kind != "custom" {
				return nil, apiError(400, "Type de fournisseur invalide.")
			}
			apiKey := ""
			if existing != nil {
				apiKey = existing.APIKey
			}
			if clear, _ := input["clearKey"].(bool); clear {
				apiKey = ""
			} else if s, ok := input["apiKey"].(string); ok && trimSpace(s) != "" {
				v, err := required(s, "Clé API", 4096)
				if err != nil {
					return nil, err
				}
				apiKey = v
			}
			name, err := required(input["name"], "Nom", 128)
			if err != nil {
				return nil, err
			}
			base, err := validateBaseURL(input["baseUrl"])
			if err != nil {
				return nil, err
			}
			workspaceID := ""
			if ws, present := input["workspaceId"]; present && ws != nil {
				if s, ok := ws.(string); ok {
					if trimSpace(s) == "" {
						// Empty workspace is fine (optional field).
					} else {
						workspaceID, err = required(s, "Workspace", 256)
						if err != nil {
							return nil, err
						}
					}
				} else {
					return nil, apiError(400, "Workspace invalide.")
				}
			}
			enabled := true
			if e, ok := input["enabled"].(bool); ok {
				enabled = e
			}
			p := Provider{
				ID:          randomID(),
				Name:        name,
				BaseURL:     base,
				Kind:        kind,
				APIKey:      apiKey,
				WorkspaceID: workspaceID,
				Enabled:     enabled,
			}
			if existing != nil {
				p.ID = existing.ID
			}
			u, _ := url.Parse(p.BaseURL)
			if u != nil && portOf(u) == g.port && contains(append([]string{"localhost", "127.0.0.1"}, addresses...), u.Hostname()) {
				return nil, apiError(400, "Le fournisseur ne peut pas pointer vers cette passerelle.")
			}
			if existing != nil {
				for i := range next.Providers {
					if next.Providers[i].ID == existing.ID {
						next.Providers[i] = p
						break
					}
				}
			} else {
				next.Providers = append(next.Providers, p)
			}
			return map[string]any{"id": p.ID}, nil
		})
		if err != nil {
			panic(err)
		}
		writeJSON(w, 200, result)
	case routeMatches(r, `^/api/providers/([^/]+)/models$`, r.Method == "GET"):
		m := regexp.MustCompile(`^/api/providers/([^/]+)/models$`).FindStringSubmatch(route)
		id := m[1]
		p, err := g.providerFor(id)
		if err != nil {
			panic(err)
		}
		allModels := []map[string]any{}
		after := ""
		for page := 0; page < 20; page++ {
			suffix := ""
			if p.Kind == "anthropic" {
				suffix = "?limit=100"
				if after != "" {
					suffix += "&after_id=" + url.QueryEscape(after)
				}
			}
			reqCtx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
			req, rerr := http.NewRequestWithContext(reqCtx, "GET", p.BaseURL+"/models"+suffix, nil)
			if rerr != nil {
				cancel()
				panic(apiError(502, "Impossible de joindre le fournisseur. Vérifiez son URL et votre connexion.", "upstream_connection_error"))
			}
			for k, v := range headersFor(p, true) {
				req.Header.Set(k, v)
			}
			upstream, derr := g.client.Do(req)
			if derr != nil {
				cancel()
				panic(apiError(502, "Impossible de joindre le fournisseur. Vérifiez son URL et votre connexion.", "upstream_connection_error"))
			}
			if upstream.StatusCode < 200 || upstream.StatusCode > 299 {
				err := g.upstreamError(upstream)
				upstream.Body.Close()
				cancel()
				panic(err)
			}
			var result map[string]any
			jerr := json.NewDecoder(upstream.Body).Decode(&result)
			upstream.Body.Close()
			cancel()
			if jerr != nil {
				panic(apiError(502, "Liste de modèles incompatible.", "upstream_error"))
			}
			data, ok := asArray(result["data"])
			if !ok {
				panic(apiError(502, "Liste de modèles incompatible.", "upstream_error"))
			}
			for _, mi := range data {
				mObj, _ := mi.(map[string]any)
				if mObj == nil {
					continue
				}
				mid, ok := mObj["id"].(string)
				if !ok {
					continue
				}
				mname := mid
				if n, ok := mObj["name"].(string); ok && n != "" {
					mname = n
				} else if n, ok := mObj["display_name"].(string); ok && n != "" {
					mname = n
				}
				allModels = append(allModels, map[string]any{"id": mid, "name": mname})
			}
			if p.Kind != "anthropic" {
				break
			}
			hasMore, _ := result["has_more"].(bool)
			lastID, _ := result["last_id"].(string)
			if !hasMore || lastID == "" || lastID == after {
				break
			}
			after = lastID
		}
		writeJSON(w, 200, map[string]any{"data": allModels})
	case routeMatches(r, `^/api/providers/([^/]+)$`, r.Method == "DELETE"):
		m := regexp.MustCompile(`^/api/providers/([^/]+)$`).FindStringSubmatch(route)
		id := m[1]
		_, err := g.store.mutate(func(next *Config) (any, error) {
			for _, a := range next.Aliases {
				if a.ProviderID == id {
					return nil, apiError(409, "Supprimez ou réaffectez d’abord les alias de ce fournisseur.")
				}
			}
			found := false
			for _, p := range next.Providers {
				if p.ID == id {
					found = true
					break
				}
			}
			if !found {
				return nil, apiError(404, "Fournisseur introuvable.")
			}
			kept := next.Providers[:0]
			for _, p := range next.Providers {
				if p.ID != id {
					kept = append(kept, p)
				}
			}
			next.Providers = kept
			return nil, nil
		})
		if err != nil {
			panic(err)
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	case route == "/api/aliases" && r.Method == "POST":
		input, err := readJSONBody(r)
		if err != nil {
			panic(err)
		}
		result, err := g.store.mutate(func(next *Config) (any, error) {
			var existing *Alias
			if id, ok := input["id"].(string); ok {
				for i := range next.Aliases {
					if next.Aliases[i].ID == id {
						existing = &next.Aliases[i]
						break
					}
				}
				if existing == nil {
					return nil, apiError(404, "Alias introuvable.")
				}
			}
			name, err := required(input["name"], "Alias", 64)
			if err != nil {
				return nil, err
			}
			existingID := ""
			if existing != nil {
				existingID = existing.ID
			}
			for _, a := range next.Aliases {
				if a.Name == name && a.ID != existingID {
					return nil, apiError(409, "Ce nom de modèle local existe déjà.")
				}
			}
			providerID, _ := input["providerId"].(string)
			found := false
			for _, p := range next.Providers {
				if p.ID == providerID {
					found = true
					break
				}
			}
			if !found {
				return nil, apiError(400, "Choisissez un fournisseur.")
			}
			targetModel, err := required(input["targetModel"], "Modèle réel", 256)
			if err != nil {
				return nil, err
			}
			enabled := true
			if e, ok := input["enabled"].(bool); ok {
				enabled = e
			}
			a := Alias{ID: randomID(), Name: name, ProviderID: providerID, TargetModel: targetModel, Enabled: enabled}
			if existing != nil {
				a.ID = existing.ID
			}
			if existing != nil {
				for i := range next.Aliases {
					if next.Aliases[i].ID == existing.ID {
						next.Aliases[i] = a
						break
					}
				}
			} else {
				next.Aliases = append(next.Aliases, a)
			}
			return map[string]any{"id": a.ID}, nil
		})
		if err != nil {
			panic(err)
		}
		writeJSON(w, 200, result)
	case routeMatches(r, `^/api/aliases/([^/]+)$`, r.Method == "DELETE"):
		m := regexp.MustCompile(`^/api/aliases/([^/]+)$`).FindStringSubmatch(route)
		id := m[1]
		_, err := g.store.mutate(func(next *Config) (any, error) {
			found := false
			for _, a := range next.Aliases {
				if a.ID == id {
					found = true
					break
				}
			}
			if !found {
				return nil, apiError(404, "Alias introuvable.")
			}
			kept := next.Aliases[:0]
			for _, a := range next.Aliases {
				if a.ID != id {
					kept = append(kept, a)
				}
			}
			next.Aliases = kept
			return nil, nil
		})
		if err != nil {
			panic(err)
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	case route == "/api/token/rotate" && r.Method == "POST":
		if _, err := readJSONBody(r); err != nil {
			panic(err)
		}
		if _, err := g.store.mutate(func(next *Config) (any, error) {
			next.LocalToken = randomToken()
			return nil, nil
		}); err != nil {
			panic(err)
		}
		cfg := g.store.get()
		writeJSON(w, 200, map[string]any{"localToken": cfg.LocalToken})
	default:
		panic(apiError(404, "Route d’administration introuvable."))
	}
}

func routeMatches(r *http.Request, pattern string, methodOK bool) bool {
	if !methodOK {
		return false
	}
	return regexp.MustCompile(pattern).MatchString(r.URL.Path)
}

func (g *gateway) finishShutdown() {
	// Give the response time to flush, then stop accepting upstream work.
	time.Sleep(200 * time.Millisecond)
	if g.stopUpstream != nil {
		g.stopUpstream()
	}
	if g.server != nil {
		_ = g.server.Close()
	}
	// Exit so the port is released and the gateway can be started again.
	os.Exit(0)
}

func contains(list []string, s string) bool {
	for _, e := range list {
		if e == s {
			return true
		}
	}
	return false
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func main() {
	dataDir := os.Getenv("P4_DATA_DIR")
	if dataDir == "" {
		exe, _ := os.Executable()
		dataDir = filepath.Join(filepath.Dir(exe), "data")
	}
	g, err := newGateway(dataDir)
	if err != nil {
		log.Fatal(err)
	}
	g.loadJournal()
	ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", g.host, g.port))
	if err != nil {
		if strings.Contains(err.Error(), "address already in use") {
			log.Fatalf("Le port %d est déjà utilisé. Ouvrez http://127.0.0.1:%d ou choisissez un autre PORT.", g.port, g.port)
		}
		log.Fatal(err)
	}
	server := &http.Server{
		ReadHeaderTimeout: 10 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			g.handle(w, r)
		}),
	}
	g.server = server
	ctx, stop := context.WithCancel(context.Background())
	g.stopUpstream = stop
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		server.Close()
		stop()
	}()
	fmt.Printf("P4Relay\nInterface : http://127.0.0.1:%d\nAPI       : http://127.0.0.1:%d/v1\nÉcoute    : %s:%d\nCtrl+C pour arrêter.\n", g.port, g.port, g.host, g.port)
	if len(os.Args) > 1 && os.Args[1] == "--open-browser" {
		cmd := exec.Command("explorer.exe", fmt.Sprintf("http://127.0.0.1:%d", g.port))
		_ = cmd.Start()
	}
	if err := server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	_ = ctx
}
