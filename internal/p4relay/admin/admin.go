package admin

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

	"p4relay/internal/p4relay/config"
	apperr "p4relay/internal/p4relay/errors"
	"p4relay/internal/p4relay/journal"
	"p4relay/internal/p4relay/proxy"
)

// Deps est la partie de la passerelle dont l'administration a besoin.
type Deps interface {
	proxy.Deps
	Journal() *journal.Journal
	PublicConfig() map[string]any
	Host() string
	Port() int
	PortOverride() *int
	StartedAt() int64
	MarkShuttingDown()
	Server() *http.Server
	StopUpstream()
}

// Motifs des routes parametrables, compiles a l'initialisation du paquet.
var (
	providerModelsRoute = regexp.MustCompile(`^/api/providers/([^/]+)/models$`)
	providerDeleteRoute = regexp.MustCompile(`^/api/providers/([^/]+)$`)
	aliasDeleteRoute    = regexp.MustCompile(`^/api/aliases/([^/]+)$`)
)

func Run(d Deps, w http.ResponseWriter, r *http.Request, route string, addresses []string) {
	if r.Header.Get("X-P4-Local") != "1" {
		panic(apperr.New(403, "Ouvrez l’interface locale pour administrer la passerelle."))
	}
	// pathID porte l'identifiant de la derniere route parametree testee.
	// matches verifie la methode, puis le motif, en une seule evaluation :
	// FindStringSubmatch repond a la fois a la question « est-ce cette route »
	// et a celle « quel est l'identifiant », la ou l'ancien routeur compilait
	// deux fois le meme motif par requete. Un cas non declenche laisse pathID
	// vide, et un cas declenche l'a toujours rempli : le corps du cas n'a donc
	// jamais a re-interroger le motif, ni a verifier la longueur du resultat.
	var pathID string
	matches := func(re *regexp.Regexp, methodOK bool) bool {
		pathID = ""
		if !methodOK {
			return false
		}
		m := re.FindStringSubmatch(r.URL.Path)
		if m == nil {
			return false
		}
		pathID = m[1]
		return true
	}
	switch {
	case route == "/api/shutdown" && r.Method == "POST":
		if _, err := apperr.ReadJSONBody(r); err != nil {
			panic(err)
		}
		d.MarkShuttingDown()
		apperr.WriteJSON(w, 200, map[string]any{"ok": true})
		go finishShutdown(d)
	case route == "/api/state" && r.Method == "GET":
		cfg := d.Store().Get()
		// Snapshot renvoie desormais les entrees en ordre chronologique ;
		// le front affiche la liste du plus recent au plus ancien (comme
		// /api/logs), d'ou Page(1, ...) plutot que Snapshot(PageSize).
		logsCopy, _ := d.Journal().Page(1, journal.PageSize)
		stats := d.Journal().Stats()
		lanEnabled := d.Host() == "0.0.0.0"
		lanUrls := []string{}
		for _, ip := range addresses {
			lanUrls = append(lanUrls, fmt.Sprintf("http://%s:%d/v1", ip, d.Port()))
		}
		configuredPort := cfg.Network.Port
		if d.PortOverride() != nil {
			configuredPort = *d.PortOverride()
		}
		state := d.PublicConfig()
		state["baseUrl"] = fmt.Sprintf("http://127.0.0.1:%d/v1", d.Port())
		state["startedAt"] = d.StartedAt()
		state["stats"] = stats
		state["logs"] = logsCopy
		state["networkStatus"] = map[string]any{
			"host":            d.Host(),
			"port":            d.Port(),
			"defaultPort":     config.DefaultPort,
			"lanEnabled":      lanEnabled,
			"portOverride":    d.PortOverride(),
			"lanUrls":         lanUrls,
			"restartRequired": lanEnabled != cfg.Network.LanEnabled || d.Port() != configuredPort,
			"nextLocalUrl":    fmt.Sprintf("http://127.0.0.1:%d", configuredPort),
		}
		// logPageSize : le front ne doit pas avoir a connaitre la taille de
		// page du journal pour compter ses pages. /api/logs la renvoie deja,
		// mais /api/state sert a l'affichage initial et au compteur, et une
		// valeur calculee ici ne peut plus diverger de celle du serveur.
		state["logPageSize"] = journal.PageSize
		apperr.WriteJSON(w, 200, state)
	case route == "/api/network" && r.Method == "POST":
		input, err := apperr.ReadJSONBody(r)
		if err != nil {
			panic(err)
		}
		nw, err := config.NetworkSettings(input)
		if err != nil {
			panic(err)
		}
		if _, err := d.Store().Mutate(func(next *config.Config) (any, error) {
			next.Network = nw
			return nil, nil
		}); err != nil {
			panic(err)
		}
		apperr.WriteJSON(w, 200, map[string]any{"ok": true})
	case route == "/api/logs" && r.Method == "GET":
		page := 1
		if p, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && p > 0 {
			page = p
		}
		slice, total := d.Journal().Page(page, journal.PageSize)
		apperr.WriteJSON(w, 200, map[string]any{"logs": slice, "total": total, "page": page, "pageSize": journal.PageSize})
	case route == "/api/logs" && r.Method == "DELETE":
		d.Journal().Clear()
		apperr.WriteJSON(w, 200, map[string]any{"ok": true})
	case route == "/api/providers" && r.Method == "POST":
		input, err := apperr.ReadJSONBody(r)
		if err != nil {
			panic(err)
		}
		result, err := d.Store().Mutate(func(next *config.Config) (any, error) {
			var existing *config.Provider
			if id, ok := input["id"].(string); ok {
				for i := range next.Providers {
					if next.Providers[i].ID == id {
						existing = &next.Providers[i]
						break
					}
				}
				if existing == nil {
					return nil, apperr.New(404, "Fournisseur introuvable.")
				}
			}
			kind, _ := input["kind"].(string)
			if kind == "" {
				kind = "custom"
			}
			if kind != "openrouter" && kind != "openai" && kind != "anthropic" && kind != "custom" {
				return nil, apperr.New(400, "Type de fournisseur invalide.")
			}
			apiKey := ""
			if existing != nil {
				apiKey = existing.APIKey
			}
			if clear, _ := input["clearKey"].(bool); clear {
				apiKey = ""
			} else if s, ok := input["apiKey"].(string); ok && apperr.TrimSpace(s) != "" {
				v, err := apperr.Required(s, "Clé API", 4096)
				if err != nil {
					return nil, err
				}
				apiKey = v
			}
			name, err := apperr.Required(input["name"], "Nom", 128)
			if err != nil {
				return nil, err
			}
			base, err := config.ValidateBaseURL(input["baseUrl"])
			if err != nil {
				return nil, err
			}
			workspaceID := ""
			if ws, present := input["workspaceId"]; present && ws != nil {
				if s, ok := ws.(string); ok {
					if apperr.TrimSpace(s) == "" {
						// Empty workspace is fine (optional field).
					} else {
						workspaceID, err = apperr.Required(s, "Workspace", 256)
						if err != nil {
							return nil, err
						}
					}
				} else {
					return nil, apperr.New(400, "Workspace invalide.")
				}
			}
			enabled := true
			if e, ok := input["enabled"].(bool); ok {
				enabled = e
			}
			p := config.Provider{
				ID:          config.RandomID(),
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
			if u != nil && config.PortOf(u) == d.Port() && config.Contains(append([]string{"localhost", "127.0.0.1"}, addresses...), u.Hostname()) {
				return nil, apperr.New(400, "Le fournisseur ne peut pas pointer vers cette passerelle.")
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
		apperr.WriteJSON(w, 200, result)
	case matches(providerModelsRoute, r.Method == "GET"):
		id := pathID
		p, err := d.ProviderFor(id)
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
				panic(apperr.New(502, "Impossible de joindre le fournisseur. Vérifiez son URL et votre connexion.", "upstream_connection_error"))
			}
			for k, v := range proxy.HeadersFor(p, true) {
				req.Header.Set(k, v)
			}
			upstream, derr := d.Client().Do(req)
			if derr != nil {
				cancel()
				panic(apperr.New(502, "Impossible de joindre le fournisseur. Vérifiez son URL et votre connexion.", "upstream_connection_error"))
			}
			if upstream.StatusCode < 200 || upstream.StatusCode > 299 {
				err := proxy.UpstreamError(d, upstream)
				upstream.Body.Close()
				cancel()
				panic(err)
			}
			var result map[string]any
			jerr := json.NewDecoder(upstream.Body).Decode(&result)
			upstream.Body.Close()
			cancel()
			if jerr != nil {
				panic(apperr.New(502, "Liste de modèles incompatible.", "upstream_error"))
			}
			data, ok := apperr.AsArray(result["data"])
			if !ok {
				panic(apperr.New(502, "Liste de modèles incompatible.", "upstream_error"))
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
		apperr.WriteJSON(w, 200, map[string]any{"data": allModels})
	case matches(providerDeleteRoute, r.Method == "DELETE"):
		id := pathID
		_, err := d.Store().Mutate(func(next *config.Config) (any, error) {
			for _, a := range next.Aliases {
				if a.ProviderID == id {
					return nil, apperr.New(409, "Supprimez ou réaffectez d’abord les alias de ce fournisseur.")
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
				return nil, apperr.New(404, "Fournisseur introuvable.")
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
		apperr.WriteJSON(w, 200, map[string]any{"ok": true})
	case route == "/api/aliases" && r.Method == "POST":
		input, err := apperr.ReadJSONBody(r)
		if err != nil {
			panic(err)
		}
		result, err := d.Store().Mutate(func(next *config.Config) (any, error) {
			var existing *config.Alias
			if id, ok := input["id"].(string); ok {
				for i := range next.Aliases {
					if next.Aliases[i].ID == id {
						existing = &next.Aliases[i]
						break
					}
				}
				if existing == nil {
					return nil, apperr.New(404, "Alias introuvable.")
				}
			}
			name, err := apperr.Required(input["name"], "Alias", 64)
			if err != nil {
				return nil, err
			}
			existingID := ""
			if existing != nil {
				existingID = existing.ID
			}
			for _, a := range next.Aliases {
				if a.Name == name && a.ID != existingID {
					return nil, apperr.New(409, "Ce nom de modèle local existe déjà.")
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
				return nil, apperr.New(400, "Choisissez un fournisseur.")
			}
			targetModel, err := apperr.Required(input["targetModel"], "Modèle réel", 256)
			if err != nil {
				return nil, err
			}
			enabled := true
			if e, ok := input["enabled"].(bool); ok {
				enabled = e
			}
			a := config.Alias{ID: config.RandomID(), Name: name, ProviderID: providerID, TargetModel: targetModel, Enabled: enabled}
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
		apperr.WriteJSON(w, 200, result)
	case matches(aliasDeleteRoute, r.Method == "DELETE"):
		id := pathID
		_, err := d.Store().Mutate(func(next *config.Config) (any, error) {
			found := false
			for _, a := range next.Aliases {
				if a.ID == id {
					found = true
					break
				}
			}
			if !found {
				return nil, apperr.New(404, "Alias introuvable.")
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
		apperr.WriteJSON(w, 200, map[string]any{"ok": true})
	case route == "/api/token/rotate" && r.Method == "POST":
		if _, err := apperr.ReadJSONBody(r); err != nil {
			panic(err)
		}
		if _, err := d.Store().Mutate(func(next *config.Config) (any, error) {
			next.LocalToken = config.RandomToken()
			return nil, nil
		}); err != nil {
			panic(err)
		}
		cfg := d.Store().Get()
		apperr.WriteJSON(w, 200, map[string]any{"localToken": cfg.LocalToken})
	default:
		panic(apperr.New(404, "Route d’administration introuvable."))
	}
}

func finishShutdown(d Deps) {
	// Give the response time to flush, then stop accepting upstream work.
	time.Sleep(200 * time.Millisecond)
	d.StopUpstream()
	if s := d.Server(); s != nil {
		_ = s.Close()
	}
	// os.Exit ne declenche pas les defer : on force ici la derniere
	// persistation du journal.
	d.Journal().Stop()
	// Exit so the port is released and the gateway can be started again.
	os.Exit(0)
}
