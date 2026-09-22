package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

const maxBody = 16 * 1024 * 1024

// ApiError reflects lib/errors.mjs
type ApiError struct {
	Status  int
	Message string
	Code    string
}

func (e *ApiError) Error() string { return e.Message }

func apiError(status int, message string, code ...string) *ApiError {
	c := "invalid_request"
	if len(code) > 0 {
		c = code[0]
	}
	return &ApiError{Status: status, Message: message, Code: c}
}

// anthropicError reflects anthropicError() in lib/errors.mjs
func anthropicError(status int, message string) map[string]any {
	typ := "invalid_request_error"
	switch {
	case status == 401:
		typ = "authentication_error"
	case status == 403:
		typ = "permission_error"
	case status == 404:
		typ = "not_found_error"
	case status == 413:
		typ = "request_too_large"
	case status == 429:
		typ = "rate_limit_error"
	case status == 529:
		typ = "overloaded_error"
	case status >= 500:
		typ = "api_error"
	}
	return map[string]any{
		"type":  "error",
		"error": map[string]any{"type": typ, "message": message},
	}
}

func openaiError(status int, message string, code string) map[string]any {
	if code == "" {
		code = "internal_error"
	}
	typ := "invalid_request_error"
	if status >= 500 {
		typ = "server_error"
	}
	return map[string]any{
		"error": map[string]any{"message": message, "type": typ, "code": code},
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// readJSONBody mirrors body(): requires JSON content-type, 16 Mo max, returns an object.
func readJSONBody(r *http.Request) (map[string]any, error) {
	if !isJSONBody(r) {
		return nil, apiError(415, "Content-Type: application/json requis.")
	}
	buf, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		return nil, apiError(500, "Lecture du corps impossible.")
	}
	if int64(len(buf)) > maxBody {
		return nil, apiError(413, "Requête trop volumineuse (maximum 16 Mo).")
	}
	var value any
	if err := json.Unmarshal(buf, &value); err != nil {
		return nil, apiError(400, "JSON invalide.")
	}
	obj, ok := value.(map[string]any)
	if !ok {
		return nil, apiError(400, "Un objet JSON est requis.")
	}
	return obj, nil
}

func isJSONBody(r *http.Request) bool {
	ct := r.Header.Get("Content-Type")
	return len(ct) >= len("application/json") && ct[:len("application/json")] == "application/json"
}

func isPlainObject(v any) bool {
	_, ok := v.(map[string]any)
	return ok
}

// required mirrors required(value, name, max)
func required(value any, name string, max int) (string, error) {
	s, ok := value.(string)
	if !ok || len(trimSpace(s)) == 0 || len([]rune(trimSpace(s))) > max || hasControlChars(s) {
		return "", apiError(400, fmt.Sprintf("%s invalide.", name))
	}
	return trimSpace(s), nil
}

func hasControlChars(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c <= 0x1f || c == 0x7f {
			return true
		}
	}
	return false
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && isSpace(s[start]) {
		start++
	}
	for end > start && isSpace(s[end-1]) {
		end--
	}
	return s[start:end]
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v'
}

// asInt reports whether v is an integer JSON number.
func asInt(v any) (int64, bool) {
	f, ok := v.(float64)
	if !ok || f != float64(int64(f)) {
		return 0, false
	}
	return int64(f), true
}

// asBool reports whether v is a JSON boolean.
func asBool(v any) (bool, bool) {
	b, ok := v.(bool)
	return b, ok
}

// asString reports whether v is a JSON string.
func asString(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

// asArray reports whether v is a JSON array.
func asArray(v any) ([]any, bool) {
	a, ok := v.([]any)
	return a, ok
}
