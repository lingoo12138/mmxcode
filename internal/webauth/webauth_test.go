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
			// is the uuid. The web client puts the common parameters
			// first and the caller's own parameter after them, so the
			// query ends with the uuid — which is in the common block
			// as well, mid-list (design document 3.2).
			parts := strings.Split(r.URL.RawQuery, "&")
			last := parts[len(parts)-1]
			if !strings.HasPrefix(last, "uuid=") {
				t.Errorf("the device registration query = %q, want it to end with the uuid", r.URL.RawQuery)
			}
			if n := strings.Count(r.URL.RawQuery, "uuid="); n != 2 {
				t.Errorf("the device registration query has %d uuid parameters, want 2: %q", n, r.URL.RawQuery)
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

// TestTheDefaultGroupIsFoundEvenWhenItIsNotARealBool says a default
// group that is written as a number or as a string is found too.
//
// is_default 到底回的是不是真的布尔，没人验过（设计文档「待实测」第
// 3 条）。认错一个组和认不出一个组，后果一样：账号批不了设备码。
func TestTheDefaultGroupIsFoundEvenWhenItIsNotARealBool(t *testing.T) {
	for _, written := range []string{`1`, `"true"`, `true`} {
		f := newFakeSite(t)
		f.groups = `{"code":0,"data":{"groups":[{"id":"g1","is_default":` + written + `}]}}`
		c := f.client(t)
		f.login(t, c)
		if err := c.Approve(context.Background(), "ABCD-1234"); err != nil {
			t.Fatalf("is_default = %s: %v", written, err)
		}
		if f.approved["group_id"] != "g1" {
			t.Fatalf("is_default = %s: group_id = %v", written, f.approved["group_id"])
		}
	}
}

// TestTheOnlyGroupIsApprovedWhenNoneIsTheDefault says a group is used
// even when none of them is marked the default, as long as there is
// only one.
//
// 没有默认组的时候，站点的前端是拒绝的（它自己会写「A default group
// is required to approve authorization.」）。但只有一个组的时候没得选：
// 站点后头收不收是站点的事，让它答，比我们先拒了好。
func TestTheOnlyGroupIsApprovedWhenNoneIsTheDefault(t *testing.T) {
	f := newFakeSite(t)
	f.groups = `{"code":0,"data":{"groups":[{"id":"g1","is_default":false}]}}`
	c := f.client(t)
	f.login(t, c)
	if err := c.Approve(context.Background(), "ABCD-1234"); err != nil {
		t.Fatal(err)
	}
	if f.approved["group_id"] != "g1" {
		t.Fatalf("group_id = %v, want the only group", f.approved["group_id"])
	}
}

// TestTheGroupIsFoundByTheNameTheSiteUses says the group id is read
// from the field the site really sends.
//
// 站点自己的代码（app/oauth-authorize 页，prod-web-va-0.1.45）：
//
//	j = e => {let i = e.filter(e => e.is_default); return 1===i.length ? i[0] : null}
//	k = (e,t,i) => {"approve"===t ? {user_code:e,decision:t,group_id:i} : ...}
//	k(t, "approve", S.group_id)
//
// 组的形状是 {group_id, name, identifier, is_default, can_authorize} ——
// 里面根本没有 id。我们照着规格里那个 {id, is_default} 去认，一个组也
// 认不出来，于是每个号都死在「没有默认组」上。
func TestTheGroupIsFoundByTheNameTheSiteUses(t *testing.T) {
	f := newFakeSite(t)
	f.groups = `{"code":0,"data":{"status":"pending","groups":[` +
		`{"group_id":"g1","name":"我的组","identifier":"my-group","is_default":true,"can_authorize":true},` +
		`{"group_id":"g2","name":"别的组","identifier":"other","is_default":false,"can_authorize":true}` +
		`]}}`
	c := f.client(t)
	f.login(t, c)
	if err := c.Approve(context.Background(), "ABCD-1234"); err != nil {
		t.Fatal(err)
	}
	if f.approved["group_id"] != "g1" {
		t.Fatalf("group_id = %v, want the default group", f.approved["group_id"])
	}
}

// TestTheOnlyGroupIsApprovedWhenItIsWrittenTheSitesWay says the one
// group is used even when it is written the way the site writes it.
func TestTheOnlyGroupIsApprovedWhenItIsWrittenTheSitesWay(t *testing.T) {
	f := newFakeSite(t)
	f.groups = `{"code":0,"data":{"groups":[{"group_id":"g1","name":"我的组","is_default":false}]}}`
	c := f.client(t)
	f.login(t, c)
	if err := c.Approve(context.Background(), "ABCD-1234"); err != nil {
		t.Fatal(err)
	}
	if f.approved["group_id"] != "g1" {
		t.Fatalf("group_id = %v, want the only group", f.approved["group_id"])
	}
}

// TestTwoDefaultGroupsAreAnError says two default groups is an error.
//
// 站点只认一个默认组：它的前端是 `1===i.length ? i[0] : null`，两个默
// 认组它自己也不知道该用哪个。我们也不猜：猜错了就是把账号批进别的组里
// 去了。
func TestTwoDefaultGroupsAreAnError(t *testing.T) {
	f := newFakeSite(t)
	f.groups = `{"code":0,"data":{"groups":[` +
		`{"group_id":"g1","is_default":true},{"group_id":"g2","is_default":true}]}}`
	c := f.client(t)
	f.login(t, c)
	err := c.Approve(context.Background(), "ABCD-1234")
	if err == nil {
		t.Fatal("no error with two default groups")
	}
	if !errors.Is(err, ErrNoDefaultGroup) {
		t.Fatalf("err = %v, want the no-default-group error", err)
	}
}

// TestSeveralGroupsWithoutADefaultAreAnError says several groups and
// no default among them is an error.
//
// 有得选的时候不猜：猜错了就是把账号批进别的组里去了。站点答的那个
// 形状得在错里：默认组是真的没有，还是我们没认出来，看一眼就知道
// （设计文档「待实测」第 3 条）。
func TestSeveralGroupsWithoutADefaultAreAnError(t *testing.T) {
	f := newFakeSite(t)
	f.groups = `{"code":0,"data":{"groups":[{"id":"g1"},{"id":"g2"}]}}`
	c := f.client(t)
	f.login(t, c)
	err := c.Approve(context.Background(), "ABCD-1234")
	if err == nil {
		t.Fatal("no error with several groups and no default")
	}
	if !errors.Is(err, ErrNoDefaultGroup) {
		t.Fatalf("err = %v, want the no-default-group error", err)
	}
	if !strings.Contains(err.Error(), `"id":"g1"`) {
		t.Fatalf("the site's answer is not in the error: %v", err)
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
