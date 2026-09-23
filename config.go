package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sync"
)

type Provider struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	BaseURL     string `json:"baseUrl"`
	Kind        string `json:"kind"`
	APIKey      string `json:"apiKey"`
	WorkspaceID string `json:"workspaceId"`
	Enabled     bool   `json:"enabled"`
}

type Alias struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	ProviderID  string `json:"providerId"`
	TargetModel string `json:"targetModel"`
	Enabled     bool   `json:"enabled"`
}

type Network struct {
	LanEnabled bool `json:"lanEnabled"`
	Port       int  `json:"port"`
}

type Config struct {
	Version    int        `json:"version"`
	LocalToken string     `json:"localToken"`
	Providers  []Provider `json:"providers"`
	Aliases    []Alias    `json:"aliases"`
	Network    Network    `json:"network"`
}

var presets = []Provider{
	{ID: "openrouter", Name: "OpenRouter", BaseURL: "https://openrouter.ai/api/v1", Kind: "openrouter"},
	{ID: "openai", Name: "OpenAI", BaseURL: "https://api.openai.com/v1", Kind: "openai"},
	{ID: "anthropic", Name: "Claude · Anthropic", BaseURL: "https://api.anthropic.com/v1", Kind: "anthropic"},
}

func randomToken() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return "p4-" + hex.EncodeToString(b)
}

func randomID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	s := hex.EncodeToString(b)
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32]
}

func networkSettings(value any) (Network, error) {
	def := Network{LanEnabled: false, Port: 7777}
	if value == nil {
		return def, nil
	}
	obj, ok := value.(map[string]any)
	if !ok {
		return Network{}, apiError(400, "Le port doit être un entier entre 1 et 65535 et l’accès réseau doit être ON ou OFF.")
	}
	lan, okL := obj["lanEnabled"].(bool)
	if !okL {
		return Network{}, apiError(400, "Le port doit être un entier entre 1 et 65535 et l’accès réseau doit être ON ou OFF.")
	}
	port, okP := asInt(obj["port"])
	if !okP || port < 1 || port > 65535 {
		return Network{}, apiError(400, "Le port doit être un entier entre 1 et 65535 et l’accès réseau doit être ON ou OFF.")
	}
	return Network{LanEnabled: lan, Port: int(port)}, nil
}

// encryptConfigForDisk renvoie la config avec les clés API chiffrées (si clé maîtresse).
func encryptConfigForDisk(key []byte, cfg *Config) (*Config, error) {
	if key == nil {
		return cfg, nil
	}
	data, _ := json.Marshal(cfg)
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	for i := range c.Providers {
		enc, err := encryptValue(key, c.Providers[i].APIKey)
		if err != nil {
			return nil, err
		}
		c.Providers[i].APIKey = enc
	}
	return &c, nil
}

// decryptConfigFromDisk renvoie la config avec les clés API en clair (si clé maîtresse).
func decryptConfigFromDisk(key []byte, cfg *Config) (*Config, error) {
	if key == nil {
		return cfg, nil
	}
	data, _ := json.Marshal(cfg)
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	for i := range c.Providers {
		if c.Providers[i].APIKey == "" {
			continue
		}
		if !isEncryptedValue(c.Providers[i].APIKey) {
			continue // clé en clair (installation antérieure) : laissée telle quelle, migrée au premier enregistrement
		}
		plain, err := decryptValue(key, c.Providers[i].APIKey)
		if err != nil {
			return nil, fmt.Errorf("fournisseur « %s » : %v", c.Providers[i].Name, err)
		}
		c.Providers[i].APIKey = plain
	}
	return &c, nil
}

// loadConfig reads or creates data/config.json (mirrors createApp init).
// Les clés API lues du disque sont déchiffrées en mémoire (clé maîtresse).
func loadConfig(dataDir string, key []byte) (*Config, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("impossible de créer le dossier de données : %v", err)
	}
	configFile := filepath.Join(dataDir, "config.json")
	raw, err := os.ReadFile(configFile)
	if err != nil {
		if os.IsNotExist(err) {
			cfg := &Config{
				Version:    1,
				LocalToken: randomToken(),
				Providers:  make([]Provider, 0, len(presets)),
				Aliases:    []Alias{},
				Network:    Network{LanEnabled: false, Port: 7777},
			}
			for _, p := range presets {
				p.APIKey = ""
				p.WorkspaceID = ""
				p.Enabled = true
				cfg.Providers = append(cfg.Providers, p)
			}
			data, _ := json.MarshalIndent(cfg, "", "  ")
			if werr := os.WriteFile(configFile, data, 0o600); werr != nil {
				return nil, fmt.Errorf("impossible d’écrire la configuration : %v", werr)
			}
			return cfg, nil
		}
		return nil, fmt.Errorf("Configuration illisible. Corrigez ou restaurez data/config.json avant de redémarrer.")
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("Configuration illisible. Corrigez ou restaurez data/config.json avant de redémarrer.")
	}
	if cfg.Version != 1 || cfg.LocalToken == "" {
		return nil, fmt.Errorf("Configuration illisible. Corrigez ou restaurez data/config.json avant de redémarrer.")
	}
	var rawNet struct {
		Network map[string]any `json:"network"`
	}
	if err := json.Unmarshal(raw, &rawNet); err != nil {
		return nil, fmt.Errorf("Configuration illisible. Corrigez ou restaurez data/config.json avant de redémarrer.")
	}
	nw, err := networkSettings(rawNet.Network)
	if err != nil {
		return nil, err
	}
	cfg.Network = nw
	if cfg.Providers == nil {
		cfg.Providers = []Provider{}
	}
	if cfg.Aliases == nil {
		cfg.Aliases = []Alias{}
	}
	plain, err := decryptConfigFromDisk(key, &cfg)
	if err != nil {
		return nil, err
	}
	// Migration : si le fichier contenait encore des clés en clair
	// (installation antérieure), on le réécrit chiffré. Les valeurs vides
	// ne sont jamais chiffrées (un fournisseur sans clé garde "apiKey": "").
	needMigrate := false
	for i := range cfg.Providers {
		if cfg.Providers[i].APIKey != "" && !isEncryptedValue(cfg.Providers[i].APIKey) {
			needMigrate = true
			break
		}
	}
	if needMigrate {
		disk, merr := encryptConfigForDisk(key, &cfg)
		if merr != nil {
			return nil, merr
		}
		data, _ := json.MarshalIndent(disk, "", "  ")
		if werr := os.WriteFile(configFile, data, 0o600); werr != nil {
			return nil, fmt.Errorf("impossible de chiffrer la configuration : %v", werr)
		}
	}
	return plain, nil
}

// store provides serialized mutation of the config file (mirrors mutate()).
// config contient les clés API en CLAIR en mémoire ; le fichier config.json
// les stocke chiffrées en AES-GCM avec la clé maîtresse (data/key ou variable
// d'environnement P4RELAY_MASTER_KEY).
type store struct {
	mu     sync.Mutex
	file   string
	key    []byte // clé maîtresse AES-GCM
	config *Config
}

func (s *store) mutate(fn func(next *Config) (any, error)) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.clone()
	result, err := fn(next)
	if err != nil {
		return nil, err
	}
	toWrite := next
	if s.key != nil {
		toWrite, err = encryptConfigForDisk(s.key, next)
		if err != nil {
			return nil, apiError(500, "Impossible de chiffrer la clé API.")
		}
	}
	data, _ := json.MarshalIndent(toWrite, "", "  ")
	tmp := s.file + "." + randomID() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return nil, apiError(500, "Opération impossible. Vérifiez la connexion et l’accès au fichier de configuration.")
	}
	if err := os.Rename(tmp, s.file); err != nil {
		_ = os.Remove(tmp)
		return nil, apiError(500, "Opération impossible. Vérifiez la connexion et l’accès au fichier de configuration.")
	}
	s.config = next
	return result, nil
}

func (s *store) clone() *Config {
	data, _ := json.Marshal(s.config)
	var c Config
	_ = json.Unmarshal(data, &c)
	return &c
}

func (s *store) get() *Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.clone()
}

// lanAddresses returns the non-internal IPv4 addresses (mirrors lanAddresses()).
func lanAddresses() []string {
	seen := map[string]bool{}
	var out []string
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return out
	}
	for _, a := range addrs {
		ipNet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipNet.IP.To4()
		if ip == nil || ip.IsLoopback() {
			continue
		}
		s := ip.String()
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func loopback(address string) bool {
	if address == "::1" {
		return true
	}
	return regexp.MustCompile(`^127\.`).MatchString(address) || regexp.MustCompile(`^::ffff:127\.`).MatchString(address)
}

// baseUrl mirrors baseUrl(): https required except localhost http, no credentials/query/hash.
func validateBaseURL(value any) (string, error) {
	raw, err := required(value, "URL de base", 2000)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", apiError(400, "URL de base invalide.")
	}
	local := u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost" || u.Hostname() == "::1"
	if (u.Scheme != "https" && !(u.Scheme == "http" && local)) || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", apiError(400, "Utilisez HTTPS, ou HTTP pour un fournisseur sur localhost. Aucun identifiant ni paramètre dans l’URL.")
	}
	// normalize: strip trailing slashes
	for len(raw) > 0 && raw[len(raw)-1] == '/' {
		raw = raw[:len(raw)-1]
	}
	return raw, nil
}

// portOf returns the explicit port of a URL, defaulting to 443/80.
func portOf(u *url.URL) int {
	if u.Port() != "" {
		var p int
		fmt.Sscanf(u.Port(), "%d", &p)
		return p
	}
	if u.Scheme == "https" {
		return 443
	}
	return 80
}
