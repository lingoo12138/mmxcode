package oauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mmxcode/internal/upstream"
)

func testClient(t *testing.T) *upstream.Client {
	t.Helper()
	return upstream.New(upstream.Options{Timeout: 5 * time.Second})
}

func TestStartDevice(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth2/device/code" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if got := r.Form.Get("client_id"); got != "mcode-public" {
			t.Errorf("client_id = %q", got)
		}
		if got := r.Form.Get("code_challenge_method"); got != "S256" {
			t.Errorf("code_challenge_method = %q", got)
		}
		if r.Form.Get("code_challenge") == "" {
			t.Error("no code_challenge")
		}
		json.NewEncoder(w).Encode(map[string]any{
			"device_code":               "dev-1",
			"user_code":                 "ABCD-EFGH",
			"verification_uri":          "https://example.invalid/activate",
			"verification_uri_complete": "https://example.invalid/activate?code=ABCD-EFGH",
			"expires_in":                600,
			"interval":                  5,
		})
	}))
	defer srv.Close()

	c := testClient(t)
	d, err := StartDevice(context.Background(), c, srv.URL+"/oauth2/device/code")
	if err != nil {
		t.Fatal(err)
	}
	if d.UserCode != "ABCD-EFGH" {
		t.Fatalf("user code = %q", d.UserCode)
	}
	if d.URL != "https://example.invalid/activate?code=ABCD-EFGH" {
		t.Fatalf("url = %q", d.URL)
	}
	if d.pollField != "device_code" || d.pollValue != "dev-1" {
		t.Fatalf("poll = %s/%s", d.pollField, d.pollValue)
	}
}

func TestStartDeviceUserCodeVariant(t *testing.T) {
	// MiniMax's own variant: no device_code, the user code is polled
	// for, and the end is an absolute time in milliseconds.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"user_code":        "WXYZ-1234",
			"verification_uri": "https://example.invalid/activate",
			"expired_in":       time.Now().Add(10 * time.Minute).UnixMilli(),
			"interval":         3000,
		})
	}))
	defer srv.Close()

	c := testClient(t)
	d, err := StartDevice(context.Background(), c, srv.URL+"/oauth2/device/code")
	if err != nil {
		t.Fatal(err)
	}
	if d.pollField != "user_code" || d.pollValue != "WXYZ-1234" {
		t.Fatalf("poll = %s/%s", d.pollField, d.pollValue)
	}
	if d.interval != 3*time.Second {
		t.Fatalf("interval = %v, want 3s", d.interval)
	}
}

func TestStartDeviceExpiredInSeconds(t *testing.T) {
	// expired_in 小于 1e12 时是"还剩多少秒"，不是绝对时间。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"device_code":      "dev-2",
			"user_code":        "ABCD-EFGH",
			"verification_uri": "https://example.invalid/activate",
			"expired_in":       600,
			"interval":         5,
		})
	}))
	defer srv.Close()

	c := testClient(t)
	d, err := StartDevice(context.Background(), c, srv.URL+"/oauth2/device/code")
	if err != nil {
		t.Fatal(err)
	}
	// 十分钟，不多不少
	left := time.Until(d.deadline)
	if left < 9*time.Minute || left > 10*time.Minute {
		t.Fatalf("deadline is %v away, want about 10m", left)
	}
}

func TestStartDeviceBadAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid_client"}`))
	}))
	defer srv.Close()

	c := testClient(t)
	if _, err := StartDevice(context.Background(), c, srv.URL+"/oauth2/device/code"); err == nil {
		t.Fatal("expected an error")
	}
}

func TestPollDeviceSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth2/token" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if got := r.Form.Get("grant_type"); got != DeviceGrant {
			t.Errorf("grant_type = %q", got)
		}
		if r.Form.Get("code_verifier") == "" {
			t.Error("no code_verifier")
		}
		json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "acc-token",
			"refresh_token": "ref-token",
			"expires_in":    3600,
		})
	}))
	defer srv.Close()

	c := testClient(t)
	d := &Device{
		UserCode:  "ABCD",
		verifier:  "v",
		pollField: "device_code",
		pollValue: "dev-1",
		deadline:  time.Now().Add(time.Minute),
		interval:  time.Millisecond,
	}
	tok, err := d.Poll(context.Background(), c, srv.URL+"/oauth2/token")
	if err != nil {
		t.Fatal(err)
	}
	if tok.Access != "acc-token" || tok.Refresh != "ref-token" {
		t.Fatalf("token = %+v", tok)
	}
	if tok.Expires.Before(time.Now().Add(50 * time.Minute)) {
		t.Fatalf("expires = %v", tok.Expires)
	}
}

func TestPollKeepsWaitingWhilePending(t *testing.T) {
	// 用户还没点授权：token 端点用 400 加 authorization_pending 回答。
	// 这是正常状态，接着等就是了 —— 当成失败的话，每一次登录都死在
	// 第一轮轮询上。
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":"authorization_pending"}`))
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "acc-token",
			"refresh_token": "ref-token",
			"expires_in":    3600,
		})
	}))
	defer srv.Close()

	c := testClient(t)
	d := &Device{
		UserCode: "ABCD", verifier: "v",
		pollField: "device_code", pollValue: "dev-1",
		deadline: time.Now().Add(time.Minute), interval: time.Millisecond,
	}
	tok, err := d.Poll(context.Background(), c, srv.URL+"/oauth2/token")
	if err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("polled %d times, want 3", calls)
	}
	if tok.Access != "acc-token" {
		t.Fatalf("token = %+v", tok)
	}
}

func TestPollKeepsWaitingByStatus(t *testing.T) {
	// 另一种写法：200 加 status=pending。
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 2 {
			json.NewEncoder(w).Encode(map[string]any{"status": "pending"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "acc-token",
			"refresh_token": "ref-token",
			"expires_in":    3600,
		})
	}))
	defer srv.Close()

	c := testClient(t)
	d := &Device{
		UserCode: "ABCD", verifier: "v",
		pollField: "device_code", pollValue: "dev-1",
		deadline: time.Now().Add(time.Minute), interval: time.Millisecond,
	}
	if _, err := d.Poll(context.Background(), c, srv.URL+"/oauth2/token"); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("polled %d times, want 2", calls)
	}
}

func TestPollSlowDown(t *testing.T) {
	old := slowDownWait
	slowDownWait = time.Millisecond
	defer func() { slowDownWait = old }()
	// slow_down 是让我们等久一点，不是失败。
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 2 {
			json.NewEncoder(w).Encode(map[string]any{"error": "slow_down"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "acc-token",
			"refresh_token": "ref-token",
			"expires_in":    3600,
		})
	}))
	defer srv.Close()

	c := testClient(t)
	d := &Device{
		UserCode: "ABCD", verifier: "v",
		pollField: "device_code", pollValue: "dev-1",
		deadline: time.Now().Add(time.Minute), interval: time.Millisecond,
	}
	if _, err := d.Poll(context.Background(), c, srv.URL+"/oauth2/token"); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("polled %d times, want 2", calls)
	}
}

func TestPollDeviceDenied(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"error": "access_denied"})
	}))
	defer srv.Close()

	c := testClient(t)
	d := &Device{
		UserCode: "ABCD", verifier: "v",
		pollField: "device_code", pollValue: "dev-1",
		deadline: time.Now().Add(time.Minute), interval: time.Millisecond,
	}
	if _, err := d.Poll(context.Background(), c, srv.URL+"/oauth2/token"); err == nil {
		t.Fatal("expected an error")
	} else if !strings.Contains(err.Error(), "access_denied") {
		t.Fatalf("err = %v", err)
	}
}

func TestOAuthErrorCodes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer srv.Close()

	c := testClient(t)
	d := &Device{
		UserCode: "ABCD", verifier: "v",
		pollField: "device_code", pollValue: "dev-1",
		deadline: time.Now().Add(time.Minute), interval: time.Millisecond,
	}
	_, err := d.Poll(context.Background(), c, srv.URL+"/oauth2/token")
	if err == nil {
		t.Fatal("expected an error")
	}
	oe, ok := err.(*Error)
	if !ok {
		t.Fatalf("err = %T, want *Error", err)
	}
	if oe.Code != "invalid_grant" || oe.HTTPStatus != 400 {
		t.Fatalf("error = %+v", oe)
	}
}

func TestRefreshToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if got := r.Form.Get("grant_type"); got != "refresh_token" {
			t.Errorf("grant_type = %q", got)
		}
		if got := r.Form.Get("refresh_token"); got != "old-refresh" {
			t.Errorf("refresh_token = %q", got)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "new-access",
			"refresh_token": "new-refresh",
			"expires_in":    3600,
		})
	}))
	defer srv.Close()

	c := testClient(t)
	tok, err := Refresh(context.Background(), c, srv.URL+"/oauth2/token", "old-refresh")
	if err != nil {
		t.Fatal(err)
	}
	if tok.Access != "new-access" || tok.Refresh != "new-refresh" {
		t.Fatalf("token = %+v", tok)
	}
}

func TestRefreshKeepsOldTokenWhenNoNewOneGiven(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "new-access",
			"expires_in":   3600,
		})
	}))
	defer srv.Close()

	c := testClient(t)
	tok, err := Refresh(context.Background(), c, srv.URL+"/oauth2/token", "old-refresh")
	if err != nil {
		t.Fatal(err)
	}
	if tok.Refresh != "old-refresh" {
		t.Fatalf("refresh = %q, want the old one", tok.Refresh)
	}
}

func TestRefreshInvalidGrantIsSignedOut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer srv.Close()

	c := testClient(t)
	_, err := Refresh(context.Background(), c, srv.URL+"/oauth2/token", "old-refresh")
	oe, ok := err.(*Error)
	if !ok {
		t.Fatalf("err = %T, want *Error", err)
	}
	if !oe.IsSignedOut() {
		t.Fatal("expected the error to be a sign-out")
	}
}

func TestRenewerSpendsOnceUnderConcurrency(t *testing.T) {
	var calls int32
	r := NewRenewer()
	spend := func(ctx context.Context, refresh string) (*Token, error) {
		atomic.AddInt32(&calls, 1)
		time.Sleep(50 * time.Millisecond)
		return &Token{Access: "new-access", Refresh: "new-refresh", Expires: time.Now().Add(time.Hour)}, nil
	}
	current := &Token{Access: "old-access", Refresh: "old-refresh", Expires: time.Now().Add(time.Hour)}

	var wg sync.WaitGroup
	results := make([]*Token, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tok, err := r.Renew(context.Background(), "acc_01", current, spend)
			if err != nil {
				t.Errorf("Renew: %v", err)
				return
			}
			results[i] = tok
		}(i)
	}
	wg.Wait()

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("the refresh token was spent %d times, want 1", got)
	}
	for i, tok := range results {
		if tok == nil || tok.Access != "new-access" {
			t.Fatalf("result %d = %+v", i, tok)
		}
	}
}

func TestRenewerDoesNotSpendASpentTokenAgain(t *testing.T) {
	var calls int32
	r := NewRenewer()
	spend := func(ctx context.Context, refresh string) (*Token, error) {
		atomic.AddInt32(&calls, 1)
		return &Token{Access: "new-access", Refresh: "new-refresh", Expires: time.Now().Add(time.Hour)}, nil
	}
	current := &Token{Access: "old-access", Refresh: "old-refresh", Expires: time.Now().Add(time.Hour)}

	if _, err := r.Renew(context.Background(), "acc_01", current, spend); err != nil {
		t.Fatal(err)
	}
	// 同一个 refresh 再来一次：不能再发出去，但要把最新的那个给它
	tok, err := r.Renew(context.Background(), "acc_01", current, spend)
	if err != nil {
		t.Fatalf("Renew: %v", err)
	}
	if tok == nil || tok.Access != "new-access" {
		t.Fatalf("expected the newest token to be given, got %+v", tok)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("spend called %d times, want 1", got)
	}
}

func TestRenewerLatestWins(t *testing.T) {
	r := NewRenewer()
	first := &Token{Access: "a1", Refresh: "r1", Expires: time.Now().Add(time.Hour)}
	second := &Token{Access: "a2", Refresh: "r2", Expires: time.Now().Add(time.Hour)}
	r.Renew(context.Background(), "acc_01", first, func(ctx context.Context, refresh string) (*Token, error) {
		return second, nil
	})
	got := r.Latest("acc_01", first)
	if got == nil || got.Access != "a2" {
		t.Fatalf("latest = %+v", got)
	}
}
