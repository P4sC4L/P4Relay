package main

import (
	"crypto/subtle"
	"embed"
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

//go:embed public
var publicFS embed.FS

var (
	staticFiles = map[string][2]string{
		"/":                            {"index.html", "text/html"},
		"/app.js":                      {"app.js", "text/javascript"},
		"/style.css":                   {"style.css", "text/css"},
		"/logo.png":                    {"logo.png", "image/png"},
		"/icon.png":                    {"icon.png", "image/png"},
		"/fournisseurs/openrouter.png": {"fournisseurs/openrouter.png", "image/png"},
		"/fournisseurs/openai.png":     {"fournisseurs/openai.png", "image/png"},
		"/fournisseurs/anthropic.png":  {"fournisseurs/anthropic.png", "image/png"},
		"/fournisseurs/custom.png":     {"fournisseurs/custom.png", "image/png"},
	}
	cspHeader = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'"
)

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
