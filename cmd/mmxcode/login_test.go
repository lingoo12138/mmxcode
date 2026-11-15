package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"mmxcode/internal/site"
	"mmxcode/internal/upstream"
)

// aFakeSignIn answers the sign-in endpoints.
//
// 一个测试不许碰网络：令牌的端点在这里答，没有哪一条请求出得去。
type aFakeSignIn struct {
	mu   sync.Mutex
	seen []string
}

// RoundTrip answers one request.
func (f *aFakeSignIn) RoundTrip(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, r.Method+" "+r.URL.Path)
	body := ""
	switch r.URL.Path {
	case site.DeviceCodePath:
		body = `{"device_code":"dc","user_code":"ABCD-EFGH",` +
			`"verification_uri":"https://account.minimax.io/activate",` +
			`"expires_in":600,"interval":1}`
	case site.TokenPath:
		body = `{"access_token":"access-token","refresh_token":"rt","expires_in":3600}`
	}
	status := http.StatusOK
	if body == "" {
		status = http.StatusNotFound
		body = `{"error":"no such endpoint"}`
	}
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    r,
	}, nil
}

// TestTheLoginReadsTheCreditsOfTheNewAccount says an account signed in
// at the command line has its credits read.
//
// 刚登进来的账号马上读一次账本：不等调度器那一趟
// （usage.refresh.scan_every，默认 30 秒），人才看得到它有多少积分。
func TestTheLoginReadsTheCreditsOfTheNewAccount(t *testing.T) {
	var reads int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case site.IdentityPath:
			w.Write([]byte(`{"data":{"userInfo":{"realUserID":"1234567890",` +
				`"userEmail":"who@example.com"}}}`))
		case site.CreditPath:
			mu.Lock()
			reads++
			mu.Unlock()
			w.Write([]byte(`{"total_count":1,"details":[{"credit_type":1,` +
				`"granted_amount":"600","consumed_amount":"0","remaining_amount":"600",` +
				`"granted_at_ms":1763000000000,"expire_at_ms":1900000000000}]}`))
		default:
			w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	e := testEnvWithServer(t, srv)
	fake := &aFakeSignIn{}
	e.up = upstream.New(upstream.Options{Timeout: 5 * time.Second, Transport: fake})

	var out bytes.Buffer
	cmd := newLoginCmd(e)
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("login: %v", err)
	}
	mu.Lock()
	got := reads
	mu.Unlock()
	if got == 0 {
		t.Fatalf("the credits of the new account were not read:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "600") {
		t.Fatalf("out = %q, want it to say how much is left", out.String())
	}
}
