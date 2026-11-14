package gateway

import (
	"encoding/json"
	"net/http"
)

// writeError writes a plain error answer.
//
// The shape is the one of the protocol the client speaks: an OpenAI
// client reads the error out of "error", and an Anthropic or a Codex
// client reads the type out of it as well.
func writeError(w http.ResponseWriter, proto protocol, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(errorBody(proto, "api_error", msg))
}

// writeRateLimit writes the answer for a refusal that is about the
// credits.
//
// The client is told the limit was reached, so it knows to wait and
// try again rather than that something broke.
func writeRateLimit(w http.ResponseWriter, proto protocol, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	json.NewEncoder(w).Encode(errorBody(proto, "rate_limit_error", "usage limit reached: "+message))
}

// errorBody gives the body of an error.
//
// The Anthropic and the Codex protocols carry the type beside the
// error; the OpenAI one carries the error on its own.
func errorBody(proto protocol, kind, msg string) map[string]any {
	errObj := map[string]any{
		"type":    kind,
		"message": msg,
	}
	if proto == protocolOpenAI {
		return map[string]any{"error": errObj}
	}
	return map[string]any{
		"type":  "error",
		"error": errObj,
	}
}
