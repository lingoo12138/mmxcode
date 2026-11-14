// Package account reads MiniMax's account API: who the account is,
// what plan it has, how many credits it has left and what its plan's
// windows say.
//
// Every call is signed the way MiniMax Code signs one: the yy and
// x-signature headers. A bearer-only request is refused.
package account

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"

	"mmxcode/internal/site"
	"mmxcode/internal/upstream"
)

// modelsTimeout is how long the live model list is waited for.
//
// The reference gives the call ten seconds; the console's endpoints are
// not documented, so a stalled one gets little of that.
const modelsTimeout = 10 * time.Second

// The credit types the plan counts.
//
// The ledger writes them as plain numbers, and the plan counts only
// these two: what was bought and what a check-in gave. Another type is
// a bucket the plan does not count.
const (
	// CreditTypePurchase is a batch that was bought.
	CreditTypePurchase = 1
	// CreditTypeCheckin is a batch that a check-in gave.
	CreditTypeCheckin = 2
)

// Identity is who the account is.
type Identity struct {
	// RealUserID is the MiniMax user id.
	RealUserID string
	// Email is the account's email.
	Email string
	// Name is the account's name.
	Name string
}

// Batch is one batch of credits.
//
// The site writes the amounts as strings, sometimes with a thousands
// separator, so they are kept as they come and read by ParseAmount.
type Batch struct {
	// CreditType is CreditTypePurchase or CreditTypeCheckin for the
	// batches the plan counts. Another type is a bucket the plan does
	// not count.
	CreditType int `json:"credit_type"`
	// GrantedAmount is what the batch gave.
	GrantedAmount string `json:"granted_amount"`
	// ConsumedAmount is what of it was used.
	ConsumedAmount string `json:"consumed_amount"`
	// RemainingAmount is what of it is left.
	RemainingAmount string `json:"remaining_amount"`
	// GrantedAtMS is when it was given, in milliseconds.
	GrantedAtMS int64 `json:"granted_at_ms"`
	// ExpiresAtMS is when it runs out, in milliseconds.
	ExpiresAtMS int64 `json:"expire_at_ms"`

	// Granted is what the batch gave, once read.
	Granted float64 `json:"-"`
	// Consumed is what of it was used, once read.
	Consumed float64 `json:"-"`
	// Remaining is what of it is left, once read.
	Remaining float64 `json:"-"`
	// GrantedAt is when it was given.
	GrantedAt time.Time `json:"-"`
	// ExpiresAt is when it runs out.
	ExpiresAt time.Time `json:"-"`
	// HasGrantedAt says the answer carried the time the batch was
	// given. A batch without one has no GrantedAt, not one in 1970.
	HasGrantedAt bool `json:"-"`
	// HasExpiresAt says the answer carried the time the batch runs
	// out. A batch without one cannot be read: the credits of a batch
	// that never expires cannot be told from those of a spent one, and
	// a wrong share is worse than none.
	HasExpiresAt bool `json:"-"`
}

// Window is one plan window: the five hours, the seven days, or a
// model's own.
type Window struct {
	// Name is the window's name.
	Name string
	// ModelName is the model group the window belongs to.
	ModelName string
	// Used is the share used, in percent.
	Used float64
	// ResetsAt is when the window resets.
	ResetsAt time.Time
	// Amount is what was used, when the window counts.
	Amount float64
	// Limit is what was allowed, when the window counts.
	Limit float64
	// Unit is what it counts, when it counts.
	Unit string
	// Unlimited says the window is not limited.
	Unlimited bool
}

// Membership is the account's plan.
type Membership struct {
	// HasTokenPlan says the account has a plan.
	HasTokenPlan bool
	// Known says the site's membership API answered.
	Known bool
	// Tier is the plan's tier.
	Tier string
	// Title is the plan's name as the console says it.
	Title string
	// ExpiresAt is when the plan ends.
	ExpiresAt time.Time
	// GroupID is the group the plan's windows are under.
	GroupID string
	// Balance is the credits left, as the site writes it.
	Balance string
}

// Sender sends a request.
//
// It is an interface so that an account with its own proxy can have
// every one of its calls go out through it: the caller hands over the
// sender that belongs to the account.
type Sender interface {
	Do(req *http.Request) (*http.Response, error)
}

// nowOffset gives the timezone offset in seconds east of UTC.
//
// It is the value the reference sends as getTimezoneOffset() * -60.
// time.Time has no Offset() method: Zone()'s second value is it.
func nowOffset() int {
	_, offset := time.Now().Zone()
	return offset
}

// Client reads the account API.
type Client struct {
	up Sender
	// base is the host the account API is on.
	base string
	// platform is the host the plan console is on.
	platform string
	// now gives the time; a test can set it.
	now func() time.Time
	// tzOffset is the client's timezone offset in seconds.
	tzOffset int
	// osName is the client's platform name.
	osName string
}

// New makes a client.
func New(up Sender) *Client {
	return &Client{
		up:       up,
		base:     site.AgentHost,
		platform: site.PlatformHost,
		now:      time.Now,
		// 秒，UTC 以东。参考实现发的是 getTimezoneOffset() * -60，
		// 和 Zone() 给的偏移是同一个值。time.Time 没有 Offset()
		// 方法，Zone() 的第二个返回值就是它。
		tzOffset: nowOffset(),
		osName:   runtime.GOOS,
	}
}

// SetBase sends the calls to other hosts, for a test.
//
// The account API and the plan console live on two hosts, so both are
// given. Without this the tests talk to MiniMax itself, and fail
// wherever the site is out of reach.
func (c *Client) SetBase(base, platform string) {
	c.base = base
	c.platform = platform
}

// With gives a copy of the client that sends through another sender.
//
// Every fetch an account makes goes through the account's own way out,
// or the global one. An account behind a proxy that is not used for
// its usage reading is an account whose reading never arrives.
func (c *Client) With(up Sender) *Client {
	// The hosts go with it: a client made for an account's own exit
	// must still call the same hosts, or the request goes nowhere.
	return &Client{up: up, base: c.base, platform: c.platform, now: c.now, tzOffset: c.tzOffset, osName: c.osName}
}

// SetClock replaces the clock, for a test.
func (c *Client) SetClock(now func() time.Time, tzOffset int) {
	c.now = now
	c.tzOffset = tzOffset
}

// CheckinQuery is the query MiniMax Code's own check-in sends.
//
// It is the site's one, in the shape the signing package takes. The
// check-in and the credits ledger ask the same way, and the copy is
// kept in one place: the site's.
func CheckinQuery() []QueryKV {
	out := make([]QueryKV, 0, len(site.CheckinQuery))
	for _, kv := range site.CheckinQuery {
		out = append(out, QueryKV{K: kv.K, V: kv.V})
	}
	return out
}

// Identity reads who the account is.
func (c *Client) Identity(ctx context.Context, access, userID string) (Identity, error) {
	body, err := c.Call(ctx, access, userID, http.MethodGet, site.IdentityPath, nil, nil)
	if err != nil {
		return Identity{}, err
	}
	var env struct {
		Data struct {
			UserInfo struct {
				RealUserID  string `json:"realUserID"`
				RealUserID2 string `json:"real_user_id"`
				UserEmail   string `json:"userEmail"`
				Email       string `json:"email"`
				UserMail    string `json:"userMail"`
				UserEmail2  string `json:"user_email"`
				Name        string `json:"name"`
				UserName    string `json:"userName"`
				UserName2   string `json:"user_name"`
			} `json:"userInfo"`
		} `json:"data"`
		UserInfo *struct {
			RealUserID string `json:"realUserID"`
		} `json:"userInfo"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return Identity{}, fmt.Errorf("account: not JSON: %w", err)
	}
	id := firstNonEmpty(env.Data.UserInfo.RealUserID, env.Data.UserInfo.RealUserID2)
	if id == "" {
		return Identity{}, fmt.Errorf("account: no user id in the reply")
	}
	return Identity{
		RealUserID: id,
		Email:      firstNonEmpty(env.Data.UserInfo.UserEmail, env.Data.UserInfo.Email, env.Data.UserInfo.UserMail, env.Data.UserInfo.UserEmail2),
		Name:       firstNonEmpty(env.Data.UserInfo.Name, env.Data.UserInfo.UserName, env.Data.UserInfo.UserName2),
	}, nil
}

// Membership reads the account's plan.
//
// The international site answers the membership API with 404, which
// means the plan is unknown, not that there is none.
//
// The answer's fields are read from the top level first and from the
// object under "data" second, the way the reference reads them: a
// field at the top level wins, and one that is only under "data" is
// used when the top level does not carry it. The real answer nests
// them under "data", so a plan that is only there is a plan.
func (c *Client) Membership(ctx context.Context, access, userID string) (Membership, error) {
	m, err := c.membershipOnce(ctx, access, userID)
	if err == nil {
		return m, nil
	}
	if isNotFound(err) {
		return Membership{Known: false}, nil
	}
	return Membership{}, err
}

func (c *Client) membershipOnce(ctx context.Context, access, userID string) (Membership, error) {
	body, err := c.Call(ctx, access, userID, http.MethodPost,
		site.MembershipPath, []byte("{}"), nil)
	if err != nil {
		return Membership{}, err
	}
	var env map[string]json.RawMessage
	if err := json.Unmarshal(body, &env); err != nil {
		return Membership{}, fmt.Errorf("membership: not JSON: %w", err)
	}
	// The fields are sometimes at the top level and sometimes under
	// "data". A "data" that is not an object carries no fields.
	var data map[string]json.RawMessage
	if raw, ok := env["data"]; ok {
		_ = json.Unmarshal(raw, &data)
	}
	pick := func(k string) json.RawMessage {
		if v, ok := env[k]; ok && !isJSONNull(v) {
			return v
		}
		if v, ok := data[k]; ok && !isJSONNull(v) {
			return v
		}
		return nil
	}
	m := Membership{Known: true}
	if raw := pick("has_token_plan"); raw != nil {
		var has bool
		if json.Unmarshal(raw, &has) == nil {
			m.HasTokenPlan = has
		}
	}
	m.Tier = jsonString(pick("token_plan_tier"))
	m.GroupID = jsonString(pick("op_group_id"))
	if f, ok := jsonNumber(pick("token_plan_expires_at")); ok && f > 0 {
		ms := int64(f)
		if ms < 1e12 {
			ms *= 1000
		}
		m.ExpiresAt = time.UnixMilli(ms)
	}
	m.Balance = membershipBalance(env, data, pick)
	return m, nil
}

// membershipBalance is the credits left, as the site writes them.
//
// The summary's total is the balance. When there is no total, the
// account's own opcredit_balance is it: as a string, trimmed, or as a
// number.
func membershipBalance(env, data map[string]json.RawMessage, pick func(string) json.RawMessage) string {
	sum := jsonObject(env["op_credit_summary"])
	if sum == nil {
		sum = jsonObject(data["op_credit_summary"])
	}
	if sum != nil {
		if v := jsonString(sum["total_remaining_amount"]); v != "" {
			return v
		}
	}
	return jsonBalance(pick("opcredit_balance"))
}

// isJSONNull says a value is the JSON null.
func isJSONNull(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return false
	}
	return v == nil
}

// jsonObject is a value as an object, or nothing when it is not one.
func jsonObject(raw json.RawMessage) map[string]json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	return m
}

// jsonString is a value as a trimmed string, or nothing when it is not
// a string or is empty.
func jsonString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return strings.TrimSpace(s)
}

// jsonBalance is a balance the site wrote as a string or as a number.
func jsonBalance(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	var f float64
	if json.Unmarshal(raw, &f) == nil {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	return ""
}

// jsonNumber is a number the site wrote as a number or as a string.
func jsonNumber(raw json.RawMessage) (float64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var f float64
	if json.Unmarshal(raw, &f) == nil {
		return f, true
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		s = strings.TrimSpace(s)
		if s == "" {
			return 0, false
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return f, true
		}
	}
	return 0, false
}

// ledgerBatch is a batch as the site writes it.
//
// The times are pointers, so that a batch without one is told from a
// batch with a zero one. The site writes the time the credits run out
// in, and a batch without it cannot be read.
type ledgerBatch struct {
	Batch
	GrantedAtMS *int64 `json:"granted_at_ms"`
	ExpiresAtMS *int64 `json:"expire_at_ms"`
}

// batch turns the answer's batch into the one the callers see.
//
// A time the answer did not carry stays unset: it is not the epoch,
// and saying so would make an unreadable batch look like a spent one.
func (l ledgerBatch) batch() Batch {
	b := l.Batch
	if l.GrantedAtMS != nil {
		b.GrantedAtMS = *l.GrantedAtMS
		b.HasGrantedAt = true
		b.GrantedAt = time.UnixMilli(*l.GrantedAtMS)
	}
	if l.ExpiresAtMS != nil {
		b.ExpiresAtMS = *l.ExpiresAtMS
		b.HasExpiresAt = true
		b.ExpiresAt = time.UnixMilli(*l.ExpiresAtMS)
	}
	return b
}

// Ledger reads every batch of credits.
//
// A ledger that was read and is empty gives an empty list: the account
// has no credits, which is not the same as a ledger that could not be
// read. A ledger that cannot be read gives nil: a part of the ledger
// would tell a wrong share. So does a batch whose amounts cannot be
// read: the credits cannot be told apart then, and none is better
// than a wrong share.
func (c *Client) Ledger(ctx context.Context, access, userID string) ([]Batch, error) {
	const pageSize = 20
	const maxPages = 10
	// The list starts empty and not nil: a ledger that was read and is
	// empty is an empty list, and a nil one would say the ledger could
	// not be read.
	out := []Batch{}
	for page := 1; page <= maxPages; page++ {
		query := append(append([]QueryKV{}, CheckinQuery()...),
			QueryKV{K: "page_number", V: strconv.Itoa(page)},
			QueryKV{K: "page_size", V: strconv.Itoa(pageSize)},
			QueryKV{K: "include_expired", V: "true"},
		)
		body, err := c.Call(ctx, access, userID, http.MethodGet, site.CreditPath, nil, query)
		if err != nil {
			return nil, err
		}
		var env struct {
			Details    []ledgerBatch `json:"details"`
			TotalCount *int          `json:"total_count"`
			Data       *struct {
				Details    []ledgerBatch `json:"details"`
				TotalCount *int          `json:"total_count"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &env); err != nil {
			return nil, fmt.Errorf("credits: not JSON: %w", err)
		}
		details, total := env.Details, env.TotalCount
		if len(details) == 0 && env.Data != nil {
			details, total = env.Data.Details, env.Data.TotalCount
		}
		if total == nil {
			// The answer is not a ledger: it cannot be read.
			return nil, nil
		}
		for _, lb := range details {
			b := lb.batch()
			if b.CreditType == CreditTypePurchase || b.CreditType == CreditTypeCheckin {
				granted, ok1 := ParseAmount(b.GrantedAmount, false)
				remaining, ok2 := ParseAmount(b.RemainingAmount, true)
				if !ok1 || !ok2 {
					// The amounts cannot be read: the credits of
					// this batch cannot be told from the ones of
					// a spent batch, and the ledger as a whole
					// cannot be read.
					return nil, nil
				}
				b.Granted = granted
				b.Remaining = math.Min(remaining, granted)
				b.Consumed = granted - b.Remaining
			}
			out = append(out, b)
		}
		if len(out) >= *total || len(details) == 0 {
			if len(out) < *total {
				// The answer says there is more than it gave.
				return nil, nil
			}
			return out, nil
		}
	}
	// Too many pages: the ledger was not read whole.
	return nil, nil
}

// PlanWindows reads the plan's windows.
//
// The console's own answer comes first; the open platform's is the
// fallback.
//
// The console's answer is used only when it has a list of windows in
// it. The reference uses it only when the answer is a list that is not
// empty, and asks the open platform otherwise: the international site's
// remains_percent answers 200 with an empty list, and the windows are
// at the other endpoint.
//
// A list that is not empty is the console's answer even when every
// entry in it is a bucket the plan does not have: then the plan has no
// windows, and that is an answer, not a reason to ask again.
//
// An answer the console could not give is not thrown away when the
// open platform has the windows: the two endpoints are two ways of
// asking the same question, and the one that answered is the one that
// counts.
func (c *Client) PlanWindows(ctx context.Context, access, groupID string) ([]Window, error) {
	ws, listed, err := c.planWindowsOnce(ctx, access, groupID, site.PlanRemainsPath)
	if err == nil && listed {
		return ws, nil
	}
	ws2, _, err2 := c.planWindowsOnce(ctx, access, groupID, site.CodingPlanPath)
	if err2 != nil {
		return nil, err2
	}
	return ws2, nil
}

func (c *Client) planWindowsOnce(ctx context.Context, access, groupID, path string) ([]Window, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.platform+path, nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+access)
	req.Header.Set("Accept-Encoding", "identity")
	if groupID != "" {
		req.Header.Set("X-Group-Id", groupID)
	}
	resp, err := c.up.Do(req)
	if err != nil {
		return nil, false, err
	}
	body, err := upstream.ReadBody(resp)
	if err != nil {
		return nil, false, err
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, false, &RefusedError{Status: resp.StatusCode}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, false, fmt.Errorf("M Plan: HTTP %d", resp.StatusCode)
	}
	return parsePlanWindows(body)
}

// PlanTitle reads the plan's name as the console says it.
//
// The reference gives up quietly when the name cannot be read and the
// usage card names the tier instead. This keeps the error, so that a
// caller can tell a plan without a name from a plan that could not be
// read; the manager treats a failure as no name, as the reference does.
func (c *Client) PlanTitle(ctx context.Context, access, groupID string) (string, error) {
	path := c.platform + site.PlanTitlePath + "?biz_line=2&cycle_type=1&resource_package_type=7"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, path, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+access)
	req.Header.Set("Accept-Encoding", "identity")
	if groupID != "" {
		req.Header.Set("X-Group-Id", groupID)
	}
	resp, err := c.up.Do(req)
	if err != nil {
		return "", err
	}
	body, err := upstream.ReadBody(resp)
	if err != nil {
		return "", err
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return "", &RefusedError{Status: resp.StatusCode}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", fmt.Errorf("M Plan: HTTP %d", resp.StatusCode)
	}
	var env struct {
		CurrentSubscribe *struct {
			Title string `json:"current_subscribe_title"`
		} `json:"current_subscribe"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return "", fmt.Errorf("M Plan: not JSON: %w", err)
	}
	if env.CurrentSubscribe == nil {
		return "", nil
	}
	return strings.TrimSpace(env.CurrentSubscribe.Title), nil
}

// Models gives the site's models.
//
// The live list is asked for with the account's own token, the way the
// reference asks for it: the site's own answer, the list without a
// token when the account's one is refused, and the built-in list when
// the live one cannot be read. The built-in list is what the site is
// known to have, so a fallback and a live read are not confused with
// each other.
func (c *Client) Models(access string) []site.Model {
	if c.up == nil {
		return site.FallbackModels()
	}
	ctx, cancel := context.WithTimeout(context.Background(), modelsTimeout)
	defer cancel()
	body, status, ok := c.modelsOnce(ctx, access)
	if !ok && (status == http.StatusUnauthorized || status == http.StatusForbidden) {
		// The account's own token was turned away: the site's list is
		// asked for without one.
		body, _, ok = c.modelsOnce(ctx, "")
	}
	if !ok {
		return site.FallbackModels()
	}
	ms := parseModels(body)
	if len(ms) == 0 {
		return site.FallbackModels()
	}
	return ms
}

// modelsOnce asks the site for its model list.
//
// The model list is on the chat host, which is the account host today:
// the two are the same host, and the path is the chat's.
func (c *Client) modelsOnce(ctx context.Context, access string) ([]byte, int, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+site.ModelsPath+"?"+site.ModelsQuery, nil)
	if err != nil {
		return nil, 0, false
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "identity")
	if access != "" {
		req.Header.Set("Authorization", "Bearer "+access)
	}
	resp, err := c.up.Do(req)
	if err != nil {
		return nil, 0, false
	}
	body, err := upstream.ReadBody(resp)
	if err != nil {
		return nil, resp.StatusCode, false
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, resp.StatusCode, false
	}
	return body, resp.StatusCode, true
}

// modelProvider is one provider of the model config.
type modelProvider struct {
	ProviderID string          `json:"providerId"`
	Config     json.RawMessage `json:"config"`
}

// modelConfig is the models of one provider.
//
// The models are kept as they were written: Go's maps have no order,
// and the answer's order is the order the site's models are listed in.
type modelConfig struct {
	Models     json.RawMessage `json:"models"`
	ModelOrder []string        `json:"model_order"`
}

// modelEntry is one model of the config.
type modelEntry struct {
	Name       string `json:"name"`
	Reasoning  *bool  `json:"reasoning"`
	Attachment *bool  `json:"attachment"`
	Limit      struct {
		Context json.Number `json:"context"`
		Output  json.Number `json:"output"`
	} `json:"limit"`
	Modalities struct {
		Input []string `json:"input"`
	} `json:"modalities"`
	ThinkingConfig struct {
		Mode string `json:"mode"`
	} `json:"thinking_config"`
	Thinking *struct {
		EffortOptions    []string `json:"effortOptions"`
		EffortOptionsAlt []string `json:"effort_options"`
	} `json:"thinking"`
	EffortOptions []string `json:"effort_options"`
}

// parseModels reads MiniMax Code's model config as models.
//
// The answer is {providers:[{providerId:"minimax",config:{models:{<id>:{…}},
// model_order?}}]}. The models come in the order model_order gives
// them, the rest in the order the answer wrote them.
func parseModels(body []byte) []site.Model {
	var env struct {
		Providers []modelProvider `json:"providers"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil
	}
	var cfg *modelConfig
	for i := range env.Providers {
		if env.Providers[i].ProviderID != "minimax" {
			continue
		}
		var c modelConfig
		if err := json.Unmarshal(env.Providers[i].Config, &c); err != nil {
			continue
		}
		cfg = &c
		break
	}
	if cfg == nil || len(cfg.Models) == 0 {
		return nil
	}
	models := map[string]json.RawMessage{}
	if json.Unmarshal(cfg.Models, &models) != nil {
		return nil
	}
	ids := make([]string, 0, len(models))
	seen := map[string]bool{}
	for _, id := range cfg.ModelOrder {
		if _, ok := models[id]; ok && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	for _, id := range jsonKeys(cfg.Models) {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	out := make([]site.Model, 0, len(ids))
	for _, id := range ids {
		if strings.TrimSpace(id) == "" {
			continue
		}
		var e modelEntry
		if err := json.Unmarshal(models[id], &e); err != nil {
			continue
		}
		m := site.Model{ID: id, Name: id}
		if name := strings.TrimSpace(e.Name); name != "" {
			m.Name = name
		}
		if f, err := e.Limit.Context.Float64(); err == nil && f > 0 {
			m.Context = int(f)
		}
		if f, err := e.Limit.Output.Float64(); err == nil && f > 0 {
			m.Output = int(f)
		}
		inputs := []string{"text"}
		for _, k := range e.Modalities.Input {
			k = strings.ToLower(k)
			if k == "" || k == "text" {
				continue
			}
			inputs = append(inputs, k)
		}
		// A model said to take attachments takes images, as before #985.
		if e.Attachment != nil && *e.Attachment && !hasInput(inputs, "image") {
			inputs = append(inputs, "image")
		}
		m.Inputs = inputs
		m.Thinking = modelThinking(e)
		m.Efforts = modelEfforts(e)
		out = append(out, m)
	}
	return out
}

// modelThinking says how a model handles thinking.
func modelThinking(e modelEntry) site.Thinking {
	switch strings.TrimSpace(e.ThinkingConfig.Mode) {
	case "switchable":
		return site.ThinkingSwitchable
	case "forced_off":
		return site.ThinkingOff
	case "forced_on":
		return site.ThinkingForcedOn
	}
	if e.Reasoning != nil && !*e.Reasoning {
		return site.ThinkingOff
	}
	return site.ThinkingOn
}

// modelEfforts are the reasoning levels the model takes.
func modelEfforts(e modelEntry) []string {
	var all []string
	if e.Thinking != nil {
		if len(e.Thinking.EffortOptions) > 0 {
			all = e.Thinking.EffortOptions
		} else {
			all = e.Thinking.EffortOptionsAlt
		}
	}
	if len(all) == 0 {
		all = e.EffortOptions
	}
	out := []string{}
	for _, v := range all {
		v = strings.TrimSpace(v)
		if v == "" || v == "default" || v == "none" {
			continue
		}
		out = append(out, v)
	}
	return out
}

// hasInput says the kinds of input already name one.
func hasInput(inputs []string, k string) bool {
	for _, in := range inputs {
		if in == k {
			return true
		}
	}
	return false
}

// jsonKeys gives an object's keys in the order the answer wrote them.
//
// Go's maps have no order, and the answer's order is the order the
// site's models are listed in.
func jsonKeys(raw json.RawMessage) []string {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return nil
	}
	var keys []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return keys
		}
		key, ok := tok.(string)
		if !ok {
			return keys
		}
		keys = append(keys, key)
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return keys
		}
	}
	return keys
}

// ParseAmount reads a ledger amount.
//
// The site writes them as decimal strings, sometimes with a thousands
// separator. A negative remaining is zero.
func ParseAmount(v string, clampNegative bool) (float64, bool) {
	t := strings.ReplaceAll(strings.TrimSpace(v), ",", "")
	if t == "" {
		return 0, false
	}
	if clampNegative && strings.HasPrefix(t, "-") {
		if !isDecimal(t[1:]) {
			return 0, false
		}
		return 0, true
	}
	if !isDecimal(t) {
		return 0, false
	}
	f, err := strconv.ParseFloat(t, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

func isDecimal(s string) bool {
	if s == "" {
		return false
	}
	dots := 0
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] >= '0' && s[i] <= '9':
		case s[i] == '.':
			dots++
			if dots > 1 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}
