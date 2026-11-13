package gateway

import (
	"encoding/json"
	"net/http"

	"mmxcode/internal/upstream"
)

// writeError writes a plain error answer.
func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    "api_error",
			"message": msg,
		},
	})
}

// writeRateLimit writes the answer for a refusal that is about the
// credits.
//
// The client is told the limit was reached, so it knows to wait and
// try again rather than that something broke.
func writeRateLimit(w http.ResponseWriter, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	json.NewEncoder(w).Encode(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    "rate_limit_error",
			"message": "usage limit reached: " + message,
		},
	})
}

// quotaMessage says what the answer's message is.
func quotaMessage(body []byte) string {
	var env struct {
		Message string `json:"message"`
		Error   *struct {
			Message string `json:"message"`
		} `json:"error"`
		Msg string `json:"msg"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return string(body)
	}
	switch {
	case env.Error != nil && env.Error.Message != "":
		return env.Error.Message
	case env.Message != "":
		return env.Message
	case env.Msg != "":
		return env.Msg
	}
	return "the account's credits or plan are used up"
}

// isQuotaRefusal says the answer is a refusal for the credits.
func isQuotaRefusal(status int, body []byte) bool {
	switch status {
	case http.StatusPaymentRequired, http.StatusForbidden, http.StatusTooManyRequests:
		return upstream.IsQuotaRefusal(body)
	}
	return false
}
