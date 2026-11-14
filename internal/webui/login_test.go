package webui

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"mmxcode/internal/account"
	"mmxcode/internal/config"
	"mmxcode/internal/egress"
	"mmxcode/internal/manager"
	"mmxcode/internal/oauth"
	"mmxcode/internal/site"
	"mmxcode/internal/store"
	"mmxcode/internal/upstream"
)

// fakeSite is the site itself, answered here.
//
// 一个测试不许碰网络：令牌的端点、账号的 API 都在这里答。没有哪一条
// 请求能出去。
type fakeSite struct {
	mu       sync.Mutex
	device   string
	token    string
	identity string
	seen     []string
}

// RoundTrip answers one request.
func (f *fakeSite) RoundTrip(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, r.Method+" "+r.URL.Path)
	body, status := "", http.StatusOK
	switch r.URL.Path {
	case site.DeviceCodePath:
		body = f.device
	case site.TokenPath:
		body = f.token
	case site.IdentityPath:
		body = f.identity
	}
	if body == "" {
		status, body = http.StatusNotFound, `{"error":"no such endpoint"}`
	}
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    r,
	}, nil
}

// paths gives the paths that were asked for.
func (f *fakeSite) paths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.seen...)
}

// newFakeSiteWebUI makes a WebUI whose site is a stand-in.
//
// 账号的 API 和令牌的端点都在 stand-in 后面，所以登录这条路整条都能走
// 一遍，而没有一条请求出得去。
func newFakeSiteWebUI(t *testing.T, f *fakeSite) (*WebUI, *store.Store) {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	up := upstream.New(upstream.Options{Timeout: 5 * time.Second, Transport: f})
	acc := account.New(up)
	mgr := manager.New(s, up, acc, egress.New(map[string]string{}, ""), oauth.NewRenewer())
	return New(mgr, config.Default(), s), s
}

// deviceAnswer is what the site answers to a sign-in code.
const deviceAnswer = `{"device_code":"dc","user_code":"ABCD-EFGH",` +
	`"verification_uri":"https://account.minimax.io/activate","expires_in":600,"interval":1}`

// tokenWithSubject makes a token that carries a subject claim.
//
// 令牌里的 sub 是猜的：这个令牌带一个，好让测试看得见"猜的那个"和
// "真的那个"不是一回事。
func tokenWithSubject(sub string) string {
	payload := base64.RawURLEncoding.EncodeToString(
		[]byte(fmt.Sprintf(`{"sub":%q}`, sub)))
	return "header." + payload + ".signature"
}

// waitForAccounts waits until the store holds n accounts.
func waitForAccounts(t *testing.T, s *store.Store, n int) []store.Account {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		accs, err := s.ListAccounts(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(accs) >= n {
			return accs
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("there are not %d accounts yet", n)
	return nil
}

// TestTheWebUIAcquiresTheRealUserIDAndEmail says the account the WebUI
// makes is the account the site knows.
//
// 账号是谁要问站点本人：令牌里的那个 sub 是猜的，签到和积分明细要的
// 是真的那个 user id。猜出来的那个号，签到签不上，积分明细也是空的。
func TestTheWebUIAcquiresTheRealUserIDAndEmail(t *testing.T) {
	f := &fakeSite{
		device: deviceAnswer,
		token: `{"access_token":"` + tokenWithSubject("jwt-subject") +
			`","refresh_token":"rt","expires_in":3600}`,
		identity: `{"data":{"userInfo":{"realUserID":"1234567890",` +
			`"userEmail":"who@example.com"}}}`,
	}
	w, s := newFakeSiteWebUI(t, f)

	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/accounts", nil))
	if rec.Code != 200 {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	accs := waitForAccounts(t, s, 1)
	acc := accs[0]
	if acc.UserID != "1234567890" {
		t.Errorf("user id = %q, want the one the site says", acc.UserID)
	}
	if acc.Email != "who@example.com" {
		t.Errorf("email = %q, want the one the site says", acc.Email)
	}
	if acc.Name != "who@example.com" {
		t.Errorf("name = %q, want the email the site says", acc.Name)
	}
	if !strings.HasPrefix(acc.ID, "acc_") {
		t.Errorf("id = %q, want the acc_ prefix", acc.ID)
	}
	// 站点本人被问过了：那个 user id 和邮箱是从它那里来的。
	asked := false
	for _, p := range f.paths() {
		if p == "GET "+site.IdentityPath {
			asked = true
		}
	}
	if !asked {
		t.Errorf("the identity API was never asked: %v", f.paths())
	}
}

// TestTheWebUIKeepsTheLoginWhenTheIdentityIsUnreadable says a sign-in
// that worked is not thrown away because the identity could not be read.
//
// 登录成功了就是登录成功了：读不到账户信息只是少知道一点，账号照样建
// 起来，用令牌自己带着的东西填。
func TestTheWebUIKeepsTheLoginWhenTheIdentityIsUnreadable(t *testing.T) {
	f := &fakeSite{
		device: deviceAnswer,
		token: `{"access_token":"` + tokenWithSubject("jwt-subject") +
			`","refresh_token":"rt","expires_in":3600}`,
		// 身份读不到：没有回答就是没有回答。
		identity: "",
	}
	w, s := newFakeSiteWebUI(t, f)

	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/accounts", nil))
	if rec.Code != 200 {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	accs := waitForAccounts(t, s, 1)
	acc := accs[0]
	if acc.UserID != "jwt-subject" {
		t.Errorf("user id = %q, want the one the token carries", acc.UserID)
	}
	if acc.Name != "jwt-subject" {
		t.Errorf("name = %q, want the one the token carries", acc.Name)
	}
	if acc.Email != "" {
		t.Errorf("email = %q, want none", acc.Email)
	}
}

// TestTwoSignInsDoNotShareAnAccountID says two sign-ins that end at the
// same moment do not fight over one id.
//
// 账号的 id 是主键：两个登录在同一毫秒里结束，第二个会被数据库挡下
// 来。挡下来的那一个要是没人说，这一次登录就悄悄没了。
func TestTwoSignInsDoNotShareAnAccountID(t *testing.T) {
	f := &fakeSite{
		device: deviceAnswer,
		token: `{"access_token":"` + tokenWithSubject("jwt-subject") +
			`","refresh_token":"rt","expires_in":3600}`,
		identity: `{"data":{"userInfo":{"realUserID":"1234567890"}}}`,
	}
	w, s := newFakeSiteWebUI(t, f)

	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		w.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/accounts", nil))
		if rec.Code != 200 {
			t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
		}
	}
	accs := waitForAccounts(t, s, 3)
	ids := map[string]bool{}
	for _, a := range accs {
		if ids[a.ID] {
			t.Fatalf("two accounts have the id %q", a.ID)
		}
		ids[a.ID] = true
		if !strings.HasPrefix(a.ID, "acc_") {
			t.Errorf("id = %q, want the acc_ prefix", a.ID)
		}
	}
}

// TestTheAccountIDIsNotOnlyTheTime says the id is not the time alone.
//
// 光是时间不够：同一毫秒里结束的两个登录会拿到同一个 id。
func TestTheAccountIDIsNotOnlyTheTime(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		id := newAccountID()
		if !strings.HasPrefix(id, "acc_") {
			t.Fatalf("id = %q, want the acc_ prefix", id)
		}
		if seen[id] {
			t.Fatalf("the same id twice: %q", id)
		}
		seen[id] = true
	}
}

// TestARefusedSignInIsToldInTheBrowser says the page is told why the
// sign-in did not work.
//
// 失败要说出来：页面只看到"还在等"，人只当是网慢。
func TestARefusedSignInIsToldInTheBrowser(t *testing.T) {
	f := &fakeSite{
		device: deviceAnswer,
		token:  `{"error":"access_denied","error_description":"the user refused"}`,
	}
	w, s := newFakeSiteWebUI(t, f)

	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/accounts", nil))
	if rec.Code != 200 {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	var started struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		rec := httptest.NewRecorder()
		body := strings.NewReader(fmt.Sprintf(`{"id":%q}`, started.ID))
		w.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/accounts/poll", body))
		var out struct {
			Error string `json:"error"`
			Done  bool   `json:"done"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if out.Error != "" {
			if !strings.Contains(out.Error, "access_denied") {
				t.Fatalf("the error is %q, want the site's own words", out.Error)
			}
			accs, err := s.ListAccounts(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(accs) != 0 {
				t.Fatalf("the refused sign-in made an account: %+v", accs)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the refusal never came")
}

// TestAFailedSignInIsForgotten says a sign-in that failed does not stay
// forever.
//
// 说完失败，那次登录就该忘掉：服务器开得久，不能把每一次登录都留着。
func TestAFailedSignInIsForgotten(t *testing.T) {
	w, _ := newTestWebUI(t)
	w.mu.Lock()
	w.pendingTTL = 50 * time.Millisecond
	w.pending["pending_1"] = &pendingLogin{ID: "pending_1"}
	w.mu.Unlock()
	w.failLogin("pending_1", "the site refused the sign-in")

	// 页面先问到那句话。
	rec := httptest.NewRecorder()
	body := strings.NewReader(`{"id":"pending_1"}`)
	w.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/accounts/poll", body))
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["error"] != "the site refused the sign-in" {
		t.Fatalf("body = %s, want the reason", rec.Body.String())
	}

	// 过了一会儿，那次登录不在了。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		w.mu.Lock()
		n := len(w.pending)
		w.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the failed sign-in is still there")
}

// TestThePollDoesNotRaceWithAFailedSignIn says the page can ask about a
// sign-in while it fails.
//
// 页面每隔几秒问一次登录怎么了，登录失败的时候那个失败是写进去的。
// 读它的时候得拿着锁：字符串是两个词，读到一半就是半个指针加一个
// 不在那里的长度。
func TestThePollDoesNotRaceWithAFailedSignIn(t *testing.T) {
	w, _ := newTestWebUI(t)
	w.mu.Lock()
	w.pending["pending_1"] = &pendingLogin{ID: "pending_1"}
	w.mu.Unlock()

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	// 登录失败：那个失败是写在锁里的。
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			w.mu.Lock()
			p := w.pending["pending_1"]
			if p != nil {
				p.Err = "the site refused the sign-in"
			}
			w.mu.Unlock()
		}
	}()
	// 页面在问。
	for i := 0; i < 500; i++ {
		rec := httptest.NewRecorder()
		body := strings.NewReader(`{"id":"pending_1"}`)
		w.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/accounts/poll", body))
	}
	close(done)
	wg.Wait()
}

// TestTheDeviceCodeIsNeverMarkup says the code the site gives is never
// written as markup.
//
// 上游答上来的验证码和地址是给人看的，不是给浏览器执行的。Go 这边拼
// 出来的每一段页面都是模板里的，脚本那边也不许有 innerHTML。
func TestTheDeviceCodeIsNeverMarkup(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range []string{"innerHTML", "template.HTML"} {
			if strings.Contains(string(b), bad) {
				t.Errorf("%s has %s in it", name, bad)
			}
		}
	}
	js, err := os.ReadFile(filepath.Join("static", "app.js"))
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"innerHTML", `href="${`, "href='${"} {
		if strings.Contains(string(js), bad) {
			t.Errorf("static/app.js has %s in it", bad)
		}
	}
}

// TestTheLoginPageEscapesTheDeviceCode says the page shows the code as
// text, and the address as an address.
//
// 验证码是上游给的，地址也是。一个 <script> 进去就是脚本，一个
// javascript: 的地址进去就是点一下就跑。
func TestTheLoginPageEscapesTheDeviceCode(t *testing.T) {
	w, _ := newTestWebUI(t)
	w.mu.Lock()
	w.pending["pending_1"] = &pendingLogin{
		ID:       "pending_1",
		UserCode: `<script>alert("x")</script>`,
		URL:      "javascript:alert(1)",
	}
	w.mu.Unlock()

	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/login", nil))
	if rec.Code != 200 {
		t.Fatalf("code = %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "<script>alert") {
		t.Fatalf("the code was written as markup: %s", body)
	}
	if strings.Contains(body, "javascript:alert") {
		t.Fatalf("the address was written as a link: %s", body)
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Fatalf("the code is not escaped: %s", body)
	}
}
