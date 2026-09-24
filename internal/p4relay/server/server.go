package server

import (
	"bytes"
	"context"
	"crypto/subtle"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"p4relay/internal/p4relay/admin"
	"p4relay/internal/p4relay/config"
	"p4relay/internal/p4relay/crypto"
	apperr "p4relay/internal/p4relay/errors"
	"p4relay/internal/p4relay/journal"
	"p4relay/internal/p4relay/proxy"
	"p4relay/internal/p4relay/web"
)

// Version est la version affichee par /health.
const Version = "1.1.3"

// versionPlaceholder est la graine presentee par index.html. Elle est remplacee
// par Version a chaque reponse : la version affichee par l'interface ne peut
// plus diverger de celle du binaire, et il n'y a plus de second endroit ou la
// mettre a jour. Si la graine survit dans la reponse, c'est que la substitution
// a saute (une page servie ailleurs que par le routeur) : l'erreur est visible.
const versionPlaceholder = "__P4_VERSION__"

type Gateway struct {
	store        *config.Store
	jnl          *journal.Journal
	shutdownMu   sync.Mutex
	shuttingDown bool
	stopUpstream context.CancelFunc
	server       *http.Server
	client       *http.Client
	startedAt    int64
	portOverride *int
	host         string
	port         int
}

func New(dataDir string) (*Gateway, error) {
	// Le répertoire de données n'existe ni dans le dépôt git ni dans un dossier
	// où l'exécutable vient d'être posé : il doit être créé avant la moindre
	// écriture (data/key, data/config.json, data/journal.log).
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("impossible de créer le dossier de données %s : %v", dataDir, err)
	}
	masterKey, err := crypto.LoadMasterKey(dataDir)
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load(dataDir, masterKey)
	if err != nil {
		return nil, err
	}
	jnl := journal.New(filepath.Join(dataDir, "journal.log"))
	g := &Gateway{
		store:     config.NewStore(dataDir, masterKey, cfg),
		jnl:       jnl,
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

// Store expose le store de configuration.
func (g *Gateway) Store() *config.Store { return g.store }

// Journal expose le journal d'activite.
func (g *Gateway) Journal() *journal.Journal { return g.jnl }

// Client expose le client HTTP partage.
func (g *Gateway) Client() *http.Client { return g.client }

// Host renvoie l'adresse d'ecoute reelle.
func (g *Gateway) Host() string { return g.host }

// Port renvoie le port d'ecoute reel.
func (g *Gateway) Port() int { return g.port }

// PortOverride renvoie le port force par la variable PORT, si defini.
func (g *Gateway) PortOverride() *int { return g.portOverride }

// StartedAt renvoie l'horodatage de demarrage (ms).
func (g *Gateway) StartedAt() int64 { return g.startedAt }

// Record delegue une entree au journal.
func (g *Gateway) Record(e journal.Entry) { g.jnl.Record(e) }

// Server renvoie le serveur HTTP (fermeture a distance).
func (g *Gateway) Server() *http.Server { return g.server }

// SetServer enregistre le serveur HTTP.
func (g *Gateway) SetServer(s *http.Server) { g.server = s }

// StopUpstream annule les requetes amont en vol.
func (g *Gateway) StopUpstream() {
	if g.stopUpstream != nil {
		g.stopUpstream()
	}
}

// SetStopUpstream enregistre la fonction d'annulation des requetes amont.
func (g *Gateway) SetStopUpstream(stop context.CancelFunc) { g.stopUpstream = stop }

// MarkShuttingDown bascule le drapeau de fermeture.
func (g *Gateway) MarkShuttingDown() {
	g.shutdownMu.Lock()
	g.shuttingDown = true
	g.shutdownMu.Unlock()
}

func (g *Gateway) Redact(value string) string {
	cfg := g.store.Get()
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

func (g *Gateway) PublicConfig() map[string]any {
	cfg := g.store.Get()
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

func (g *Gateway) ProviderFor(id string) (*config.Provider, error) {
	cfg := g.store.Get()
	for i := range cfg.Providers {
		p := &cfg.Providers[i]
		if p.ID == id {
			if !p.Enabled {
				return nil, apperr.New(400, "Ce fournisseur est désactivé.")
			}
			if p.APIKey == "" && p.Kind != "custom" {
				return nil, apperr.New(400, "Ajoutez la clé API de ce fournisseur dans l’interface.", "provider_not_configured")
			}
			return p, nil
		}
	}
	return nil, apperr.New(404, "Fournisseur introuvable.")
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

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

func (g *Gateway) authorizedLocal(r *http.Request) bool {
	cfg := g.store.Get()
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
func (g *Gateway) Handle(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", cspHeader)

	var failed bool
	var apiErr *apperr.ApiError
	defer func() {
		if p := recover(); p != nil {
			var ok bool
			switch e := p.(type) {
			case *apperr.ApiError:
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
				g.writeError(w, r, apperr.New(500, "Erreur interne.", "internal"))
			}
		} else if failed {
			g.writeError(w, r, apperr.New(500, "Erreur interne.", "internal"))
		}
	}()

	// Host / origin checks.
	localHosts := []string{fmt.Sprintf("127.0.0.1:%d", g.port), fmt.Sprintf("localhost:%d", g.port)}
	if g.port == 80 {
		localHosts = append(localHosts, "127.0.0.1", "localhost")
	}
	addresses := []string{}
	if g.host == "0.0.0.0" {
		addresses = config.LANAddresses()
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
	if !config.Contains(allowedHosts, host) {
		panic(apperr.New(403, "Hôte non autorisé."))
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
			panic(apperr.New(403, "Origine non autorisée."))
		}
	}
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		panic(apperr.New(403, "Accès intersite refusé."))
	}
	g.shutdownMu.Lock()
	shutting := g.shuttingDown
	g.shutdownMu.Unlock()
	if shutting {
		panic(apperr.New(503, "La passerelle est en cours de fermeture."))
	}
	route := r.URL.Path
	localAdmin := config.Loopback(clientIP(r)) && config.Contains(localHosts, host)
	if !localAdmin && route != "/health" && !strings.HasPrefix(route, "/v1") {
		panic(apperr.New(403, "L’interface et l’administration sont réservées à ce PC. Utilisez l’API /v1 avec votre clé locale."))
	}

	if route == "/health" && r.Method == "GET" {
		apperr.WriteJSON(w, 200, map[string]any{"status": "ok", "name": "P4Relay", "version": Version, "protocols": []string{"openai-chat", "anthropic-messages"}})
		return
	}

	if strings.HasPrefix(route, "/api/") {
		admin.Run(g, w, r, route, addresses)
		return
	}

	if route == "/v1" || strings.HasPrefix(route, "/v1") {
		if !g.authorizedLocal(r) {
			panic(apperr.New(401, "Clé locale absente ou invalide. Copiez-la depuis l’interface.", "invalid_api_key"))
		}
		if route == "/v1/models" && r.Method == "GET" {
			cfg := g.store.Get()
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
			apperr.WriteJSON(w, 200, map[string]any{"object": "list", "data": data})
			return
		}
		if route == "/v1/chat/completions" && r.Method == "POST" {
			proxy.Run(g, w, r, "openai", false)
			return
		}
		if route == "/v1/messages" && r.Method == "POST" {
			proxy.Run(g, w, r, "anthropic", false)
			return
		}
		if route == "/v1/messages/count_tokens" && r.Method == "POST" {
			proxy.Run(g, w, r, "anthropic", true)
			return
		}
		panic(apperr.New(404, "Routes disponibles : GET /v1/models, POST /v1/chat/completions, POST /v1/messages et POST /v1/messages/count_tokens.", "unsupported_endpoint"))
	}

	if r.Method == "GET" {
		if sf, ok := staticFiles[route]; ok {
			data, err := web.FS.ReadFile("public/" + sf[0])
			if err != nil {
				panic(apperr.New(404, "Page introuvable.", "not_found"))
			}
			if sf[0] == "index.html" {
				data = bytes.ReplaceAll(data, []byte(versionPlaceholder), []byte(Version))
			}
			w.Header().Set("Content-Type", sf[1]+"; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			w.WriteHeader(200)
			_, _ = w.Write(data)
			return
		}
	}
	panic(apperr.New(404, "Page introuvable.", "not_found"))
}

func (g *Gateway) writeError(w http.ResponseWriter, r *http.Request, apiErr *apperr.ApiError) {
	if apiErr == nil {
		return
	}
	isMessages := regexp.MustCompile(`^/v1/messages(?:/count_tokens)?`).MatchString(r.URL.Path)
	body := any(apperr.OpenAI(apiErr.Status, g.Redact(apiErr.Message), apiErr.Code))
	if isMessages {
		body = apperr.Anthropic(apiErr.Status, g.Redact(apiErr.Message))
	}
	apperr.WriteJSON(w, apiErr.Status, body)
}
