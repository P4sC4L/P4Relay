package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"time"
)

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
