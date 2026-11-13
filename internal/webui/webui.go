// Package webui serves the management pages and the management API.
//
// The pages are rendered on the server; the script on them only
// changes what has to be changed. Nothing is rebuilt in the browser.
package webui

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"sync"
	"time"

	"mmxcode/internal/config"
	"mmxcode/internal/egress"
	"mmxcode/internal/manager"
	"mmxcode/internal/oauth"
	"mmxcode/internal/site"
	"mmxcode/internal/store"
	"mmxcode/internal/usage"
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
	Err       string
}

// WebUI serves the pages and the API.
type WebUI struct {
	// Mgr holds the accounts.
	Mgr *manager.Manager
	// Cfg is the configuration.
	Cfg config.Config
	// Store is the database.
	Store *store.Store

	tmpl    *template.Template
	mu      sync.Mutex
	pending map[string]*pendingLogin
}

// New makes the WebUI.
func New(mgr *manager.Manager, cfg config.Config, s *store.Store) *WebUI {
	// Only the layout is parsed here. Every page defines "content",
	// and one set of templates for all of them would keep only the
	// last page's content, so every page would render the last one.
	tmpl := template.Must(template.New("mmxcode").Funcs(template.FuncMap{"num": num}).ParseFS(templateFS, "templates/layout.html"))
	return &WebUI{
		Mgr:     mgr,
		Cfg:     cfg,
		Store:   s,
		tmpl:    tmpl,
		pending: map[string]*pendingLogin{},
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
	case "/api/egress":
		w.apiEgress(rw, r)
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
	}
	if strings.HasPrefix(r.URL.Path, "/api/accounts/") {
		w.apiAccount(rw, r)
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
	snaps, err := w.Store.LatestUsageSnapshots(ctx)
	if err != nil {
		return nil, err
	}
	byAccount := map[string]store.UsageSnapshot{}
	for _, s := range snaps {
		byAccount[s.AccountID] = s
	}
	groups := w.groupNames()
	out := make([]map[string]any, 0, len(accounts))
	for _, a := range accounts {
		snap := byAccount[a.ID]
		checkedIn, streak := w.todayCheckin(ctx, a.ID)
		out = append(out, map[string]any{
			"ID":            a.ID,
			"Name":          a.Name,
			"Email":         a.Email,
			"Status":        a.Status,
			"ProxyGroup":    a.ProxyGroup,
			"ProxyURL":      egress.Redacted(a.ProxyURL),
			"Current":       a.ProxyGroup,
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
	return map[string]any{"Accounts": out, "Groups": groups}, nil
}

// groupNames gives the outlet groups for the drop-down.
func (w *WebUI) groupNames() []map[string]any {
	var out []map[string]any
	for name := range w.Cfg.Proxy.Groups {
		out = append(out, map[string]any{"Name": name})
	}
	out = append(out, map[string]any{"Name": "direct"})
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
func (w *WebUI) egressData(ctx context.Context) (map[string]any, error) {
	health, err := w.Store.EgressHealth(ctx)
	if err != nil {
		return nil, err
	}
	byURL := map[string]store.EgressHealth{}
	for _, h := range health {
		byURL[h.URL] = h
	}
	var out []map[string]any
	for name, url := range w.Cfg.Proxy.Groups {
		h, ok := byURL[url]
		out = append(out, map[string]any{
			"Name": name, "URL": egress.Redacted(url),
			"OK": ok && h.OK, "Checked": ok, "Error": h.Error,
		})
	}
	return map[string]any{"Egress": out}, nil
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
	emails := map[string]string{}
	for _, a := range accounts {
		emails[a.ID] = a.Email
	}
	s := usage.Aggregate(snaps, time.Now())
	for i := range s.Accounts {
		s.Accounts[i].Email = emails[s.Accounts[i].ID]
	}
	return map[string]any{"Summary": s}, nil
}

// loginData is the login page's data.
func (w *WebUI) loginData(ctx context.Context) (map[string]any, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, p := range w.pending {
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
	w.mu.Lock()
	p := w.pending[id]
	w.mu.Unlock()
	if err != nil {
		w.mu.Lock()
		if p != nil {
			p.Err = err.Error()
		}
		w.mu.Unlock()
		return
	}
	now := time.Now()
	acc := store.Account{
		ID:        fmt.Sprintf("acc_%d", now.UnixMilli()),
		Name:      tok.Subject,
		UserID:    tok.Subject,
		Status:    store.StatusActive,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := w.Store.CreateAccount(ctx, acc); err != nil {
		return
	}
	_ = w.Store.SaveToken(ctx, store.Token{
		AccountID: acc.ID, Access: tok.Access, Refresh: tok.Refresh,
		ExpiresAt: tok.Expires, Subject: tok.Subject, UpdatedAt: now,
	})
	w.mu.Lock()
	delete(w.pending, id)
	w.mu.Unlock()
}

// apiPoll says how a sign-in is going.
func (w *WebUI) apiPoll(rw http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"id"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	w.mu.Lock()
	p := w.pending[body.ID]
	w.mu.Unlock()
	if p == nil {
		writeJSON(rw, map[string]any{"done": true})
		return
	}
	if p.Err != "" {
		writeJSON(rw, map[string]any{"error": p.Err})
		return
	}
	writeJSON(rw, map[string]any{"done": false})
}

// apiUsage gives the credits of every account.
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
	emails := map[string]string{}
	for _, a := range accounts {
		emails[a.ID] = a.Email
	}
	s := usage.Aggregate(snaps, time.Now())
	for i := range s.Accounts {
		s.Accounts[i].Email = emails[s.Accounts[i].ID]
	}
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

// apiEgressTest tests one outlet now.
func (w *WebUI) apiEgressTest(rw http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	url, ok := w.Cfg.Proxy.Groups[body.Name]
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
			ProxyGroup *string `json:"proxy_group"`
			ProxyURL   *string `json:"proxy_url"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		a, err := w.Store.GetAccount(r.Context(), id)
		if err != nil {
			http.Error(rw, err.Error(), http.StatusNotFound)
			return
		}
		if body.Name != nil {
			a.Name = *body.Name
		}
		if body.ProxyGroup != nil {
			// 组名写错会让账号悄悄改走全局默认。挡在这里。
			if *body.ProxyGroup != "" {
				if _, ok := w.Cfg.Proxy.Groups[*body.ProxyGroup]; !ok {
					http.Error(rw, "no such outlet group: "+*body.ProxyGroup, http.StatusBadRequest)
					return
				}
			}
			a.ProxyGroup = *body.ProxyGroup
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
		}
		a.UpdatedAt = time.Now()
		if err := w.Store.UpdateAccount(r.Context(), a); err != nil {
			http.Error(rw, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(rw, map[string]any{"ok": true})
	case http.MethodDelete:
		if err := w.Store.DeleteAccount(r.Context(), id); err != nil {
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
	rw.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(rw).Encode(v)
}
