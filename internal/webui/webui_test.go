package webui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"mmxcode/internal/account"
	"mmxcode/internal/config"
	"mmxcode/internal/egress"
	"mmxcode/internal/manager"
	"mmxcode/internal/oauth"
	"mmxcode/internal/store"
	"mmxcode/internal/upstream"
)

func newTestWebUI(t *testing.T) (*WebUI, *store.Store) {
	t.Helper()
	s, err := store.Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	now := time.Now()
	s.CreateAccount(context.Background(), store.Account{
		ID: "acc_01", Name: "主号", Email: "a@example.com", Status: store.StatusActive,
		CreatedAt: now, UpdatedAt: now,
	})
	s.SaveToken(context.Background(), store.Token{
		AccountID: "acc_01", Access: "tok", Refresh: "ref",
		ExpiresAt: now.Add(time.Hour), UpdatedAt: now,
	})
	up := upstream.New(upstream.Options{Timeout: 5 * time.Second})
	groups := map[string]string{"warp": "socks5h://warp:4000"}
	// The account API is pointed at a stand-in server: the check-in
	// route reaches the account API, and a test must never talk to
	// MiniMax itself.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no upstream in the test", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	acc := account.New(up)
	acc.SetBase(srv.URL, srv.URL)
	mgr := manager.New(s, up, acc, egress.New(groups, ""), oauth.NewRenewer())
	cfg := config.Default()
	// The outlet groups are the configuration's; the resolver is built
	// from the same map, so the page and the resolver agree on which
	// groups there are.
	cfg.Proxy.Groups = groups
	return New(mgr, cfg, s), s
}

func TestAccountsPage(t *testing.T) {
	w, _ := newTestWebUI(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "主号") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestAPIAccounts(t *testing.T) {
	w, _ := newTestWebUI(t)
	req := httptest.NewRequest(http.MethodGet, "/api/accounts", nil)
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code = %d", rec.Code)
	}
	var out struct {
		Accounts []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Accounts) != 1 || out.Accounts[0].ID != "acc_01" {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestAPIUsage(t *testing.T) {
	w, s := newTestWebUI(t)
	s.SaveUsageSnapshot(context.Background(), store.UsageSnapshot{
		AccountID: "acc_01", HasAmounts: true,
		Granted: 600, Consumed: 100, Remaining: 500,
		ExpiresAt: time.Now().Add(time.Hour), CreatedAt: time.Now(),
	})
	req := httptest.NewRequest(http.MethodGet, "/api/usage", nil)
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code = %d", rec.Code)
	}
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out["total_granted"] != float64(600) {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestAPICheckin(t *testing.T) {
	w, _ := newTestWebUI(t)
	// 签到会打上游；这里只看路由认不认这个路径
	req := httptest.NewRequest(http.MethodPost, "/api/accounts/acc_01/checkin", nil)
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code == http.StatusNotFound {
		t.Fatalf("the route is missing")
	}
}

func TestAPIEgress(t *testing.T) {
	w, _ := newTestWebUI(t)
	req := httptest.NewRequest(http.MethodGet, "/api/egress", nil)
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "warp") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestAPIPatchAccount(t *testing.T) {
	w, s := newTestWebUI(t)
	body := strings.NewReader(`{"proxy_group":"warp","name":"新名字"}`)
	req := httptest.NewRequest(http.MethodPatch, "/api/accounts/acc_01", body)
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	a, err := s.GetAccount(context.Background(), "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if a.ProxyGroup != "warp" || a.Name != "新名字" {
		t.Fatalf("account = %+v", a)
	}
}

func TestAPIDeleteAccount(t *testing.T) {
	w, s := newTestWebUI(t)
	req := httptest.NewRequest(http.MethodDelete, "/api/accounts/acc_01", nil)
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code = %d", rec.Code)
	}
	if _, err := s.GetAccount(context.Background(), "acc_01"); err == nil {
		t.Fatal("the account is still there")
	}
}
