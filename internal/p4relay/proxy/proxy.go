package proxy

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

	"p4relay/internal/p4relay/anthropic"
	"p4relay/internal/p4relay/config"
	apperr "p4relay/internal/p4relay/errors"
	"p4relay/internal/p4relay/journal"
)

const timeout = 180 * time.Second

// Deps est la partie de la passerelle dont le proxy a besoin.
type Deps interface {
	Store() *config.Store
	Client() *http.Client
	Redact(string) string
	Record(journal.Entry)
	ProviderFor(string) (*config.Provider, error)
}

func HeadersFor(p *config.Provider, models bool) map[string]string {
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

func UpstreamError(d Deps, resp *http.Response) error {
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
	return apperr.New(status, d.Redact("Fournisseur : "+message), "upstream_error")
}

// lineScanner scans SSE lines with a 16 MiB buffer.
type lineScanner struct {
	sc *bufio.Scanner
}

func newLineScanner(body io.Reader) *lineScanner {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64*1024), anthropic.SSELineLimit+1)
	return &lineScanner{sc: sc}
}

func (s *lineScanner) Scan() bool { return s.sc.Scan() }

// Err signale une erreur de lecture autre que la fin normale du flux.
func (s *lineScanner) Err() error { return s.sc.Err() }
func (s *lineScanner) Text() string {
	return s.sc.Text()
}

// responseGuard repond aux deux questions que le proxy doit se poser quand une
// erreur survient : la reponse a-t-elle deja commence (apres le premier octet
// d'un flux SSE, le statut HTTP est parti et vouloir le remplacer serait faux),
// et sait-on reellement flusher. canFlush reporte la capacite du writer reel,
// pas celle de l'enveloppe.
type responseGuard struct {
	http.ResponseWriter
	headerSent bool
	canFlush   bool
}

func (g *responseGuard) WriteHeader(code int) {
	g.headerSent = true
	g.ResponseWriter.WriteHeader(code)
}

func (g *responseGuard) Write(data []byte) (int, error) {
	g.headerSent = true
	return g.ResponseWriter.Write(data)
}

func (g *responseGuard) Flush() {
	if g.canFlush {
		g.ResponseWriter.(http.Flusher).Flush()
	}
}

// wrappedResponse enveloppe un ResponseWriter sans lui preter un Flusher qu'il
// n'a pas.
func wrappedResponse(w http.ResponseWriter) *responseGuard {
	_, ok := w.(http.Flusher)
	return &responseGuard{ResponseWriter: w, canFlush: ok}
}

// canFlushTo dit si un flush est réellement possible, a travers l'enveloppe.
func canFlushTo(w http.ResponseWriter) bool {
	if g, ok := w.(*responseGuard); ok {
		return g.canFlush
	}
	_, ok := w.(http.Flusher)
	return ok
}

// aliasStream mirrors aliasStream(stream, model): rewrites the model field.
// La valeur de retour est nommee : une panique recuperee ici (JSON amont
// invalide, evenement d'erreur emis par le fournisseur en cours de flux) doit
// devenir l'erreur renvoyee, sans quoi l'appelant lirait un flux hache en
// succes. out.Err reste reserve aux vrais echecs d'ecriture : s'en servir pour
// memoriser l'erreur applicative muet l'emission de l'evenement d'erreur vers
// le client.
func aliasStream(body io.Reader, model string, out *anthropic.SSEWriter) (err error) {
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
			panic(apperr.New(502, "Événement JSON invalide dans le flux du fournisseur.", "upstream_error"))
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
				panic(apperr.New(502, msg, "upstream_error"))
			}
			if _, present := obj["model"]; present {
				obj["model"] = model
			}
		}
		return "data: " + anthropic.MustJSON(chunk)
	}
	defer func() {
		if p := recover(); p != nil {
			if apiErr, ok := p.(*apperr.ApiError); ok {
				// Erreur attendue : JSON amont invalide ou evenement d'erreur
				// emis par le fournisseur en cours de flux. Elle devient la
				// valeur retournee : sans elle, l'appelant lirait un flux
				// interrompu comme un succes.
				err = apiErr
				return
			}
			// Panique inattendue : elle n'est pas convertie en succes, elle
			// est remise en circulation vers le mecanisme superieur.
			panic(p)
		}
	}()
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		out.Write([]byte(rewrite(line) + "\n"))
	}
	// Une erreur de lecture autre que la fin normale du flux doit etre vue.
	if serr := scanner.Err(); serr != nil {
		if out.Err == nil {
			out.Err = serr
		}
		return apperr.New(502, "Lecture du flux du fournisseur interrompue.", "upstream_error")
	}
	if !doneSeen {
		return apperr.New(502, "Le flux du fournisseur s’est interrompu avant sa fin.", "upstream_error")
	}
	// out.Err ne porte plus ici que de veritables echecs d'ecriture vers
	// le client (connexion rompue) : ils ne doivent pas non plus passer
	// pour un succes.
	return out.Err
}

func Run(d Deps, w http.ResponseWriter, r *http.Request, protocol string, countOnly bool) {
	started := time.Now()
	w = wrappedResponse(w)
	var aliasName, providerName, targetModel string
	status := 500
	streaming := false

	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	var proxyErr error
	defer func() {
		var apiErr *apperr.ApiError
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
		// Erreur remontee alors qu'aucun octet n'a ete envoye : le client
		// doit la recevoir, pas une reponse 200 vide. Apres le debut d'un
		// flux, le statut HTTP n'est plus modifiable : doProxy a emis un
		// evenement d'erreur SSE et le journal garde la trace de l'echec.
		if proxyErr != nil {
			g := w.(*responseGuard)
			if !g.headerSent && r.Context().Err() == nil {
				writeClientError(d, w, protocol, proxyErr)
			}
		}
		d.Record(journal.Entry{
			ID:         config.RandomID(),
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

	input, err := apperr.ReadJSONBody(r)
	if err != nil {
		proxyErr = err
		return
	}
	if s, ok := input["stream"].(bool); ok {
		streaming = s
	}
	var name string
	if name, err = apperr.Required(input["model"], "Nom du modèle", 64); err != nil {
		proxyErr = err
		return
	}
	msgs, ok := apperr.AsArray(input["messages"])
	if !ok || len(msgs) == 0 {
		proxyErr = apperr.New(400, "messages doit être une liste non vide de messages avec un rôle.")
		return
	}
	for _, m := range msgs {
		obj, ok := m.(map[string]any)
		if !ok {
			proxyErr = apperr.New(400, "messages doit être une liste non vide de messages avec un rôle.")
			return
		}
		if _, ok := obj["role"].(string); !ok {
			proxyErr = apperr.New(400, "messages doit être une liste non vide de messages avec un rôle.")
			return
		}
	}
	if v, present := input["stream"]; present {
		if _, ok := v.(bool); !ok {
			proxyErr = apperr.New(400, "stream doit être un booléen.")
			return
		}
	}
	if protocol == "anthropic" {
		if err = anthropic.ValidateMessages(input, countOnly); err != nil {
			proxyErr = err
			return
		}
	}

	cfg := d.Store().Get()
	found := false
	for i := range cfg.Aliases {
		a := &cfg.Aliases[i]
		if a.Name == name && a.Enabled {
			found = true
			aliasName = a.Name
			targetModel = a.TargetModel
			provider, perr := d.ProviderFor(a.ProviderID)
			if perr != nil {
				proxyErr = perr
				return
			}
			providerName = provider.Name
			if _, present := input["models"]; present {
				proxyErr = apperr.New(400, "Les champs models et route ne sont pas acceptés : utilisez un alias local.")
				return
			}
			if _, present := input["route"]; present {
				proxyErr = apperr.New(400, "Les champs models et route ne sont pas acceptés : utilisez un alias local.")
				return
			}
			proxyErr = doProxy(d, w, r, ctx, provider, aliasName, targetModel, protocol, countOnly, streaming, input)
			if proxyErr == nil {
				// Le succes ne s'ecrit qu'apres un appel reussi : une erreur
				// amont ne traverse plus Run paree d'un 200. Les erreurs
				// portant deja leur statut sont reevaluees par le defer.
				status = 200
			}
			return
		}
	}
	if !found {
		proxyErr = apperr.New(404, fmt.Sprintf("Alias inconnu ou désactivé : %s. Ajoutez-le dans l’interface.", name), "model_not_found")
	}
}

// writeClientError ecrit une erreur HTTP au format attendu par le client :
// enveloppe OpenAI pour /v1/chat/completions, enveloppe Anthropic pour
// /v1/messages et son comptage de tokens. C'est le meme contrat que celui du
// routeur pour les paniques, et la version JSON des messages d'erreur deja
// utilises en amont.
func writeClientError(d Deps, w http.ResponseWriter, protocol string, err error) {
	var apiErr *apperr.ApiError
	if !errors.As(err, &apiErr) {
		apiErr = apperr.New(500, "Erreur interne.", "internal")
	}
	msg := d.Redact(apiErr.Message)
	body := any(apperr.OpenAI(apiErr.Status, msg, apiErr.Code))
	if protocol == "anthropic" {
		body = apperr.Anthropic(apiErr.Status, msg)
	}
	apperr.WriteJSON(w, apiErr.Status, body)
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
func doProxy(d Deps, w http.ResponseWriter, r *http.Request, ctx context.Context, provider *config.Provider, aliasName, targetModel, protocol string, countOnly, streaming bool, input map[string]any) error {
	native := protocol == "anthropic" && provider.Kind == "anthropic"
	var outgoing map[string]any
	var err error
	if protocol == "anthropic" && !native {
		outgoing, err = anthropic.ToChatRequest(input, targetModel, provider.Kind)
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
		apperr.WriteJSON(w, 200, map[string]any{"input_tokens": anthropic.EstimateInputTokens(outgoing)})
		return nil
	}
	headers := HeadersFor(provider, native)
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
	upstream, err := d.Client().Do(req)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return apperr.New(504, "Le fournisseur a dépassé le délai de réponse (180 s par défaut).", "upstream_timeout")
		}
		if errors.Is(r.Context().Err(), context.Canceled) {
			return apperr.New(499, "Requête annulée.", "upstream_timeout")
		}
		return apperr.New(502, "Impossible de joindre le fournisseur. Vérifiez son URL et votre connexion.", "upstream_connection_error")
	}
	defer upstream.Body.Close()
	if upstream.StatusCode < 200 || upstream.StatusCode >= 300 {
		if ra := upstream.Header.Get("Retry-After"); ra != "" {
			w.Header().Set("Retry-After", ra)
		}
		return UpstreamError(d, upstream)
	}
	if countOnly {
		var result map[string]any
		if err := json.NewDecoder(upstream.Body).Decode(&result); err != nil {
			return apperr.New(502, "Comptage Anthropic invalide.", "upstream_error")
		}
		n, ok := apperr.AsInt(result["input_tokens"])
		if !ok || n < 0 {
			return apperr.New(502, "Comptage Anthropic invalide.", "upstream_error")
		}
		apperr.WriteJSON(w, 200, map[string]any{"input_tokens": n})
		return nil
	}
	if streaming {
		ct := upstream.Header.Get("Content-Type")
		if !strings.Contains(ct, "text/event-stream") {
			return apperr.New(502, "Le fournisseur n’a pas renvoyé de flux SSE.", "upstream_error")
		}
		if !canFlushTo(w) {
			return apperr.New(500, "Streaming non disponible.")
		}
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache, no-transform")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(200)
		out := &anthropic.SSEWriter{Dst: w}
		streamErr := runStream(protocol, native, upstream.Body, aliasName, out)
		if streamErr != nil {
			// Les en-tetes sont partis : le statut HTTP ne peut plus etre
			// remplace, l'erreur voyage donc dans le flux, au format du
			// protocole appele. Si la connexion est rompue (out.Err),
			// l'emission est muette et l'erreur remontee telle quelle :
			// c'est bien un echec, pas un succes.
			emitStreamError(d, protocol, out, streamErr)
		}
		return streamErr
	}
	// Non-streaming.
	raw, err := io.ReadAll(io.LimitReader(upstream.Body, apperr.MaxBody+1))
	if err != nil {
		return apperr.New(502, "Réponse JSON invalide du fournisseur.", "upstream_error")
	}
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		return apperr.New(502, "Réponse JSON invalide du fournisseur.", "upstream_error")
	}
	if native {
		if result["type"] != "message" {
			return apperr.New(502, "Réponse incompatible avec Anthropic Messages.", "upstream_error")
		}
		if _, ok := apperr.AsArray(result["content"]); !ok {
			return apperr.New(502, "Réponse incompatible avec Anthropic Messages.", "upstream_error")
		}
		result["model"] = aliasName
		apperr.WriteJSON(w, 200, result)
		return nil
	}
	if !apperr.IsPlainObject(result) {
		return apperr.New(502, "Réponse incompatible avec Chat Completions.", "upstream_error")
	}
	if _, ok := apperr.AsArray(result["choices"]); !ok {
		return apperr.New(502, "Réponse incompatible avec Chat Completions.", "upstream_error")
	}
	if protocol == "anthropic" {
		converted, err := anthropic.FromChatResponse(result, aliasName)
		if err != nil {
			return err
		}
		apperr.WriteJSON(w, 200, converted)
		return nil
	}
	result["model"] = aliasName
	apperr.WriteJSON(w, 200, result)
	return nil
}

// runStream lance le transformateur de flux adapte au protocole. Les
// transformateurs signalent aussi leurs erreurs par panique ( evenement SSE
// trop volumineux ) : une panique qui atteindrait le routeur apres le debut du
// flux produirait une seconde reponse sur une connexion deja engagee. Elle est
// donc convertie ici en erreur de flux, tandis qu'une panique inattendue est
// remise en circulation.
func runStream(protocol string, native bool, body io.Reader, aliasName string, out *anthropic.SSEWriter) (err error) {
	defer func() {
		if p := recover(); p != nil {
			if apiErr, ok := p.(*apperr.ApiError); ok {
				err = apiErr
				return
			}
			panic(p)
		}
	}()
	switch {
	case protocol == "anthropic" && native:
		return anthropic.NativeMessageStream(body, aliasName, out)
	case protocol == "anthropic":
		return anthropic.ChatToMessageStream(body, aliasName, out)
	default:
		return aliasStream(body, aliasName, out)
	}
}

// emitStreamError ecrit l'evenement d'erreur dans un flux deja commence.
func emitStreamError(d Deps, protocol string, out *anthropic.SSEWriter, streamErr error) {
	st := 502
	var apiErr *apperr.ApiError
	if errors.As(streamErr, &apiErr) {
		st = apiErr.Status
	}
	msg := d.Redact(apiErrorMessage(streamErr))
	if protocol == "anthropic" {
		out.Write([]byte(anthropic.SSEEncode(apperr.Anthropic(st, msg))))
	} else {
		out.Write([]byte("data: " + anthropic.MustJSON(map[string]any{"error": map[string]any{"message": msg, "type": "upstream_error"}}) + "\n\n"))
	}
}

func apiErrorMessage(err error) string {
	var apiErr *apperr.ApiError
	if errors.As(err, &apiErr) {
		return apiErr.Message
	}
	return "Connexion au fournisseur interrompue."
}
