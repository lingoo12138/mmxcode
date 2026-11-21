package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lingoo12138/mmxcode/internal/config"
)

// TestTheHealthEndpointAnswers says the health endpoint answers.
//
// 它不用 key：容器的健康检查没有 key（密钥不进镜像），人也要一个不看
// 钥匙就能问的地址 —— 问的是「进程还在吗」，不是「这个人是谁」。
func TestTheHealthEndpointAnswers(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	if got := w.Body.String(); got != "ok\n" {
		t.Fatalf("body = %q, want %q", got, "ok\n")
	}
}

// TestTheHealthEndpointAnswersWhenThereAreKeys says the health endpoint
// answers even when the gateway has keys.
//
// 钥匙管的是「谁可以用这些账号」。健康检查不查这个：它查的是进程。
func TestTheHealthEndpointAnswersWhenThereAreKeys(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	cfg := config.Default()
	cfg.Server.APIKeys = []string{"sk-test"}
	gw := New(h.gw.Mgr, cfg, h.store, nil)
	t.Cleanup(gw.Close)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	gw.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	// 光看 200 不够：没有 key 的时候网关会把没认出来的人送到登录页，
	// 那也是 200。要看的是它答的是不是「ok」。
	if got := w.Body.String(); got != "ok\n" {
		t.Fatalf("body = %q, want %q", got, "ok\n")
	}
}
