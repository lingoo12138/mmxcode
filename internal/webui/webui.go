// Package webui serves the management pages and the management API.
//
// The pages are rendered on the server; the script on them only
// changes what has to be changed. Nothing is rebuilt in the browser.
package webui

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/lingoo12138/mmxcode/internal/account"
	"github.com/lingoo12138/mmxcode/internal/config"
	"github.com/lingoo12138/mmxcode/internal/egress"
	"github.com/lingoo12138/mmxcode/internal/manager"
	"github.com/lingoo12138/mmxcode/internal/oauth"
	"github.com/lingoo12138/mmxcode/internal/provision"
	"github.com/lingoo12138/mmxcode/internal/site"
	"github.com/lingoo12138/mmxcode/internal/store"
	"github.com/lingoo12138/mmxcode/internal/usage"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static/*
var staticFS embed.FS

// pendingLogin is a sign-in that is waiting for the user.
type pendingLogin struct {
	ID        string
	UserCode  string
	URL       string
	Device    *oauth.Device
	CreatedAt time.Time
	// Err says why the sign-in did not work. It is empty while the
	// sign-in is still going on.
	//
	// It is written and read under the WebUI's lock. A string is two
	// words, and reading one of them while the other one is being
	// written gives a string that is half of one thing and half of
	// another: a pointer to nowhere with a length that is not there.
	Err string
	// FailedAt is when the sign-in failed. It is zero while the sign-in
	// is still going on.
	FailedAt time.Time
}

// defaultPendingTTL is how long a sign-in that has ended is kept.
//
// A sign-in that failed has to be kept long enough for the browser to
// be told why: the page asks about it every few seconds. After that it
// goes, so that a server that is up for a long time does not keep every
// sign-in it ever had.
const defaultPendingTTL = 10 * time.Minute

// WebUI serves the pages and the API.
type WebUI struct {
	// Mgr holds the accounts.
	Mgr *manager.Manager
	// Cfg is the configuration.
	Cfg config.Config
	// Store is the database.
	Store *store.Store
	// Import brings a batch of accounts in. It is a field so that the
	// page and the command share one task, and so that a test can
	// stand in for the network.
	Import func(ctx context.Context, entries []provision.Entry) ([]provision.Result, error)
	// Relogin signs the accounts that are signed out in again.
	Relogin func(ctx context.Context) (manager.ReloginResult, error)
	// PasswordNote says what happens to the passwords of the accounts
	// that are brought in.
	//
	// It is empty when the passwords are kept. When they are not, the
	// page has to say so: an account whose password was thrown away
	// cannot sign itself back in, and the person has to know that
	// before the account drops out.
	PasswordNote string

	tmpl    *template.Template
	mu      sync.Mutex
	pending map[string]*pendingLogin
	// pendingTTL is how long a sign-in that has ended is kept. It is a
	// field so that a test can make it short.
	pendingTTL time.Duration
}

// New makes the WebUI.
func New(mgr *manager.Manager, cfg config.Config, s *store.Store) *WebUI {
	// Only the layout is parsed here. Every page defines "content",
	// and one set of templates for all of them would keep only the
	// last page's content, so every page would render the last one.
	tmpl := template.Must(template.New("mmxcode").Funcs(template.FuncMap{"num": num}).ParseFS(templateFS, "templates/layout.html"))
	return &WebUI{
		Mgr:        mgr,
		Cfg:        cfg,
		Store:      s,
		tmpl:       tmpl,
		pending:    map[string]*pendingLogin{},
		pendingTTL: defaultPendingTTL,
	}
}

// ServeHTTP routes a request.
func (w *WebUI) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/static/app.js":
		w.serveStatic(rw, r)
		return
	case "/api/accounts":
		w.apiAccounts(rw, r)
		return
	case "/api/accounts/poll":
		w.apiPoll(rw, r)
		return
	case "/api/usage":
		w.apiUsage(rw, r)
		return
	case "/api/usage/refresh":
		w.apiUsageRefresh(rw, r)
		return
	case "/api/egress":
		w.apiEgress(rw, r)
		return
	case "/api/proxy-groups":
		w.apiProxyGroups(rw, r)
		return
	case "/api/egress/test":
		w.apiEgressTest(rw, r)
		return
	case "/api/sessions":
		w.apiSessions(rw, r)
		return
	case "/api/events":
		w.apiEvents(rw, r)
		return
	case "/api/import":
		w.apiImport(rw, r)
		return
	case "/api/relogin":
		w.apiRelogin(rw, r)
		return
	case "/api/keys":
		w.apiKeys(rw, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/keys/") {
		w.apiKey(rw, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/accounts/") {
		w.apiAccount(rw, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/import/") {
		w.apiImportRun(rw, r)
		return
	}
	switch r.URL.Path {
	case "/":
		w.page(rw, r, "accounts.html", w.accountsData)
	case "/egress":
		w.page(rw, r, "egress.html", w.egressData)
	case "/usage":
		w.page(rw, r, "usage.html", w.usageData)
	case "/login":
		w.page(rw, r, "login.html", w.loginData)
	case "/import":
		// 上传不再因为配了 key 就关着：那把 key 就是后台登录的那把，
		// 能登录的人就是管这个网关的人，明文密码走不走内网是他自己
		// 的选择。上传这个口子跟后台的其它页面一样，钥匙管着。
		w.page(rw, r, "import.html", w.importData)
	case "/relogin":
		// 重登页不接触明文密码：它登的是已经签出的账号，用的是库里
		// 存着的那一份。
		w.page(rw, r, "relogin.html", w.reloginData)
	case "/keys":
		w.page(rw, r, "keys.html", w.keysData)
	default:
		http.NotFound(rw, r)
	}
}

// num is a number that may not be there.
//
// The credits are pointers, so that an account whose ledger could not
// be read is left out rather than counted as zero. A page has to show
// a number, so the pointer is read here.
func num(v *float64) float64 {
	if v == nil {
		return 0
	}
	return *v
}

// serveStatic gives the script.
func (w *WebUI) serveStatic(rw http.ResponseWriter, r *http.Request) {
	b, err := staticFS.ReadFile("static/app.js")
	if err != nil {
		http.Error(rw, "not found", http.StatusNotFound)
		return
	}
	rw.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	rw.Write(b)
}

// page renders one page.
func (w *WebUI) page(rw http.ResponseWriter, r *http.Request, name string, data func(context.Context) (map[string]any, error)) {
	d, err := data(r.Context())
	if err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}
	d["Title"] = strings.TrimSuffix(name, ".html")
	// 配了 key 才有登录和退出可做：没配 key 的网关不用登录。
	d["HasKeys"] = len(w.Cfg.Server.APIKeys) > 0
	// The page brings its own "content", so it is parsed into a copy
	// of the layout.
	tmpl, err := w.tmpl.Clone()
	if err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := tmpl.ParseFS(templateFS, "templates/"+name); err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}
	var buf strings.Builder
	if err := tmpl.ExecuteTemplate(&buf, "layout", d); err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	rw.Write([]byte(buf.String()))
}

// accountsData is the accounts page's data.
func (w *WebUI) accountsData(ctx context.Context) (map[string]any, error) {
	accounts, err := w.Store.ListAccounts(ctx)
	if err != nil {
		return nil, err
	}
	groups, err := w.Store.ListProxyGroups(ctx)
	if err != nil {
		return nil, err
	}
	snaps, err := w.Store.LatestUsageSnapshots(ctx)
	if err != nil {
		return nil, err
	}
	byAccount := map[string]store.UsageSnapshot{}
	for _, s := range snaps {
		byAccount[s.AccountID] = s
	}
	out := make([]map[string]any, 0, len(accounts))
	for _, a := range accounts {
		snap := byAccount[a.ID]
		checkedIn, streak := w.todayCheckin(ctx, a.ID)
		out = append(out, map[string]any{
			"ID":            a.ID,
			"Name":          a.Name,
			"Email":         a.Email,
			"Status":        a.Status,
			"ProxyURL":      egress.Redacted(a.ProxyURL),
			"ProxyGroup":    a.ProxyGroup,
			"HasAmounts":    snap.HasAmounts,
			"Granted":       snap.Granted,
			"Remaining":     snap.Remaining,
			"Plan":          snap.Plan,
			"WindowsError":  snap.WindowsError,
			"WindowSummary": w.windowSummary(ctx, a.ID),
			"CheckedIn":     checkedIn,
			"Streak":        streak,
			// 账号在 error 或者 signed_out 的时候，光有状态是没用的：
			// 上一次为什么失败，才是那个人要做的事。
			"LastError": a.LastError,
		})
	}
	return map[string]any{"Accounts": out, "Groups": groupList(groups)}, nil
}

// groupList gives the groups for the page: the name and how many
// addresses it has.
func groupList(groups []store.ProxyGroup) []map[string]any {
	out := make([]map[string]any, 0, len(groups))
	for _, g := range groups {
		out = append(out, map[string]any{
			"Name":   g.Name,
			"Global": g.Name == store.GlobalGroup,
			"Count":  len(g.URLs),
		})
	}
	return out
}

// windowSummary gives a one-line summary of the account's windows.
func (w *WebUI) windowSummary(ctx context.Context, accountID string) string {
	ws, err := w.Mgr.LatestWindows(ctx, accountID)
	if err != nil || len(ws) == 0 {
		return ""
	}
	parts := make([]string, 0, len(ws))
	for _, x := range ws {
		if x.Unlimited {
			parts = append(parts, fmt.Sprintf("%s 不限", x.ModelName))
			continue
		}
		parts = append(parts, fmt.Sprintf("%s %.0f%%", x.ModelName, x.Used))
	}
	return strings.Join(parts, " · ")
}

// todayCheckin says whether the account has checked in today.
func (w *WebUI) todayCheckin(ctx context.Context, accountID string) (bool, int) {
	day := store.DayKey(time.Now())
	list, err := w.Store.ListCheckins(ctx, accountID, 5)
	if err != nil {
		return false, 0
	}
	for _, c := range list {
		if c.Day == day {
			return c.Outcome == "claimed" || c.Outcome == "done", c.Streak
		}
	}
	return false, 0
}

// egressData is the outlets page's data.
//
// 列的是在用的那些：全局默认，加上账号自己写的地址。列那些没人走的地址
// 没有意义 —— 「可用」什么也不说明。
func (w *WebUI) egressData(ctx context.Context) (map[string]any, error) {
	health, err := w.Store.EgressHealth(ctx)
	if err != nil {
		return nil, err
	}
	byURL := map[string]store.EgressHealth{}
	for _, h := range health {
		byURL[h.URL] = h
	}
	accounts, err := w.Store.ListAccounts(ctx)
	if err != nil {
		return nil, err
	}
	groups, err := w.Store.ListProxyGroups(ctx)
	if err != nil {
		return nil, err
	}
	// 组：一个组一行 —— 名字、几条地址、那些地址（去掉凭据的）。
	var groupRows []map[string]any
	for _, g := range groups {
		text := make([]string, 0, len(g.URLs))
		for _, u := range g.URLs {
			text = append(text, egress.Redacted(u))
		}
		groupRows = append(groupRows, map[string]any{
			"Name":   g.Name,
			"Global": g.Name == store.GlobalGroup,
			"Count":  len(g.URLs),
			"Text":   strings.Join(text, "\n"),
		})
	}
	// 在用的地址：从哪来、地址本身、健不健康。
	// 库里还没有全局组的时候，配置里那个全局默认就是全局组。
	gm := egress.GroupsWithDefault(egress.GroupMap(groups), w.Cfg.Proxy.Default)
	var out []map[string]any
	for _, o := range egress.Outlets(accounts, gm) {
		h, ok := byURL[o.URL]
		out = append(out, map[string]any{
			"Name": o.Name, "URL": egress.Redacted(o.URL),
			"OK": ok && h.OK, "Checked": ok, "Error": h.Error,
		})
	}
	return map[string]any{"Groups": groupRows, "Egress": out}, nil
}

// usageData is the usage page's data.
func (w *WebUI) usageData(ctx context.Context) (map[string]any, error) {
	snaps, err := w.Store.LatestUsageSnapshots(ctx)
	if err != nil {
		return nil, err
	}
	accounts, err := w.Store.ListAccounts(ctx)
	if err != nil {
		return nil, err
	}
	s := usage.Aggregate(snaps, accounts, time.Now())
	return map[string]any{"Summary": s}, nil
}

// loginData is the login page's data.
func (w *WebUI) loginData(ctx context.Context) (map[string]any, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, p := range w.pending {
		// A sign-in that failed is not waiting for anybody any more. It
		// is kept a while so that the page can say why it failed, and
		// it must not be shown as one that is still going on.
		if p.Err != "" {
			continue
		}
		return map[string]any{"Pending": map[string]any{
			"UserCode": p.UserCode, "URL": p.URL,
		}}, nil
	}
	return map[string]any{"Pending": nil}, nil
}

// apiAccounts starts a sign-in, or lists the accounts.
func (w *WebUI) apiAccounts(rw http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		d, err := w.accountsData(r.Context())
		if err != nil {
			http.Error(rw, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(rw, map[string]any{"accounts": d["Accounts"]})
	case http.MethodPost:
		d, err := w.startLogin(r.Context())
		if err != nil {
			http.Error(rw, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(rw, d)
	default:
		rw.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// startLogin asks the site for a sign-in code.
func (w *WebUI) startLogin(ctx context.Context) (map[string]any, error) {
	d, err := oauth.StartDevice(ctx, w.Mgr.Up, site.AccountURL(site.DeviceCodePath))
	if err != nil {
		return nil, err
	}
	id := fmt.Sprintf("pending_%d", time.Now().UnixNano())
	w.mu.Lock()
	w.pending[id] = &pendingLogin{
		ID: id, UserCode: d.UserCode, URL: d.URL, Device: d, CreatedAt: time.Now(),
	}
	w.mu.Unlock()
	go w.waitLogin(id, d)
	return map[string]any{
		"id": id, "user_code": d.UserCode, "verification_url": d.URL,
	}, nil
}

// waitLogin waits for the sign-in to finish.
func (w *WebUI) waitLogin(id string, d *oauth.Device) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	tok, err := d.Poll(ctx, w.Mgr.Up, site.AccountURL(site.TokenPath))
	if err != nil {
		w.failLogin(id, err.Error())
		return
	}
	now := time.Now()
	acc := store.Account{
		ID:        newAccountID(),
		Status:    store.StatusActive,
		CreatedAt: now,
		UpdatedAt: now,
	}
	// 账户是谁，要问站点本人：令牌里的那个 sub 是猜的，签到和积分
	// 明细要的是真的那个 user id。命令行就是这么做的，这里也一样。
	who, ierr := w.identity(ctx, tok)
	if ierr != nil {
		// 登录成功了，只是读不到账户信息：账号还是建起来，用令牌
		// 自己带着的东西填。
		fmt.Fprintf(os.Stderr, "webui: signed in, but the account's identity could not be read: %v\n", ierr)
	}
	// 名字不给：库里已经有这一行的时候，名字是上次那人自己写的标签，
	// 一次登录不该把它换掉。新账号的名字由库来起（地址、用户 id、令牌
	// 里带的那个 id）。
	acc.UserID = firstNonEmpty(who.RealUserID, tok.Subject)
	acc.Email = who.Email
	// 库里已经有这个站点账号了（多半是批量导入进来的）：改那一行，不再
	// 另起一行——同一个站点账号两行都是活的，轮转会挑到两次。
	acc, err = w.Store.SignIn(ctx, acc, store.Token{
		Access: tok.Access, Refresh: tok.Refresh,
		ExpiresAt: tok.Expires, Subject: tok.Subject,
	})
	if err != nil {
		// 建账号失败要说出来：不然这一次登录就悄悄没了，人只看到
		// 页面在转圈。
		w.failLogin(id, "the account could not be written down: "+err.Error())
		return
	}
	// 刚登进来的账号马上读一次账本：不等调度器那一趟
	// （usage.refresh.scan_every，默认 30 秒），人才看得到它有多少
	// 积分。读不到不算登录失败——账号是进来了的。
	w.refreshUsage(ctx, acc.ID)
	w.mu.Lock()
	delete(w.pending, id)
	w.mu.Unlock()
}

// identity reads who the account is.
//
// The token's own claims are guesses, so the site is asked. A failure
// is not fatal: the sign-in worked, and the caller falls back to what
// the token carries.
func (w *WebUI) identity(ctx context.Context, tok *oauth.Token) (account.Identity, error) {
	if w.Mgr == nil || w.Mgr.Acc == nil {
		return account.Identity{}, errors.New("no account client")
	}
	return w.Mgr.Acc.Identity(ctx, tok.Access, tok.Subject)
}

// failLogin says the sign-in did not work.
//
// The reason is kept on the entry, so that the page can be told why.
// The entry itself goes away after a while: a server that is up for a
// long time must not keep every sign-in it ever had.
//
// The entry is not taken away at once, because the browser has to be
// able to read the reason. The page asks every few seconds, and it
// stops asking once it has the answer.
func (w *WebUI) failLogin(id string, reason string) {
	w.mu.Lock()
	p := w.pending[id]
	if p != nil {
		p.Err = reason
		p.FailedAt = time.Now()
	}
	ttl := w.pendingTTL
	w.mu.Unlock()
	if p == nil {
		return
	}
	time.AfterFunc(ttl, func() {
		w.mu.Lock()
		delete(w.pending, id)
		w.mu.Unlock()
	})
}

// newAccountID makes an id for a new account.
//
// The time alone is not enough: two sign-ins that end in the same
// millisecond get the same id, and the second one is refused by the
// primary key. So the id has a random part too.
func newAccountID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		// 没有随机数的时候只能用时间：撞车的机会很小，撞上了也
		// 只是这一次登录白做。
		return fmt.Sprintf("acc_%d", time.Now().UnixMilli())
	}
	return "acc_" + hex.EncodeToString(b)
}

// firstNonEmpty gives the first value that is not empty.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// apiPoll says how a sign-in is going.
func (w *WebUI) apiPoll(rw http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"id"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	// The reason is read under the lock and copied out of it. It is
	// written under the same lock, and reading it outside would read
	// half of one string and half of another.
	w.mu.Lock()
	p := w.pending[body.ID]
	reason := ""
	if p != nil {
		reason = p.Err
	}
	w.mu.Unlock()
	if reason != "" {
		writeJSON(rw, map[string]any{"error": reason})
		return
	}
	if p == nil {
		writeJSON(rw, map[string]any{"done": true})
		return
	}
	writeJSON(rw, map[string]any{"done": false})
}

// apiUsageRefresh reads every account's credits now.
//
// 页面上那些数是上一次读数时的数。这一趟是现读的：不等调度器，也不
// 等下一次。读不到的账号要说得出来，人才知道去看哪一个。正在被别的
// 那一趟读着的账号被跳过了 —— 跳过的也要说得出来。
func (w *WebUI) apiUsageRefresh(rw http.ResponseWriter, r *http.Request) {
	res, err := w.Mgr.RefreshAllUsage(r.Context())
	if err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}
	for _, reason := range res.Reasons {
		log.Printf("usage: %s", reason)
	}
	if res.Skipped > 0 {
		log.Printf("usage: %d account(s) are being read by another round and were skipped", res.Skipped)
	}
	writeJSON(rw, map[string]any{
		"refreshed": res.Refreshed,
		"failed":    res.Failed,
		"skipped":   res.Skipped,
		"reasons":   res.Reasons,
	})
}

// apiUsage gives the credits summary.
func (w *WebUI) apiUsage(rw http.ResponseWriter, r *http.Request) {
	snaps, err := w.Store.LatestUsageSnapshots(r.Context())
	if err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}
	accounts, err := w.Store.ListAccounts(r.Context())
	if err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}
	s := usage.Aggregate(snaps, accounts, time.Now())
	writeJSON(rw, s)
}

// apiEgress gives the outlets and their health.
func (w *WebUI) apiEgress(rw http.ResponseWriter, r *http.Request) {
	d, err := w.egressData(r.Context())
	if err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(rw, d)
}

// apiProxyGroups reads and writes the outlet groups.
//
// 组在库里，页面上能改：建一个、改一个、删一个。删掉的组，指着它的账号
// 回到全局组去。
func (w *WebUI) apiProxyGroups(rw http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		w.apiProxyGroupsList(rw, r)
	case http.MethodPost:
		w.apiProxyGroupsSave(rw, r)
	case http.MethodDelete:
		w.apiProxyGroupsDelete(rw, r)
	default:
		rw.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// apiProxyGroupsList gives the groups.
func (w *WebUI) apiProxyGroupsList(rw http.ResponseWriter, r *http.Request) {
	groups, err := w.Store.ListProxyGroups(r.Context())
	if err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}
	counts, err := w.Store.AccountGroups(r.Context())
	if err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}
	out := make([]map[string]any, 0, len(groups))
	for _, g := range groups {
		out = append(out, map[string]any{
			"Name":     g.Name,
			"Global":   g.Name == store.GlobalGroup,
			"URLs":     g.URLs,
			"Accounts": counts[g.Name],
		})
	}
	writeJSON(rw, map[string]any{"Groups": out})
}

// apiProxyGroupsSave makes a group or changes it.
//
// 名字是组的身份：这个名字已经有了，就是改它的地址。
func (w *WebUI) apiProxyGroupsSave(rw http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string   `json:"name"`
		URLs []string `json:"urls"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		http.Error(rw, "the group has no name", http.StatusBadRequest)
		return
	}
	// 页面显示的是去掉凭据的地址，送回来的也是去了凭据的：没改过的那
	// 几条，存回去的是带凭据的那一个。
	var stored []string
	if old, err := w.Store.GetProxyGroup(r.Context(), name); err == nil {
		stored = old.URLs
	}
	urls := mergeAddresses(body.URLs, stored)
	now := time.Now()
	if err := w.Store.SaveProxyGroup(r.Context(), store.ProxyGroup{
		Name: name, URLs: urls, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}
	// 解析器得马上知道：网关下一次挑地址就该按新的来。
	if err := w.Mgr.RefreshOutlets(r.Context()); err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(rw, map[string]any{"ok": true})
}

// apiProxyGroupsDelete takes a group away.
func (w *WebUI) apiProxyGroupsDelete(rw http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		http.Error(rw, "the group has no name", http.StatusBadRequest)
		return
	}
	back, err := w.Store.DeleteProxyGroup(r.Context(), name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(rw, "no such group: "+name, http.StatusNotFound)
			return
		}
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}
	_ = w.Mgr.RefreshOutlets(r.Context())
	writeJSON(rw, map[string]any{"ok": true, "back": back})
}

// mergeAddresses puts the credentials back into the addresses that were
// not really changed.
//
// 页面显示的是去掉凭据的地址（凭据是秘密），送回来的也是去了凭据的。
// 没改过的那几条，存回去的是带凭据的那一个 —— 否则改一下别的地址就把凭
// 据抹了。
func mergeAddresses(shown, stored []string) []string {
	used := make([]bool, len(stored))
	out := make([]string, 0, len(shown))
	for _, s := range shown {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		matched := false
		for i, old := range stored {
			if used[i] {
				continue
			}
			if s == egress.Redacted(old) {
				out = append(out, old)
				used[i] = true
				matched = true
				break
			}
		}
		if !matched {
			out = append(out, s)
		}
	}
	return out
}

// apiEgressTest tests one outlet now.
func (w *WebUI) apiEgressTest(rw http.ResponseWriter, r *http.Request) {
	var body struct {
		URL string `json:"url"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	url, ok := w.outletURL(r.Context(), body.URL)
	if !ok {
		http.Error(rw, "no such outlet", http.StatusNotFound)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	err := w.Mgr.Egress.Check(ctx, url, "https://agent.minimax.io/")
	ok2 := err == nil
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	_ = w.Store.SetEgressHealth(r.Context(), url, ok2, msg)
	writeJSON(rw, map[string]any{"ok": ok2, "error": msg})
}

// outletURL finds the address behind the one the page shows.
//
// 页面显示的是去掉凭据的地址（凭据是秘密，不能露在页面上），那个地址发
// 回来的时候也是去了凭据的。它得对上真的那个地址才测得动。
func (w *WebUI) outletURL(ctx context.Context, shown string) (string, bool) {
	accounts, err := w.Store.ListAccounts(ctx)
	if err != nil {
		return "", false
	}
	groups, err := w.Store.ListProxyGroups(ctx)
	if err != nil {
		return "", false
	}
	gm := egress.GroupsWithDefault(egress.GroupMap(groups), w.Cfg.Proxy.Default)
	for _, o := range egress.Outlets(accounts, gm) {
		if egress.Redacted(o.URL) == shown {
			return o.URL, true
		}
	}
	return "", false
}

// apiAccount handles one account: patch, delete, usage, check-in.
func (w *WebUI) apiAccount(rw http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/accounts/")
	parts := strings.Split(rest, "/")
	id := parts[0]
	if id == "" {
		http.NotFound(rw, r)
		return
	}
	if len(parts) == 2 {
		switch parts[1] {
		case "checkin":
			w.apiCheckin(rw, r, id)
			return
		case "usage":
			w.apiAccountUsage(rw, r, id)
			return
		}
		http.NotFound(rw, r)
		return
	}
	switch r.Method {
	case http.MethodPatch:
		var body struct {
			Name       *string `json:"name"`
			ProxyURL   *string `json:"proxy_url"`
			ProxyGroup *string `json:"proxy_group"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		a, err := w.Store.GetAccount(r.Context(), id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				http.Error(rw, "no such account: "+id, http.StatusNotFound)
				return
			}
			http.Error(rw, err.Error(), http.StatusInternalServerError)
			return
		}
		if body.ProxyURL != nil && body.ProxyGroup != nil {
			// 地址和组是二选一：账号只有一条出口。
			http.Error(rw, "the outlet is an address or a group, not both", http.StatusBadRequest)
			return
		}
		if body.ProxyURL != nil {
			// The page shows the address without its credentials, so
			// the address that comes back unchanged is the one that
			// is stored, and not the redacted one. An address the
			// user really changed is written as it is.
			shown := egress.Redacted(a.ProxyURL)
			if *body.ProxyURL != shown || shown == a.ProxyURL {
				a.ProxyURL = *body.ProxyURL
			}
			// 给了地址，组就清掉：账号只有一条出口。
			a.ProxyGroup = ""
		}
		if body.ProxyGroup != nil {
			a.ProxyGroup = strings.TrimSpace(*body.ProxyGroup)
			a.ProxyURL = ""
		}
		// 名字和出口分开写。整行写回去会把别的写者在这中间改的东西抹掉：
		// 管理器刚写下的登出，会被这一次改名字抹回 active。
		if body.Name != nil && *body.Name != a.Name {
			if err := w.Store.SetAccountName(r.Context(), a.ID, *body.Name, time.Now()); err != nil {
				http.Error(rw, err.Error(), http.StatusInternalServerError)
				return
			}
		}
		if body.ProxyURL != nil || body.ProxyGroup != nil {
			if err := w.Store.SetAccountOutlet(r.Context(), a.ID, a.ProxyURL, a.ProxyGroup, time.Now()); err != nil {
				http.Error(rw, err.Error(), http.StatusInternalServerError)
				return
			}
		}
		writeJSON(rw, map[string]any{"ok": true})
	case http.MethodDelete:
		if err := w.Store.DeleteAccount(r.Context(), id); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				http.Error(rw, "no such account: "+id, http.StatusNotFound)
				return
			}
			http.Error(rw, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(rw, map[string]any{"ok": true})
	default:
		rw.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// apiCheckin claims one account's credits.
func (w *WebUI) apiCheckin(rw http.ResponseWriter, r *http.Request, id string) {
	out, err := w.Mgr.Checkin(r.Context(), id)
	if err != nil {
		http.Error(rw, err.Error(), http.StatusBadGateway)
		return
	}
	// 签到给的积分是账本池里的：读完这一账号的额度，卡上的余额才是
	// 签到之后的数。人在等，所以只读这一个。
	round := usage.NewRound(w.Store, w.Mgr, w.Mgr.HotAccounts, w.Cfg)
	if _, err := round.Settle(r.Context(), id); err != nil {
		fmt.Printf("checkin %s: the credits could not be read: %v\n", id, err)
	}
	writeJSON(rw, map[string]any{
		"outcome": out.Outcome, "credit": out.Credit,
		"streak": out.Streak, "message": out.Message,
	})
}

// apiAccountUsage gives one account's credits.
func (w *WebUI) apiAccountUsage(rw http.ResponseWriter, r *http.Request, id string) {
	snap, err := w.Mgr.RefreshUsage(r.Context(), id)
	if err != nil {
		http.Error(rw, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(rw, snap)
}

// apiSessions gives the session bindings.
func (w *WebUI) apiSessions(rw http.ResponseWriter, r *http.Request) {
	writeJSON(rw, map[string]any{"sessions": []any{}})
}

// apiEvents pushes the state changes as they happen.
func (w *WebUI) apiEvents(rw http.ResponseWriter, r *http.Request) {
	rw.Header().Set("Content-Type", "text/event-stream")
	rw.Header().Set("Cache-Control", "no-cache")
	rw.WriteHeader(http.StatusOK)
	f, ok := rw.(http.Flusher)
	if !ok {
		return
	}
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			fmt.Fprint(rw, ": ping\n\n")
			f.Flush()
		}
	}
}

// writeJSON writes a JSON answer.
func writeJSON(rw http.ResponseWriter, v any) {
	writeJSONStatus(rw, http.StatusOK, v)
}

// writeJSONStatus writes a JSON answer with a status code.
//
// The header is set before the status is written: after WriteHeader the
// header map is frozen, and a Content-Type set afterwards never reaches
// the client. Go sniffs the body instead and says text/plain, and the
// page's script then cannot read the answer — it reads nothing, and the
// upload looks like it failed even though it went through.
func writeJSONStatus(rw http.ResponseWriter, status int, v any) {
	rw.Header().Set("Content-Type", "application/json; charset=utf-8")
	rw.WriteHeader(status)
	json.NewEncoder(rw).Encode(v)
}

// maxImportBytes is how big an uploaded list may be.
//
// 一行一个账号，八兆字节已经是很长的一份清单了。
const maxImportBytes = 8 << 20

// importData is the import page's data.
func (w *WebUI) importData(ctx context.Context) (map[string]any, error) {
	runs, err := w.Store.ListImportRuns(ctx, 10)
	if err != nil {
		return nil, err
	}
	views := make([]importRunView, 0, len(runs))
	for _, r := range runs {
		views = append(views, importRunView{ImportRun: r, Whole: wholeReason(r.Results)})
	}
	return map[string]any{
		"Runs":         views,
		"PasswordNote": w.PasswordNote,
		"TextLines":    maxImportTextLines,
	}, nil
}

// importRunView is one import run as the page shows it.
//
// 整趟没成的导入，成功和失败都是零：光看那两个数，看不出这一趟出了什
// 么事，像是这一趟什么都没干。理由得摆在眼前。
type importRunView struct {
	store.ImportRun
	// Whole is why the run failed as a whole, when it did.
	Whole string
}

// wholeReason gives the reason a run failed as a whole.
//
// 整趟没成的时候，结果里有一行是整趟的（whole），它说的就是这一趟为什
// 么没成。那一行不是账号：失败的账号数是零，可这一趟确实没成，得说。
func wholeReason(results string) string {
	if strings.TrimSpace(results) == "" {
		return ""
	}
	var list []provision.Result
	if err := json.Unmarshal([]byte(results), &list); err != nil {
		return ""
	}
	for _, r := range list {
		if r.Whole {
			return r.Message
		}
	}
	return ""
}

// apiImport takes a list of accounts and brings them in.
//
// 两种交法，交的是同一份清单（每行 邮箱|密码）：
//
//   - 上传一个文件（multipart 的 file 那一栏）
//   - 贴一段清单（JSON 的 {"list": "…"}）
//
// The work goes on in the background, and the answer says which run to
// watch.
//
// The list is read into memory and thrown away afterwards: it holds
// passwords in the clear, and it must not land on the disk.
//
// 配了 key 也不关这个口子了：那把 key 就是后台登录的那把，能进来的人
// 就是管这个网关的人。明文密码走不走内网，是他自己的选择。
func (w *WebUI) apiImport(rw http.ResponseWriter, r *http.Request) {
	if w.Import == nil {
		http.Error(rw, "这个网关没有批量导入", http.StatusNotImplemented)
		return
	}
	var (
		entries []provision.Entry
		bad     []provision.BadLine
	)
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		// 贴过来的一段清单。
		var body struct {
			List string `json:"list"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(rw, "这个请求不是一段清单", http.StatusBadRequest)
			return
		}
		if n := countLines(body.List); n > maxImportTextLines {
			http.Error(rw, fmt.Sprintf("这段清单有 %d 行，最多 %d 行", n, maxImportTextLines),
				http.StatusBadRequest)
			return
		}
		entries, bad = provision.Parse(strings.NewReader(body.List))
	} else {
		// 上传的一个文件。
		//
		// The request has a size limit, and the memory that
		// ParseMultipartForm is given is bigger than it: the list
		// therefore never reaches a temporary file, it stays in
		// memory.
		r.Body = http.MaxBytesReader(rw, r.Body, maxImportBytes)
		if err := r.ParseMultipartForm(maxImportBytes * 2); err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				http.Error(rw, "这个清单太大了", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(rw, "这个请求不是文件上传", http.StatusBadRequest)
			return
		}
		file, _, err := r.FormFile("file")
		if err != nil {
			http.Error(rw, "没有文件", http.StatusBadRequest)
			return
		}
		defer file.Close()
		entries, bad = provision.Parse(file)
	}
	for _, b := range bad {
		// 坏行的那句话里没有密码：清单的解析器把密码摘掉了。
		log.Printf("import: 第 %d 行：%s", b.Line, b.Reason)
	}
	if len(entries) == 0 {
		http.Error(rw, "清单里没有能用的账号", http.StatusBadRequest)
		return
	}
	runID := newImportRunID()
	now := time.Now()
	if err := w.Store.CreateImportRun(r.Context(), store.ImportRun{
		ID: runID, Source: "web", StartedAt: now, Total: len(entries),
	}); err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}
	go w.runImport(runID, entries)
	// 202：导入在后台跑着，这个回答只说这一次导入的号。
	writeJSONStatus(rw, http.StatusAccepted, map[string]any{"id": runID})
}

// maxImportTextLines is how many lines a pasted list may have.
//
// 贴的那一段是给人手写的那一份：一百行够写一百个账号，再多就该上传一
// 个文件了。上传的那一份是批量那一份，它自己的大小上限是
// maxImportBytes，行数不管。
const maxImportTextLines = 100

// countLines counts the lines of a pasted list.
//
// 末尾那几个空行不算：粘贴的时候多带一个换行是常事，那不该把一行算
// 成两行。
func countLines(text string) int {
	text = strings.TrimRight(text, "\r\n")
	if text == "" {
		return 0
	}
	return strings.Count(text, "\n") + 1
}

// newImportRunID makes the name of one import run.
//
// 时间还不够：两次上传可能落在同一毫秒里，那样的第二个会被主键挡下。
func newImportRunID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("run_%d", time.Now().UnixMilli())
	}
	return fmt.Sprintf("run_%d_%s", time.Now().UnixMilli(), hex.EncodeToString(b))
}

// runImport brings a batch in and writes down what came of it.
//
// A run that fails as a whole says why, in two places: in the log, and
// in the results the page shows. The run has no column of its own for
// it, so the reason goes in as a row of its own — otherwise the page
// says only "failed 1" and the operator never learns that the key was
// wrong.
//
// 那一行不是账号，所以它不算进失败的账号数里：一个账号都没处理的这一趟，
// 失败的账号数是零。整趟为什么没成，由那一行自己说。
func (w *WebUI) runImport(runID string, entries []provision.Entry) {
	ctx := context.Background()
	results, err := w.Import(ctx, entries)
	if err != nil {
		log.Printf("import %s: %v", runID, err)
		results = append(results, provision.Result{
			Email:   "（整趟）",
			OK:      false,
			Whole:   true,
			Stage:   stageOf(err),
			Message: "整趟导入没能完成：" + err.Error(),
		})
	}
	ok := 0
	failed := 0
	for _, r := range results {
		switch {
		case r.OK:
			ok++
			// 刚进来的账号马上读一次账本：不等调度器那一趟（15 分
			// 钟），人才看得到它有多少积分。
			w.refreshUsage(ctx, r.AccountID)
		case !r.Whole:
			// 整趟没成的那一行不是账号：它不算进失败的账号数里。
			failed++
		}
	}
	if err := w.Store.FinishImportRun(ctx, runID, time.Now(), ok, failed, provision.ResultsJSON(results)); err != nil {
		log.Printf("import %s: %v", runID, err)
	}
}

// refreshUsage reads one account's credits now.
//
// 读不到不算导入失败：账号是进来了的，账本读不到是另一件事。它要
// 记下来，别让它悄悄没掉。
func (w *WebUI) refreshUsage(ctx context.Context, accountID string) {
	if w.Mgr == nil || accountID == "" {
		return
	}
	if _, err := w.Mgr.RefreshUsage(ctx, accountID); err != nil {
		log.Printf("import: %s 的积分读不到：%v", accountID, err)
	}
}

// stageOf says where an import failed, when it says.
//
// 整趟失败的时候，失败在哪儿也是话的一部分：错在哪一步，人要看得见。
func stageOf(err error) provision.Stage {
	var pe *provision.Error
	if errors.As(err, &pe) {
		return pe.Stage
	}
	return ""
}

// apiRelogin signs the accounts that are signed out in again.
//
// 理由要带回去：只说「失败 2」，人得自己去一个一个看是哪个账号、为什么。
// 这一趟根本没跑的时候也要说，不能让它看着像「没什么要登的」。
func (w *WebUI) apiRelogin(rw http.ResponseWriter, r *http.Request) {
	if w.Relogin == nil {
		http.Error(rw, "这个网关没有重登", http.StatusNotImplemented)
		return
	}
	res, err := w.Relogin(r.Context())
	if err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(rw, map[string]any{
		"fixed":   res.Fixed,
		"failed":  res.Failed,
		"reasons": res.Reasons,
	})
}

// apiImportRun says how one batch import is going.
func (w *WebUI) apiImportRun(rw http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/import/")
	run, err := w.Store.GetImportRun(r.Context(), id)
	if err != nil {
		http.Error(rw, "没有这一次导入", http.StatusNotFound)
		return
	}
	writeJSON(rw, map[string]any{
		"id":          run.ID,
		"source":      run.Source,
		"total":       run.Total,
		"ok":          run.OK,
		"failed":      run.Failed,
		"results":     run.Results,
		"finished_at": !run.FinishedAt.IsZero(),
	})
}

// reloginData is the relogin page's data.
//
// Only the accounts that are signed out and still have a password are
// listed: the others cannot be signed in again, and saying so is
// better than a button that cannot work.
func (w *WebUI) reloginData(ctx context.Context) (map[string]any, error) {
	accounts, err := w.Store.ListAccounts(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0)
	for _, a := range accounts {
		if a.Status != store.StatusSignedOut {
			continue
		}
		cipher, err := w.Store.Password(ctx, a.ID)
		if err != nil || cipher == "" {
			continue
		}
		// 存着的密码是加密的，可它也不该到页面上：这里只说这个账号
		// 还能登回去。
		out = append(out, map[string]any{
			"ID": a.ID, "Name": a.Name, "Email": a.Email, "Error": a.LastError,
		})
	}
	return map[string]any{"SignedOut": out}, nil
}
