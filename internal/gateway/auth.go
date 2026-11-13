package gateway

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// auth checks the request's key.
//
// When no key is configured the gateway does not check: it is then
// meant to listen on the loopback address only, which the server does.
func (s *Server) auth(r *http.Request) bool {
	keys := s.Cfg.Server.APIKeys
	if len(keys) == 0 {
		return true
	}
	given := r.Header.Get("x-api-key")
	if given == "" {
		auth := r.Header.Get("Authorization")
		if strings.HasPrefix(auth, "Bearer ") {
			given = strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
		}
	}
	for _, k := range keys {
		if subtle.ConstantTimeCompare([]byte(given), []byte(k)) == 1 {
			return true
		}
	}
	return false
}
