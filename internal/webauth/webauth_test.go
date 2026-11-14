package webauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeSite is a stand-in for the account site.
//
// It answers the four calls the login makes and writes down what it
// was asked, so a test can look at the request the client really made.
type fakeSite struct {
	t        *testing.T
	server   *httptest.Server
	logins   []map[string]any
	deviceID string
	groups   string
	approved map[string]any
}

func newFakeSite(t *testing.T) *fakeSite {
	f := &fakeSite{t: t, deviceID: "574786429807779842"}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/api/user/device/register":
			if r.Header.Get("yy") == "" || r.Header.Get("x-signature") == "" {
				t.Errorf("the device registration is not signed")
			}
			// The device registration is the one call whose own query
			// is the uuid, and the web client puts the caller's query
			// in front (design document 3.2).
			if !strings.HasPrefix(r.URL.RawQuery, "uuid=") {
				t.Errorf("the device registration query = %q, want it to start with the uuid", r.URL.RawQuery)
			}
			json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{"deviceIDStr": f.deviceID},
			})
		case "/oauth2/login":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("the login body is not json: %v", err)
			}
			f.logins = append(f.logins, body)
			if r.Header.Get("yy") == "" {
				t.Errorf("the login is not signed")
			}
			// The web flavour's own query: the common one, web's copy.
			if r.URL.Query().Get("client") != "web" || r.URL.Query().Get("device_platform") != "web" {
				t.Errorf("the login query = %q, want the web one", r.URL.RawQuery)
			}
			if body["authToken"] == nil {
				// 探测：邮箱这一步。
				json.NewEncoder(w).Encode(map[string]any{
					"data": nil, "statusInfo": map[string]any{"code": 0},
				})
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "_sid", Value: "session-1", Path: "/"})
			json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"data": map[string]any{"login_redirect": "/oauth-authorize"},
			})
		case "/oauth2/device/authorize":
			// 这两步网页用的是原生 fetch，不过 axios 拦截器：不签名，
			// 也不带那套公共 query。
			if r.Header.Get("yy") != "" || r.Header.Get("x-signature") != "" {
				t.Errorf("the device authorization is signed, but the web page does not sign it")
			}
			if r.URL.Query().Get("client") != "" {
				t.Errorf("the device authorization carries the common query, but the web page does not")
			}
			if c, err := r.Cookie("_sid"); err != nil || c.Value != "session-1" {
				t.Errorf("the device authorization has no session cookie")
			}
			if r.Method == http.MethodGet {
				w.Write([]byte(f.groups))
				return
			}
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			f.approved = body
			json.NewEncoder(w).Encode(map[string]any{"code": 0})
		default:
			t.Errorf("path = %s", r.URL.Path)
			w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeSite) client(t *testing.T) *Client {
	return New(Options{BaseURL: f.server.URL, HTTP: f.server.Client()})
}

// login makes the client hold a session, the way Password does.
func (f *fakeSite) login(t *testing.T, c *Client) {
	t.Helper()
	if err := c.Password(context.Background(), "a@example.com", "hunter2", "dev-1", "/oauth-authorize"); err != nil {
		t.Fatal(err)
	}
}

func TestTheDeviceIsRegistered(t *testing.T) {
	f := newFakeSite(t)
	c := f.client(t)
	id, err := c.RegisterDevice(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if id != "574786429807779842" {
		t.Fatalf("device id = %q", id)
	}
}

func TestTheEmailIsAskedBeforeThePassword(t *testing.T) {
	f := newFakeSite(t)
	c := f.client(t)
	if err := c.Probe(context.Background(), "a@example.com", "dev-1", "/oauth-authorize"); err != nil {
		t.Fatal(err)
	}
	if len(f.logins) != 1 {
		t.Fatalf("got %d logins", len(f.logins))
	}
	got := f.logins[0]
	if got["loginType"] != "20" {
		t.Errorf("loginType = %v", got["loginType"])
	}
	if got["email"] != "a@example.com" {
		t.Errorf("email = %v", got["email"])
	}
	if got["authToken"] != nil {
		t.Errorf("authToken = %v, want nothing", got["authToken"])
	}
}

func TestThePasswordIsEncryptedWithTheSitesKey(t *testing.T) {
	f := newFakeSite(t)
	c := f.client(t)
	if err := c.Password(context.Background(), "a@example.com", "hunter2", "dev-1", "/oauth-authorize"); err != nil {
		t.Fatal(err)
	}
	if len(f.logins) != 1 {
		t.Fatalf("got %d logins", len(f.logins))
	}
	token, _ := f.logins[0]["authToken"].(string)
	if token == "" || token == "hunter2" {
		t.Fatalf("authToken = %q", token)
	}
	// RSA-1024 密文是 128 字节，base64 之后 172 个字符。
	if len(token) != 172 {
		t.Fatalf("the cipher text is %d characters, want 172", len(token))
	}
}

func TestTheCodeIsApprovedWithTheSession(t *testing.T) {
	f := newFakeSite(t)
	f.groups = `{"code":0,"data":{"groups":[{"id":"g1","name":"default","is_default":true},{"id":"g2","is_default":false}]}}`
	c := f.client(t)
	f.login(t, c)
	if err := c.Approve(context.Background(), "ABCD-1234"); err != nil {
		t.Fatal(err)
	}
	if f.approved == nil {
		t.Fatal("nothing was approved")
	}
	if f.approved["user_code"] != "ABCD-1234" {
		t.Errorf("user_code = %v", f.approved["user_code"])
	}
	if f.approved["decision"] != "approve" {
		t.Errorf("decision = %v", f.approved["decision"])
	}
	if f.approved["group_id"] != "g1" {
		t.Errorf("group_id = %v, want the default group", f.approved["group_id"])
	}
}

func TestAGroupThatIsNotThereIsAnError(t *testing.T) {
	f := newFakeSite(t)
	f.groups = `{"code":0,"data":{"groups":[{"id":"g1","is_default":false}]}}`
	c := f.client(t)
	f.login(t, c)
	err := c.Approve(context.Background(), "ABCD-1234")
	if err == nil {
		t.Fatal("no error without a default group")
	}
	if !strings.Contains(err.Error(), "group") {
		t.Fatalf("err = %v", err)
	}
}

func TestTheSitesCodeBecomesAReadableError(t *testing.T) {
	f := newFakeSite(t)
	f.server.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"statusInfo": map[string]any{"code": 1200056, "message": "need password"},
		})
	}))
	defer srv.Close()

	c := New(Options{BaseURL: srv.URL, HTTP: srv.Client()})
	err := c.Probe(context.Background(), "a@example.com", "dev-1", "/oauth-authorize")
	if err == nil {
		t.Fatal("no error")
	}
	var se *SiteError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want a site error", err)
	}
	if se.Code != 1200056 {
		t.Fatalf("code = %d", se.Code)
	}
	if !strings.Contains(err.Error(), "密码") {
		t.Fatalf("err = %v, want it to say the account has no password", err)
	}
}

// A site that answers with an error is not an approval. The caller
// would otherwise go on waiting for a token that is never coming.
func TestAServerErrorIsNotAnApproval(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Write([]byte(`{"code":0,"data":{"groups":[{"id":"g1","is_default":true}]}}`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"boom"}`))
	}))
	defer srv.Close()

	c := New(Options{BaseURL: srv.URL, HTTP: srv.Client()})
	err := c.Approve(context.Background(), "ABCD-1234")
	if err == nil {
		t.Fatal("no error from a server that said no")
	}
}

// A refusal without a number of its own still has to say something a
// person can act on. The site's own word is English, so the Chinese
// explanation comes from the word.
func TestARefusalWithoutACodeIsStillExplained(t *testing.T) {
	err := &SiteError{Reason: "login_required", Message: "account session is missing or expired"}
	if got := err.ExplainText(); got != "要先登录才能批准" {
		t.Errorf("ExplainText = %q", got)
	}
	text := err.Error()
	if !strings.Contains(text, "登录") {
		t.Errorf("err = %q, want it to say a login is needed", text)
	}
	// The site's own words come along, but they do not replace the
	// explanation.
	if !strings.Contains(text, "account session is missing or expired") {
		t.Errorf("err = %q, want the site's own words in it too", text)
	}
}

// Even a word nobody has heard of gets a Chinese sentence.
func TestAnUnknownRefusalWordIsStillExplained(t *testing.T) {
	err := &SiteError{Reason: "something_new"}
	text := err.Error()
	if text == "" {
		t.Fatal("no explanation at all")
	}
	if !strings.Contains(text, "站点") {
		t.Errorf("err = %q, want it to say the site refused", text)
	}
}

// The approve call answers with a word, not with a number, when the
// session is no good.
func TestTheApproveRefusalIsExplained(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Write([]byte(`{"code":0,"data":{"groups":[{"id":"g1","is_default":true}]}}`))
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"error":             "login_required",
			"error_description": "account session is missing or expired",
		})
	}))
	defer srv.Close()

	c := New(Options{BaseURL: srv.URL, HTTP: srv.Client()})
	err := c.Approve(context.Background(), "ABCD-1234")
	if err == nil {
		t.Fatal("no error")
	}
	var se *SiteError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want a site error", err)
	}
	if se.Reason != "login_required" {
		t.Errorf("reason = %q", se.Reason)
	}
	if !strings.Contains(err.Error(), "登录") {
		t.Errorf("err = %v, want it to say a login is needed", err)
	}
}

// The session belongs to one account. Two clients must not share one,
// or one account's session would approve another account's code.
func TestEachClientHasItsOwnSession(t *testing.T) {
	// This one gets its own server, because the point is to see that a
	// client which never signed in sends no session cookie — and the
	// fake site above treats a missing cookie as a failure.
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie("_sid")
		if err != nil {
			seen = append(seen, "")
		} else {
			seen = append(seen, c.Value)
		}
		if r.URL.Path == "/oauth2/login" {
			http.SetCookie(w, &http.Cookie{Name: "_sid", Value: "session-1", Path: "/"})
			json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{}})
			return
		}
		if r.Method == http.MethodGet {
			w.Write([]byte(`{"code":0,"data":{"groups":[{"id":"g1","is_default":true}]}}`))
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"code": 0})
	}))
	defer srv.Close()

	a := New(Options{BaseURL: srv.URL, HTTP: srv.Client()})
	b := New(Options{BaseURL: srv.URL, HTTP: srv.Client()})
	if err := a.Password(context.Background(), "a@example.com", "hunter2", "dev-1", "/oauth-authorize"); err != nil {
		t.Fatal(err)
	}
	if err := a.Approve(context.Background(), "ABCD-1234"); err != nil {
		t.Fatal(err)
	}
	if err := b.Approve(context.Background(), "ABCD-1234"); err != nil {
		t.Fatal(err)
	}
	// a 的登录那一步还没有会话，批准那两步带着会话；b 从来没有登过，
	// 它的两步都不带。jar 是各自的。
	want := []string{"", "session-1", "session-1", "", ""}
	if len(seen) != len(want) {
		t.Fatalf("got %d requests, want %d", len(seen), len(want))
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Errorf("request %d had the session %q, want %q", i, seen[i], want[i])
		}
	}
}
