package oauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
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

// testDevice makes a device that polls at once, for a test that does
// not care about the waiting.
func testDevice() *Device {
	return &Device{
		UserCode:  "ABCD",
		verifier:  "v",
		pollField: "device_code",
		pollValue: "dev-1",
		deadline:  time.Now().Add(time.Minute),
		interval:  time.Millisecond,
		slowDown:  time.Millisecond,
	}
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

func TestStartDeviceWithADeadCode(t *testing.T) {
	// 站点说这个码已经过期了：不该再等十分钟。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"device_code":      "dev-3",
			"user_code":        "ABCD-EFGH",
			"verification_uri": "https://example.invalid/activate",
			"expired_in":       time.Now().Add(-time.Minute).UnixMilli(),
			"interval":         5,
		})
	}))
	defer srv.Close()

	c := testClient(t)
	_, err := StartDevice(context.Background(), c, srv.URL+"/oauth2/device/code")
	if err == nil {
		t.Fatal("expected an error")
	}
	oe, ok := err.(*Error)
	if !ok {
		t.Fatalf("err = %T, want *Error", err)
	}
	if oe.Code != "expired_token" {
		t.Fatalf("error = %+v", oe)
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
	d := testDevice()
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
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) < 3 {
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
	d := testDevice()
	tok, err := d.Poll(context.Background(), c, srv.URL+"/oauth2/token")
	if err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("polled %d times, want 3", got)
	}
	if tok.Access != "acc-token" {
		t.Fatalf("token = %+v", tok)
	}
}

func TestPollKeepsWaitingByStatus(t *testing.T) {
	// 另一种写法：200 加 status=pending。
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) < 2 {
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
	d := testDevice()
	if _, err := d.Poll(context.Background(), c, srv.URL+"/oauth2/token"); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("polled %d times, want 2", got)
	}
}

func TestPollSlowDown(t *testing.T) {
	// slow_down 是让我们等久一点，不是失败。
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) < 2 {
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
	d := testDevice()
	if _, err := d.Poll(context.Background(), c, srv.URL+"/oauth2/token"); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("polled %d times, want 2", got)
	}
}

func TestPollDeviceDenied(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"error": "access_denied"})
	}))
	defer srv.Close()

	c := testClient(t)
	d := testDevice()
	if _, err := d.Poll(context.Background(), c, srv.URL+"/oauth2/token"); err == nil {
		t.Fatal("expected an error")
	} else if !strings.Contains(err.Error(), "access_denied") {
		t.Fatalf("err = %v", err)
	}
}

func TestPollNeedsARefreshToken(t *testing.T) {
	// 没有 refresh token 的账号活不过一小时：与其建一个这样的账号，
	// 不如让这次登录失败。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "acc-token",
			"expires_in":   3600,
		})
	}))
	defer srv.Close()

	c := testClient(t)
	d := testDevice()
	_, err := d.Poll(context.Background(), c, srv.URL+"/oauth2/token")
	if err == nil {
		t.Fatal("expected an error")
	}
	oe, ok := err.(*Error)
	if !ok {
		t.Fatalf("err = %T, want *Error", err)
	}
	if oe.Code != "missing_refresh_token" {
		t.Fatalf("error = %+v", oe)
	}
}

func TestOAuthErrorCodes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer srv.Close()

	c := testClient(t)
	d := testDevice()
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

func TestDeviceTextKeepsTheVerifierSecret(t *testing.T) {
	d := &Device{
		UserCode: "ABCD-EFGH",
		URL:      "https://example.invalid/activate",
		verifier: "the-pkce-verifier",
	}
	texts := []string{
		d.String(),
		fmt.Sprintf("%+v", d),
		fmt.Sprintf("%v", d),
		fmt.Sprintf("%s", d),
	}
	for _, text := range texts {
		if strings.Contains(text, "the-pkce-verifier") {
			t.Fatalf("the verifier is in the text: %s", text)
		}
	}
	if !strings.Contains(d.String(), "ABCD-EFGH") {
		t.Fatalf("the text says nothing useful: %s", d.String())
	}
}

func TestClaims(t *testing.T) {
	payload := `{"sub":"user-1","account_id":"site-1"}`
	// 没有点号的东西不是 JWT。
	if _, _, ok := claims("not-a-jwt"); ok {
		t.Fatal("a string that is not a JWT was read as one")
	}
	// 补了填充的 payload 也是 JWT。
	padded := "x." + base64.URLEncoding.EncodeToString([]byte(payload)) + ".y"
	sub, account, ok := claims(padded)
	if !ok {
		t.Fatal("a padded payload was not read")
	}
	if sub != "user-1" || account != "site-1" {
		t.Fatalf("claims = %q/%q", sub, account)
	}
	// 没补填充的也是。
	raw := "x." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".y"
	sub, account, ok = claims(raw)
	if !ok || sub != "user-1" || account != "site-1" {
		t.Fatalf("claims = %q/%q, ok = %v", sub, account, ok)
	}
	// 读不出 payload 的不是 JWT。
	if _, _, ok := claims("x.!!!.y"); ok {
		t.Fatal("a payload that cannot be decoded was read as one")
	}
}

func TestTheUnitsOfTheSite(t *testing.T) {
	// expires_in 的单位
	for _, c := range []struct {
		name string
		in   int64
		want time.Duration
	}{
		{"an hour in seconds", 3600, time.Hour},
		{"an hour in milliseconds", 3600000, time.Hour},
		{"ten minutes in milliseconds", 600000, 10 * time.Minute},
		{"a day in seconds", 86400, 24 * time.Hour},
		{"nothing", 0, 0},
	} {
		if got := expiresDuration(c.in); got != c.want {
			t.Errorf("expiresDuration(%d) = %v, want %v (%s)", c.in, got, c.want, c.name)
		}
	}
	// 轮询间隔的单位
	for _, c := range []struct {
		name string
		in   int64
		want time.Duration
	}{
		{"five seconds", 5, 5 * time.Second},
		{"five minutes", 300, 5 * time.Minute},
		{"three seconds in milliseconds", 3000, 3 * time.Second},
		{"nothing", 0, 0},
	} {
		if got := pollInterval(c.in); got != c.want {
			t.Errorf("pollInterval(%d) = %v, want %v (%s)", c.in, got, c.want, c.name)
		}
	}
	// expired_in 的单位
	now := time.Now()
	for _, c := range []struct {
		name string
		in   int64
		want time.Time
	}{
		{"ten minutes left in seconds", 600, now.Add(10 * time.Minute)},
		{"ten minutes left in milliseconds", 600000, now.Add(10 * time.Minute)},
		{"an absolute time in milliseconds", now.Add(10 * time.Minute).UnixMilli(), now.Add(10 * time.Minute)},
		{"an absolute time in seconds", now.Add(10 * time.Minute).Unix(), now.Add(10 * time.Minute)},
	} {
		got, ok := siteDeadline(c.in)
		if !ok {
			t.Errorf("siteDeadline(%d) said nothing (%s)", c.in, c.name)
			continue
		}
		if diff := got.Sub(c.want); diff < -time.Minute || diff > time.Minute {
			t.Errorf("siteDeadline(%d) = %v, want about %v (%s)", c.in, got, c.want, c.name)
		}
	}
	if _, ok := siteDeadline(0); ok {
		t.Error("siteDeadline(0) said something")
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

func TestRefreshSignOutInA200Body(t *testing.T) {
	// MiniMax 把业务错误放在 200 的 body 里。签出也是这么来的。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"error": "invalid_grant"})
	}))
	defer srv.Close()

	c := testClient(t)
	_, err := Refresh(context.Background(), c, srv.URL+"/oauth2/token", "old-refresh")
	oe, ok := err.(*Error)
	if !ok {
		t.Fatalf("err = %T, want *Error", err)
	}
	if oe.HTTPStatus != 200 {
		t.Fatalf("error = %+v", oe)
	}
	if !oe.IsSignedOut() {
		t.Fatal("a sign-out in a 200 body was not a sign-out")
	}
}

func TestRefreshKeepsTheMessage(t *testing.T) {
	// 有的 4xx 只带 message：这个说明是账号的 last error。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid_request","message":"the code is wrong"}`))
	}))
	defer srv.Close()

	c := testClient(t)
	_, err := Refresh(context.Background(), c, srv.URL+"/oauth2/token", "old-refresh")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "the code is wrong") {
		t.Fatalf("err = %v, want the message", err)
	}
	// 刷新的错误说的是 token 端点，不是登录服务器。
	if !strings.Contains(err.Error(), "token endpoint") {
		t.Fatalf("err = %v, want it to name the token endpoint", err)
	}
}

func TestRefreshWithALostAnswer(t *testing.T) {
	// 请求到了站点，答案没了：站点可能已经把 refresh token 用了。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		panic(http.ErrAbortHandler)
	}))
	defer srv.Close()

	c := testClient(t)
	_, err := Refresh(context.Background(), c, srv.URL+"/oauth2/token", "old-refresh")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, ErrRefreshOutcomeUnknown) {
		t.Fatalf("err = %v, want an unknown outcome", err)
	}
}

func TestRefreshWithARefusedConnection(t *testing.T) {
	// 连都没连上：请求肯定没到站点，同一个 token 再来一次是安全的。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	endpoint := srv.URL + "/oauth2/token"
	srv.Close()

	c := testClient(t)
	_, err := Refresh(context.Background(), c, endpoint, "old-refresh")
	if err == nil {
		t.Fatal("expected an error")
	}
	if errors.Is(err, ErrRefreshOutcomeUnknown) {
		t.Fatalf("err = %v, want a plain failure", err)
	}
}

func TestRenewerSpendsOnceUnderConcurrency(t *testing.T) {
	var calls int32
	release := make(chan struct{})
	r := NewRenewer()
	spend := func(ctx context.Context, refresh string) (*Token, error) {
		atomic.AddInt32(&calls, 1)
		<-release
		return &Token{Access: "new-access", Refresh: "new-refresh", Expires: time.Now().Add(time.Hour)}, nil
	}
	current := &Token{Access: "old-access", Refresh: "old-refresh", Expires: time.Now().Add(time.Hour)}

	var wg sync.WaitGroup
	var started sync.WaitGroup
	started.Add(8)
	results := make([]*Token, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			started.Done()
			tok, err := r.Renew(context.Background(), "acc_01", current, spend)
			if err != nil {
				t.Errorf("Renew: %v", err)
				return
			}
			results[i] = tok
		}(i)
	}
	started.Wait()
	// 让每个 goroutine 都进到 Renew 里去：它们都得等同一个刷新。
	time.Sleep(200 * time.Millisecond)
	close(release)
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

func TestRenewerWalksTheChainOnAfterALostStoreWrite(t *testing.T) {
	// 站点给了新 token，存库失败，库里还是那个已经用过的。
	// renewer 手上有新的：它得把它用掉，链才走得下去。
	var calls int32
	var mu sync.Mutex
	var spent []string
	r := NewRenewer()
	spend := func(ctx context.Context, refresh string) (*Token, error) {
		atomic.AddInt32(&calls, 1)
		mu.Lock()
		spent = append(spent, refresh)
		mu.Unlock()
		return &Token{
			Access:  "acc-for-" + refresh,
			Refresh: "new-" + refresh,
			Expires: time.Now().Add(time.Hour),
		}, nil
	}
	stored := &Token{Access: "old-access", Refresh: "old-refresh", Expires: time.Now().Add(time.Hour)}

	first, err := r.Renew(context.Background(), "acc_01", stored, spend)
	if err != nil {
		t.Fatal(err)
	}
	// 存库失败：调用方手上还是那个旧 token。
	second, err := r.Renew(context.Background(), "acc_01", stored, spend)
	if err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("the chain was walked %d times, want 2", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if spent[1] != "new-old-refresh" {
		t.Fatalf("the second spend used %q, want the token the renewer held", spent[1])
	}
	if second.Refresh == first.Refresh {
		t.Fatalf("the chain did not move on: %+v", second)
	}
}

func TestRenewerSpendsAgainWhenTheSiteDoesNotRotate(t *testing.T) {
	// 站点不给新的 refresh token：它没有把旧的用掉，
	// 所以下一次刷新还得再发一次。
	var calls int32
	r := NewRenewer()
	spend := func(ctx context.Context, refresh string) (*Token, error) {
		atomic.AddInt32(&calls, 1)
		return &Token{
			Access:  fmt.Sprintf("acc-%d", atomic.LoadInt32(&calls)),
			Refresh: refresh,
			Expires: time.Now().Add(time.Hour),
		}, nil
	}
	stored := &Token{Access: "old-access", Refresh: "same-refresh", Expires: time.Now().Add(time.Hour)}

	if _, err := r.Renew(context.Background(), "acc_01", stored, spend); err != nil {
		t.Fatal(err)
	}
	tok, err := r.Renew(context.Background(), "acc_01", stored, spend)
	if err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("the refresh token was spent %d times, want 2", got)
	}
	if tok.Access == "old-access" {
		t.Fatalf("the token = %+v, want a new one", tok)
	}
}

func TestRenewerFollowsTheCallersNewerToken(t *testing.T) {
	// renewer 手上的 token 调用方不知道：调用方拿的是新的
	// （刚登录的，或者别的进程写进去的）。调用方说了算。
	var calls int32
	var mu sync.Mutex
	var spent []string
	r := NewRenewer()
	spend := func(ctx context.Context, refresh string) (*Token, error) {
		atomic.AddInt32(&calls, 1)
		mu.Lock()
		spent = append(spent, refresh)
		mu.Unlock()
		return &Token{
			Access:  "acc-for-" + refresh,
			Refresh: "new-" + refresh,
			Expires: time.Now().Add(time.Hour),
		}, nil
	}
	if _, err := r.Renew(context.Background(), "acc_01",
		&Token{Access: "a1", Refresh: "r1", Expires: time.Now().Add(time.Hour)}, spend); err != nil {
		t.Fatal(err)
	}
	newer := &Token{Access: "a2", Refresh: "r2", Expires: time.Now().Add(time.Hour)}
	tok, err := r.Renew(context.Background(), "acc_01", newer, spend)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(spent) != 2 || spent[1] != "r2" {
		t.Fatalf("spent = %v, want the caller's token", spent)
	}
	if tok.Access != "acc-for-r2" {
		t.Fatalf("token = %+v, want the answer of the caller's own renewal", tok)
	}
}

func TestRenewerDoesNotMarkAFailedRefreshSpent(t *testing.T) {
	// 这个包的全部承诺：失败的刷新不把 token 记成用掉了。
	var calls int32
	var fail atomic.Bool
	r := NewRenewer()
	spend := func(ctx context.Context, refresh string) (*Token, error) {
		atomic.AddInt32(&calls, 1)
		if fail.Load() {
			return nil, errors.New("the network is down")
		}
		return &Token{
			Access:  "new-access",
			Refresh: "new-refresh",
			Expires: time.Now().Add(time.Hour),
		}, nil
	}
	stored := &Token{Access: "old-access", Refresh: "old-refresh", Expires: time.Now().Add(time.Hour)}

	fail.Store(true)
	if _, err := r.Renew(context.Background(), "acc_01", stored, spend); err == nil {
		t.Fatal("expected an error")
	}
	fail.Store(false)
	// 失败过的 token 还能再发：它多半没有到站点。
	tok, err := r.Renew(context.Background(), "acc_01", stored, spend)
	if err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("the refresh token was sent %d times, want 2", got)
	}
	if tok.Access != "new-access" {
		t.Fatalf("token = %+v", tok)
	}
}

func TestRenewerDoesNotSendATokenOfUnknownOutcomeAgain(t *testing.T) {
	var calls int32
	r := NewRenewer()
	spend := func(ctx context.Context, refresh string) (*Token, error) {
		atomic.AddInt32(&calls, 1)
		return nil, fmt.Errorf("the answer was lost: %w", ErrRefreshOutcomeUnknown)
	}
	stored := &Token{Access: "old-access", Refresh: "old-refresh", Expires: time.Now().Add(time.Hour)}

	_, err := r.Renew(context.Background(), "acc_01", stored, spend)
	if !errors.Is(err, ErrRefreshOutcomeUnknown) {
		t.Fatalf("err = %v, want an unknown outcome", err)
	}
	// 再来一次：答案不知道的 token 不再发。
	_, err = r.Renew(context.Background(), "acc_01", stored, spend)
	if !errors.Is(err, ErrRefreshOutcomeUnknown) {
		t.Fatalf("err = %v, want an unknown outcome", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("the refresh token was sent %d times, want 1", got)
	}
}

func TestRenewerGivesEveryCallerItsOwnToken(t *testing.T) {
	// 两个调用方各拿一个 refresh token：各发各的，
	// 谁也不能拿别人的答案。
	var calls int32
	release := make(chan struct{})
	var mu sync.Mutex
	seen := map[string]bool{}
	r := NewRenewer()
	spend := func(ctx context.Context, refresh string) (*Token, error) {
		atomic.AddInt32(&calls, 1)
		mu.Lock()
		seen[refresh] = true
		mu.Unlock()
		<-release
		return &Token{
			Access:  "acc-for-" + refresh,
			Refresh: "new-" + refresh,
			Expires: time.Now().Add(time.Hour),
		}, nil
	}
	tokens := map[string]*Token{
		"r1": {Access: "a1", Refresh: "r1", Expires: time.Now().Add(time.Hour)},
		"r2": {Access: "a2", Refresh: "r2", Expires: time.Now().Add(time.Hour)},
	}
	results := map[string]*Token{}
	var wg sync.WaitGroup
	var mu2 sync.Mutex
	for name, tok := range tokens {
		wg.Add(1)
		go func(name string, tok *Token) {
			defer wg.Done()
			got, err := r.Renew(context.Background(), "acc_01", tok, spend)
			if err != nil {
				t.Errorf("Renew: %v", err)
				return
			}
			mu2.Lock()
			results[name] = got
			mu2.Unlock()
		}(name, tok)
	}
	// 等两个刷新都开始了。
	for i := 0; i < 200 && atomic.LoadInt32(&calls) < 2; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	close(release)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if !seen["r1"] || !seen["r2"] {
		t.Fatalf("spent = %v, want both tokens", seen)
	}
	for name, tok := range results {
		if tok == nil {
			t.Fatalf("%s got nothing", name)
		}
		if want := "acc-for-" + name; tok.Access != want {
			t.Fatalf("%s got %+v, want the answer of its own renewal", name, tok)
		}
	}
}

func TestRenewerLeaderCancellationDoesNotKillTheFollowers(t *testing.T) {
	// 第一个来的调用方断线了，别的调用方还在等：
	// 刷新不该跟着死。
	var calls int32
	release := make(chan struct{})
	r := NewRenewer()
	spend := func(ctx context.Context, refresh string) (*Token, error) {
		atomic.AddInt32(&calls, 1)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return &Token{Access: "new-access", Refresh: "new-refresh", Expires: time.Now().Add(time.Hour)}, nil
	}
	stored := &Token{Access: "old-access", Refresh: "old-refresh", Expires: time.Now().Add(time.Hour)}

	leaderCtx, cancel := context.WithCancel(context.Background())
	leaderErr := make(chan error, 1)
	go func() {
		_, err := r.Renew(leaderCtx, "acc_01", stored, spend)
		leaderErr <- err
	}()
	// 等刷新真的开始了。
	for i := 0; i < 200 && atomic.LoadInt32(&calls) == 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	followerErr := make(chan error, 1)
	followerTok := make(chan *Token, 1)
	go func() {
		tok, err := r.Renew(context.Background(), "acc_01", stored, spend)
		followerTok <- tok
		followerErr <- err
	}()
	time.Sleep(100 * time.Millisecond)
	// 第一个来的调用方走了。
	cancel()
	if err := <-leaderErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("the leader got %v, want the cancellation", err)
	}
	close(release)
	if err := <-followerErr; err != nil {
		t.Fatalf("the follower got %v", err)
	}
	if tok := <-followerTok; tok == nil || tok.Access != "new-access" {
		t.Fatalf("the follower got %+v", tok)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("the refresh token was spent %d times, want 1", got)
	}
}

func TestRenewerForget(t *testing.T) {
	r := NewRenewer()
	var calls int32
	spend := func(ctx context.Context, refresh string) (*Token, error) {
		atomic.AddInt32(&calls, 1)
		return &Token{Access: "new-access", Refresh: "new-refresh", Expires: time.Now().Add(time.Hour)}, nil
	}
	stored := &Token{Access: "old-access", Refresh: "old-refresh", Expires: time.Now().Add(time.Hour)}

	if _, err := r.Renew(context.Background(), "acc_01", stored, spend); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Newest("acc_01"); !ok {
		t.Fatal("the renewer holds nothing")
	}
	r.Forget("acc_01")
	if _, ok := r.Newest("acc_01"); ok {
		t.Fatal("the token is still held after Forget")
	}
	// 已经用掉的记录也没了：这个账号签出了，它的东西都不作数了。
	if _, err := r.Renew(context.Background(), "acc_01", stored, spend); err != nil {
		t.Fatalf("Renew after Forget: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("the refresh token was spent %d times, want 2", got)
	}
}

func TestRenewerForgetLeavesARunningRenewalAlone(t *testing.T) {
	// 账号正在签出时在跑的刷新，完了不能把 token 放回来。
	var calls int32
	release := make(chan struct{})
	r := NewRenewer()
	spend := func(ctx context.Context, refresh string) (*Token, error) {
		atomic.AddInt32(&calls, 1)
		<-release
		return &Token{Access: "new-access", Refresh: "new-refresh", Expires: time.Now().Add(time.Hour)}, nil
	}
	stored := &Token{Access: "old-access", Refresh: "old-refresh", Expires: time.Now().Add(time.Hour)}

	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Renew(context.Background(), "acc_01", stored, spend)
	}()
	for i := 0; i < 200 && atomic.LoadInt32(&calls) == 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	r.Forget("acc_01")
	close(release)
	<-done
	if _, ok := r.Newest("acc_01"); ok {
		t.Fatal("the token came back from the dead")
	}
}

func TestRenewerReset(t *testing.T) {
	// Reset 把链忘掉，"用掉了"的记录还在。
	var calls int32
	r := NewRenewer()
	spend := func(ctx context.Context, refresh string) (*Token, error) {
		atomic.AddInt32(&calls, 1)
		return &Token{Access: "new-access", Refresh: "new-refresh", Expires: time.Now().Add(time.Hour)}, nil
	}
	stored := &Token{Access: "old-access", Refresh: "old-refresh", Expires: time.Now().Add(time.Hour)}

	if _, err := r.Renew(context.Background(), "acc_01", stored, spend); err != nil {
		t.Fatal(err)
	}
	r.Reset("acc_01")
	if _, ok := r.Newest("acc_01"); ok {
		t.Fatal("the token is still held after Reset")
	}
	// 用掉的 token 不能再发：链断了，账号得重新登录。
	_, err := r.Renew(context.Background(), "acc_01", stored, spend)
	if !errors.Is(err, ErrRefreshChainBroken) {
		t.Fatalf("err = %v, want a broken chain", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("the refresh token was spent %d times, want 1", got)
	}
}

func TestRenewerChainBroken(t *testing.T) {
	// renewer 手上的 token 也用掉了：链断了，没有能走的路了。
	var calls int32
	r := NewRenewer()
	spend := func(ctx context.Context, refresh string) (*Token, error) {
		atomic.AddInt32(&calls, 1)
		return &Token{Access: "new-access", Refresh: "new-refresh", Expires: time.Now().Add(time.Hour)}, nil
	}
	r.mu.Lock()
	r.latest["acc_01"] = &Token{Access: "a2", Refresh: "r2", Expires: time.Now().Add(time.Hour)}
	r.spent["acc_01"] = map[string]bool{
		tokenHash("r1"): true,
		tokenHash("r2"): true,
	}
	r.mu.Unlock()

	_, err := r.Renew(context.Background(), "acc_01",
		&Token{Access: "a1", Refresh: "r1", Expires: time.Now().Add(time.Hour)}, spend)
	if !errors.Is(err, ErrRefreshChainBroken) {
		t.Fatalf("err = %v, want a broken chain", err)
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("a spent token was sent %d times", got)
	}
}

func TestRenewerRefusesAnotherAccountsToken(t *testing.T) {
	r := NewRenewer()
	spend := func(ctx context.Context, refresh string) (*Token, error) {
		return &Token{
			Access:  "new-access",
			Refresh: "new-refresh",
			Expires: time.Now().Add(time.Hour),
			Subject: "user-1",
		}, nil
	}
	if _, err := r.Renew(context.Background(), "acc_01",
		&Token{Access: "a1", Refresh: "r1", Subject: "user-1", Expires: time.Now().Add(time.Hour)}, spend); err != nil {
		t.Fatal(err)
	}
	_, err := r.Renew(context.Background(), "acc_01",
		&Token{Access: "a9", Refresh: "r9", Subject: "user-2", Expires: time.Now().Add(time.Hour)}, spend)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "another account") {
		t.Fatalf("err = %v", err)
	}
}

func TestRenewerNewestGivesACopy(t *testing.T) {
	r := NewRenewer()
	spend := func(ctx context.Context, refresh string) (*Token, error) {
		return &Token{Access: "new-access", Refresh: "new-refresh", Expires: time.Now().Add(time.Hour)}, nil
	}
	if _, err := r.Renew(context.Background(), "acc_01",
		&Token{Access: "old-access", Refresh: "old-refresh", Expires: time.Now().Add(time.Hour)}, spend); err != nil {
		t.Fatal(err)
	}
	got, ok := r.Newest("acc_01")
	if !ok || got.Access != "new-access" {
		t.Fatalf("newest = %+v, ok = %v", got, ok)
	}
	got.Access = "changed"
	again, _ := r.Newest("acc_01")
	if again.Access == "changed" {
		t.Fatal("the caller changed what the renewer holds")
	}
	// 没有 token 时 Latest 给的是调用方自己的。
	empty := NewRenewer()
	cur := &Token{Access: "mine"}
	if got := empty.Latest("acc_02", cur); got != cur {
		t.Fatalf("latest = %+v, want the caller's own token", got)
	}
	if _, ok := empty.Newest("acc_02"); ok {
		t.Fatal("the renewer holds a token it does not have")
	}
}

func TestNoSecretIsInAnyErrorOrURL(t *testing.T) {
	const secret = "the-secret-refresh-token"
	const verifier = "the-secret-pkce-verifier"

	// 刷新的错误。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid_grant","message":"no"}`))
	}))
	defer srv.Close()
	c := testClient(t)
	_, err := Refresh(context.Background(), c, srv.URL+"/oauth2/token", secret)
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("the refresh token is in the error: %v", err)
	}

	// renewer 的错误。
	r := NewRenewer()
	_, err = r.Renew(context.Background(), "acc_01",
		&Token{Access: "a", Refresh: secret},
		func(ctx context.Context, refresh string) (*Token, error) {
			return nil, errors.New("nope")
		})
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("the refresh token is in the error: %v", err)
	}
	// 链断了的错误也不带 token。
	// 先成功一次：那个 token 就用掉了。
	if _, err = r.Renew(context.Background(), "acc_01",
		&Token{Access: "a", Refresh: secret},
		func(ctx context.Context, refresh string) (*Token, error) {
			return &Token{Access: "b", Refresh: "other", Expires: time.Now().Add(time.Hour)}, nil
		}); err != nil {
		t.Fatalf("Renew: %v", err)
	}
	r.Reset("acc_01")
	_, err = r.Renew(context.Background(), "acc_01",
		&Token{Access: "a", Refresh: secret},
		func(ctx context.Context, refresh string) (*Token, error) {
			return &Token{Access: "b", Refresh: "other", Expires: time.Now().Add(time.Hour)}, nil
		})
	if !errors.Is(err, ErrRefreshChainBroken) {
		t.Fatalf("err = %v, want a broken chain", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("the refresh token is in the error: %v", err)
	}

	// 设备码的错误和地址。
	d := &Device{
		UserCode: "ABCD",
		URL:      "https://example.invalid/activate?code=ABCD",
		verifier: verifier,
	}
	for _, text := range []string{d.String(), fmt.Sprintf("%+v", d), d.URL} {
		if strings.Contains(text, verifier) {
			t.Fatalf("the verifier is in %q", text)
		}
	}
}
