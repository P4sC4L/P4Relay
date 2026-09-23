package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	version         = "1.1.1"
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
	_, stop := context.WithCancel(context.Background())
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
}
