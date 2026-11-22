package gateway

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"html/template"
	"net/http"
	"strings"

	"github.com/lingoo12138/mmxcode/internal/apikey"
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
	return s.authWith(r, keys)
}

// authWith checks the request's key against the given keys.
//
// 头里的 key 和浏览器带的 cookie 都认。
func (s *Server) authWith(r *http.Request, keys []string) bool {
	if _, ok := matchingKey(givenKey(r), keys); ok {
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

// givenKey gives the key the request carries.
//
// 两个头都认：x-api-key，和 Authorization: Bearer。
func givenKey(r *http.Request) string {
	given := strings.TrimSpace(r.Header.Get("x-api-key"))
	if given != "" {
		return given
	}
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	}
	return ""
}

// authChat checks the request's key for the chat API.
//
// 两把 key 都认：配置里那把（后台登录用的），和在后台申请的那把（给
// 别的机器聊天用的）。
//
// 一把都没有的时候不鉴权：那时候网关只该听本机。申请过就不一样了：
// 那是有人打算从别的机器用它。
func (s *Server) authChat(r *http.Request) bool {
	keys := s.Cfg.Server.APIKeys
	if len(keys) == 0 && !s.hasMadeKeys(r.Context()) {
		return true
	}
	// 配置里那把，和浏览器带的 cookie。
	if s.authWith(r, keys) {
		return true
	}
	given := givenKey(r)
	if given == "" || s.Store == nil {
		return false
	}
	// 库里存的是哈希：请求带来的 key 算一次哈希，对上了就是那一把。
	// 撤销过的不算——它已经不能开门了。
	_, err := s.Store.APIKeyByHash(r.Context(), apikey.Hash(given))
	return err == nil
}

// hasMadeKeys says whether any key has been made in the console.
func (s *Server) hasMadeKeys(ctx context.Context) bool {
	if s.Store == nil {
		return false
	}
	have, err := s.Store.HasAPIKeys(ctx)
	return err == nil && have
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
//
// 这一页自己带样式：它在管理页之前就出现了，那会儿管理页的样式还
// 没有被请来（也不该请）。
const signInHTML = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="light dark">
<title>登录 · mmxcode</title>
<style>
/* 这一页自己带样式：它在管理页之前就出现了，那会儿管理页的样式还
   没有被请来（也不该请）。可它的颜色跟管理页是同一套：那边换了，
   这边也得跟着换，不然人从登录页进后台，像走了两个地方。 */
:root {
  color-scheme: light dark;
  --bg: #f1f3f7;
  --surface: #ffffff;
  --line: #e5e8ee;
  --line-strong: #d0d6df;
  --line-control: #7f8794;
  --text: #0f1522;
  --text-2: #4c5666;
  --text-3: #5f6a7a;
  --accent: #1f5fd6;
  --accent-hover: #1a4fb5;
  --accent-ink: #ffffff;
  --bad: #c62f38;
  --bad-soft: #fdecec;
  --shadow: 0 1px 2px rgba(16, 24, 40, .04), 0 8px 24px -12px rgba(16, 24, 40, .18);
}
@media (prefers-color-scheme: dark) {
  :root {
    --bg: #0a0e14;
    --surface: #121821;
    --line: #222c38;
    --line-strong: #35414f;
    --line-control: #687382;
    --text: #e8edf4;
    --text-2: #a5b0be;
    --text-3: #8b97a7;
    --accent: #5b93ff;
    --accent-hover: #7ba6ff;
    --accent-ink: #06101f;
    --bad: #ff7b72;
    --bad-soft: #2e1618;
    --shadow: 0 8px 28px -12px rgba(0, 0, 0, .8);
  }
}
/* 人自己挑的那个模式说了算：管理页那边也是这么记的。 */
:root[data-theme="light"] {
  --bg: #f1f3f7;
  --surface: #ffffff;
  --line: #e5e8ee;
  --line-strong: #d0d6df;
  --line-control: #7f8794;
  --text: #0f1522;
  --text-2: #4c5666;
  --text-3: #5f6a7a;
  --accent: #1f5fd6;
  --accent-hover: #1a4fb5;
  --accent-ink: #ffffff;
  --bad: #c62f38;
  --bad-soft: #fdecec;
  --shadow: 0 1px 2px rgba(16, 24, 40, .04), 0 8px 24px -12px rgba(16, 24, 40, .18);
}
:root[data-theme="dark"] {
  --bg: #0a0e14;
  --surface: #121821;
  --line: #222c38;
  --line-strong: #35414f;
  --line-control: #687382;
  --text: #e8edf4;
  --text-2: #a5b0be;
  --text-3: #8b97a7;
  --accent: #5b93ff;
  --accent-hover: #7ba6ff;
  --accent-ink: #06101f;
  --bad: #ff7b72;
  --bad-soft: #2e1618;
  --shadow: 0 8px 28px -12px rgba(0, 0, 0, .8);
}
* { box-sizing: border-box; }
body {
  margin: 0;
  min-height: 100dvh;
  display: grid;
  place-items: center;
  padding: 24px;
  background: var(--bg);
  color: var(--text);
  font: 14px/1.55 -apple-system, BlinkMacSystemFont, "Segoe UI", "PingFang SC",
    "Microsoft YaHei", system-ui, sans-serif;
}
.box {
  width: min(400px, 100%);
  padding: 26px;
  border: 1px solid var(--line);
  border-radius: 6px;
  background: var(--surface);
  box-shadow: var(--shadow);
}
.brand { display: flex; align-items: center; gap: 9px; font-weight: 650; letter-spacing: -.02em; }
.mark {
  display: grid; place-items: center;
  width: 28px; height: 28px; border-radius: 7px;
  background: var(--accent); color: var(--accent-ink);
  font-size: 12px; font-weight: 700;
}
h1 { margin: 18px 0 6px; font-size: 19px; letter-spacing: -.015em; }
p { margin: 0 0 18px; color: var(--text-2); font-size: 13px; }
label { display: block; margin-bottom: 6px; color: var(--text-2); font-size: 12px; font-weight: 600; }
form { display: flex; flex-direction: column; gap: 8px; }
input {
  font: inherit; font-size: 13px; color: var(--text);
  background: var(--surface);
  border: 1px solid var(--line-control); border-radius: 6px; padding: 8px 10px;
}
/* 聚焦的那一圈得看得见：键盘按 Tab 的时候，那道实线告诉人焦点在哪儿。 */
input:focus { border-color: var(--accent); box-shadow: 0 0 0 3px rgba(47, 111, 235, .16); }
input:focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; }
button {
  font: inherit; font-size: 13px; font-weight: 600;
  padding: 8px 14px; cursor: pointer;
  border: 1px solid var(--accent); border-radius: 6px;
  background: var(--accent); color: var(--accent-ink);
}
button:hover { background: var(--accent-hover); border-color: var(--accent-hover); }
:focus-visible { outline: 2px solid var(--accent); outline-offset: 1px; }
.bad {
  margin: 16px 0 0; padding: 9px 11px; border-radius: 4px;
  background: var(--bad-soft); color: var(--bad); font-size: 13px;
}
/* 主题按钮：跟管理页上那个是同一件事，同一个记法。 */
.theme {
  position: fixed; top: 14px; right: 14px;
  display: grid; place-items: center;
  width: 34px; height: 34px; padding: 0;
  border: 1px solid var(--line); border-radius: 6px;
  background: var(--surface); color: var(--text-2); cursor: pointer;
}
.theme:hover { border-color: var(--line-strong); color: var(--text); }
.theme svg { width: 18px; height: 18px; }
</style>
<script>
// 主题得在第一帧之前定下来：不然深色的人会看见一帧白。
(function () {
  try {
    var t = localStorage.getItem('mmxcode-theme');
    if (t === 'light' || t === 'dark') {
      document.documentElement.setAttribute('data-theme', t);
    }
  } catch (e) {
    /* 存不下就跟着系统走 */
  }
})();
</script>
</head>
<body>
<button type="button" class="theme" id="theme-toggle" title="换个深浅" aria-label="换个深浅">
  <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"
       stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
    <g data-mode="auto">
      <path d="M9.173 14.83a4 4 0 1 1 5.657 -5.657"/>
      <path d="M11.294 12.707l.174 .247a7.5 7.5 0 0 0 8.845 2.492a9 9 0 1 1 -14.671 2.914"/>
      <path d="M3 12h1"/><path d="M12 3v1"/><path d="M5.6 5.6l.7 .7"/><path d="M3 21l18 -18"/>
    </g>
    <g data-mode="light" hidden>
      <path d="M12 12m-4 0a4 4 0 1 0 8 0a4 4 0 1 0 -8 0"/>
      <path d="M3 12h1m8 -9v1m8 8h1m-9 8v1m-6.4 -15.4l.7 .7m12.1 -.7l-.7 .7m0 11.4l.7 .7m-12.1 -.7l-.7 .7"/>
    </g>
    <g data-mode="dark" hidden>
      <path d="M12 3c.132 0 .263 0 .393 0a7.5 7.5 0 0 0 7.92 12.446a9 9 0 1 1 -8.313 -12.454z"/>
    </g>
  </svg>
</button>
<div class="box">
  <div class="brand"><span class="mark">mx</span>mmxcode</div>
  {{if .HasKeys}}
  <h1>进管理页</h1>
  <p>这个网关要一把 API key。填进去就能进管理页。</p>
  <form method="post" action="/api/session">
  <label for="key">API key</label>
  <input id="key" type="password" name="key" placeholder="sk-..." autocomplete="current-password" autofocus>
  <button type="submit">进入</button>
  </form>
  {{if .Error}}<p class="bad">{{.Error}}</p>{{end}}
  {{else}}
  <h1>不用登录</h1>
  <p>这个网关没有配 API key，不用登录。</p>
  {{end}}
</div>
<script>
// 主题：跟管理页上那个是同一件事，同一个记法（跟着系统、浅色、深色）。
//
// 图标是 Tabler 的官方路径（MIT），跟管理页那一套是同一份，所以两处摆出
// 来的是同一个样子。
(function () {
  var button = document.getElementById('theme-toggle');
  if (!button) return;
  var order = ['', 'light', 'dark'];
  var names = { '': '跟着系统', light: '浅色', dark: '深色' };
  var current = '';
  try {
    current = localStorage.getItem('mmxcode-theme') || '';
  } catch (e) {
    current = '';
  }
  if (order.indexOf(current) < 0) current = '';
  var paint = function () {
    // 存着的那个值是 ''（跟着系统），图标的分组是 'auto'：对一下。
    var mode = current === '' ? 'auto' : current;
    for (var g of button.querySelectorAll('g[data-mode]')) {
      g.hidden = g.dataset.mode !== mode;
    }
    button.title = '主题：' + names[current] + '（按一下换一个）';
    button.setAttribute('aria-label', '主题：' + names[current]);
  };
  paint();
  button.addEventListener('click', function () {
    current = order[(order.indexOf(current) + 1) % order.length];
    try {
      localStorage.setItem('mmxcode-theme', current);
    } catch (e) {
      /* 存不下也就算了 */
    }
    if (current) document.documentElement.setAttribute('data-theme', current);
    else document.documentElement.removeAttribute('data-theme');
    paint();
  });
})();
</script>
</body>
</html>
`

// isChatPath says the path is one of the chat API's.
//
// 聊天那扇门和管理页那扇门不一样：聊天认两把 key，管理页只认一把。
func isChatPath(path string) bool {
	return path == "/v1" || strings.HasPrefix(path, "/v1/")
}

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
