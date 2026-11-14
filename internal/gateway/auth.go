package gateway

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"html/template"
	"net/http"
	"strings"
)

// sessionCookie is the name of the cookie the browser carries.
//
// 网关的页面要过 auth，而浏览器不会替页面带上 x-api-key。这个 cookie
// 就是浏览器能带的那张票。
const sessionCookie = "mmxcode_session"

// sessionContext is what the cookie's value is computed over.
//
// 它是固定的一串字：同一把 key 永远算出同一个值，换一把 key 就换一个
// 值。
const sessionContext = "mmxcode-webui-session-v1"

// sessionValue gives the cookie's value for one key.
//
// 值是 key 的带钥匙的哈希：HMAC-SHA256(key = API key, message = 上面
// 那串固定的字)。
//
// 这么做的取舍是：服务端不用存任何东西，转一把 key 旧的 cookie 自己
// 就作废；代价是这个 cookie 本身就是一张票，谁拿到它谁就能用这个网关。
// 所以它是 HttpOnly（脚本读不到它），也是 SameSite=Strict（别的站点
// 的表单带不上它）。
func sessionValue(apiKey string) string {
	mac := hmac.New(sha256.New, []byte(apiKey))
	mac.Write([]byte(sessionContext))
	return hex.EncodeToString(mac.Sum(nil))
}

// auth checks the request's key.
//
// The key comes in the header, the way it always did, or in the cookie
// the browser carries.
//
// 没有配 key 的时候网关不查：那时候它只该听在本机，serve 那一条命令
// 管这件事。
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
	if _, ok := matchingKey(given, keys); ok {
		return true
	}
	// 浏览器带来的 cookie：值是某一把配好的 key 算出来的
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	for _, k := range keys {
		if strings.TrimSpace(k) == "" {
			// 空的 key 算不出票来：它不该是谁的票。
			continue
		}
		if subtle.ConstantTimeCompare([]byte(c.Value), []byte(sessionValue(k))) == 1 {
			return true
		}
	}
	return false
}

// matchingKey gives the configured key the given one matches.
//
// 空白的 key 不算一把 key。配置里有一把空的时候，一个不带 key 的请求
// 带来的也是空串，两边一比就是"相等"——那等于没有认证，一份配错的配置
// 就把门打开了。所以空的配置项和空的请求都挡在这里：配置说什么都不
// 算数。
func matchingKey(given string, keys []string) (string, bool) {
	if strings.TrimSpace(given) == "" {
		return "", false
	}
	for _, k := range keys {
		if strings.TrimSpace(k) == "" {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(given), []byte(k)) == 1 {
			return k, true
		}
	}
	return "", false
}

// session handles the sign-in cookie.
//
// POST gives the cookie, DELETE takes it away. Neither needs a session:
// signing in is how one gets one, and signing out has to work even when
// the cookie is already gone.
func (s *Server) session(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		s.signIn(w, r)
	case http.MethodDelete:
		s.signOut(w, r)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// signIn takes the key and gives the cookie.
//
// The key comes from the form the browser posts, or from a JSON body a
// script sends. The page that comes back never carries the key: it is a
// secret, and the page might sit in a cache.
//
// A script gets JSON and the cookie, not a page: a redirect would send
// it to a page it cannot use. The browser gets the page.
func (s *Server) signIn(w http.ResponseWriter, r *http.Request) {
	keys := s.Cfg.Server.APIKeys
	if len(keys) == 0 {
		writeError(w, protocolOf(r.URL.Path), http.StatusBadRequest, "this gateway has no api key configured")
		return
	}
	key := submittedKey(r)
	// 空 key 进不来：登录是另一扇门，空的那把不该把门打开。
	if k, ok := matchingKey(key, keys); ok {
		s.setSessionCookie(w, r, k)
		w.Header().Set("Cache-Control", "no-store")
		if wantsJSON(r) {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"ok":true}`))
			return
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	// 错的 key：不发 cookie
	if wantsJSON(r) {
		writeError(w, protocolOf(r.URL.Path), http.StatusUnauthorized, "the key was refused")
		return
	}
	s.signInPage(w, r, http.StatusUnauthorized, "the key was refused")
}

// wantsJSON says the caller wants an answer in JSON rather than a page.
//
// 浏览器交表单，脚本交 JSON。分清楚了，脚本才拿得到 JSON：它要是
// 跟着重定向走，拿回来的是一页 HTML，那对它没有用。
func wantsJSON(r *http.Request) bool {
	return strings.HasPrefix(r.Header.Get("Content-Type"), "application/json")
}

// signOut takes the cookie away.
func (s *Server) signOut(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
}

// setSessionCookie gives the browser the cookie.
//
// The value is computed from the key, so the server keeps nothing.
func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, apiKey string) {
	c := &http.Cookie{
		Name:     sessionCookie,
		Value:    sessionValue(apiKey),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	}
	// 只有走 TLS 的连接才标 Secure：网关常常是局域网里的 http，标了
	// Secure 的 cookie 浏览器根本不会带回来。
	if r.TLS != nil {
		c.Secure = true
	}
	http.SetCookie(w, c)
}

// submittedKey reads the key out of the request.
//
// 表单和 JSON 都认：浏览器发的是表单，脚本发的多半是 JSON。
func submittedKey(r *http.Request) string {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var body struct {
			Key string `json:"key"`
		}
		if json.NewDecoder(r.Body).Decode(&body) == nil {
			return strings.TrimSpace(body.Key)
		}
		return ""
	}
	if err := r.ParseForm(); err != nil {
		return ""
	}
	return strings.TrimSpace(r.PostFormValue("key"))
}

// signInPage is the page the browser signs in on.
//
// It is a page and not a JSON answer: the browser asked for a page. The
// key never comes back into it, and it is not to be cached: a key left in
// a shared machine's cache is a key left behind.
//
// The status says whether the page is the one the browser was after (200)
// or the answer to a key that was refused (401).
func (s *Server) signInPage(w http.ResponseWriter, r *http.Request, status int, errMsg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	signInTemplate.Execute(w, map[string]any{
		"Error":   errMsg,
		"HasKeys": len(s.Cfg.Server.APIKeys) > 0,
	})
}

// signInTemplate is the page the browser signs in on.
var signInTemplate = template.Must(template.New("signin").Parse(signInHTML))

// signInHTML is the sign-in page.
//
// 表单里只有一个框：把 key 填进去。没有 key 的网关不需要登录，所以
// 那时候页面只说一句。
const signInHTML = `<!DOCTYPE html>
<html lang="zh">
<head>
<meta charset="utf-8">
<title>登录 · mmxcode</title>
<style>
:root { color-scheme: light dark; }
body { font: 14px/1.6 system-ui, sans-serif; margin: 0; padding: 2rem 1rem; }
h1 { font-size: 1.1rem; }
form { display: flex; gap: .5rem; align-items: center; max-width: 32rem; }
input { flex: 1; padding: .4rem; }
button { padding: .25rem .8rem; cursor: pointer; }
.bad { color: #c0392b; }
</style>
</head>
<body>
<h1>mmxcode</h1>
{{if .HasKeys}}
<p>这个网关要一把 API key。填进去就能进管理页。</p>
<form method="post" action="/api/session">
<input type="password" name="key" placeholder="API key" autocomplete="current-password" autofocus>
<button type="submit">进入</button>
</form>
{{if .Error}}<p class="bad">{{.Error}}</p>{{end}}
{{else}}
<p>这个网关没有配 API key，不用登录。</p>
{{end}}
</body>
</html>
`

// isAPIPath says the path is one the API clients use.
//
// 脚本和模型客户端读得懂 JSON，读不懂一个登录页；浏览器打开的是页面，
// 给它登录页才对。
func isAPIPath(path string) bool {
	for _, prefix := range []string{"/v1", "/api"} {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
}
