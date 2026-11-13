// Package account reads MiniMax's account API: who the account is,
// what plan it has, how many credits it has left and what its plan's
// windows say.
//
// Every call is signed the way MiniMax Code signs one: the yy and
// x-signature headers. A bearer-only request is refused.
package account

import (
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
	// CreditType is 1 for a purchase, 2 for a check-in.
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

// checkinQuery is the query MiniMax Code's own check-in sends.
//
// The check-in comes from the desktop client, so it says so.
var checkinQuery = []signKV{
	{K: "device_platform", V: "web"},
	{K: "is_desktop", V: "1"},
	{K: "desktop_version", V: "0.6.12"},
}

// Identity reads who the account is.
func (c *Client) Identity(ctx context.Context, access, userID string) (Identity, error) {
	body, err := c.call(ctx, access, userID, http.MethodGet, site.IdentityPath, nil, nil)
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
		return Identity{}, fmt.Errorf("account: not JSON")
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
	body, err := c.call(ctx, access, userID, http.MethodPost,
		site.MembershipPath, []byte("{}"), nil)
	if err != nil {
		return Membership{}, err
	}
	var env struct {
		HasTokenPlan     *bool       `json:"has_token_plan"`
		TokenPlanTier    string      `json:"token_plan_tier"`
		TokenPlanExpires json.Number `json:"token_plan_expires_at"`
		OpGroupID        string      `json:"op_group_id"`
		OpCreditSummary  *struct {
			TotalRemainingAmount string `json:"total_remaining_amount"`
		} `json:"op_credit_summary"`
		OpCreditBalance json.RawMessage            `json:"opcredit_balance"`
		Data            map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return Membership{}, fmt.Errorf("membership: not JSON")
	}
	m := Membership{Known: true, Tier: env.TokenPlanTier, GroupID: env.OpGroupID}
	if env.HasTokenPlan != nil {
		m.HasTokenPlan = *env.HasTokenPlan
	}
	if env.OpCreditSummary != nil {
		m.Balance = env.OpCreditSummary.TotalRemainingAmount
	}
	if env.TokenPlanExpires != "" {
		if f, err := env.TokenPlanExpires.Float64(); err == nil && f > 0 {
			ms := int64(f)
			if ms < 1e12 {
				ms *= 1000
			}
			m.ExpiresAt = time.UnixMilli(ms)
		}
	}
	return m, nil
}

// Ledger reads every batch of credits.
//
// A page that can't be read whole gives nil: a part of the ledger
// would tell a wrong share.
func (c *Client) Ledger(ctx context.Context, access, userID string) ([]Batch, error) {
	const pageSize = 20
	const maxPages = 10
	var out []Batch
	for page := 1; page <= maxPages; page++ {
		query := append(append([]signKV{}, checkinQuery...),
			signKV{K: "page_number", V: strconv.Itoa(page)},
			signKV{K: "page_size", V: strconv.Itoa(pageSize)},
			signKV{K: "include_expired", V: "true"},
		)
		body, err := c.call(ctx, access, userID, http.MethodGet, site.CreditPath, nil, query)
		if err != nil {
			return nil, err
		}
		var env struct {
			Details    []Batch `json:"details"`
			TotalCount *int    `json:"total_count"`
			Data       *struct {
				Details    []Batch `json:"details"`
				TotalCount *int    `json:"total_count"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &env); err != nil {
			return nil, fmt.Errorf("credits: not JSON")
		}
		details, total := env.Details, env.TotalCount
		if len(details) == 0 && env.Data != nil {
			details, total = env.Data.Details, env.Data.TotalCount
		}
		if total == nil {
			return nil, nil
		}
		for i := range details {
			b := &details[i]
			granted, ok1 := ParseAmount(b.GrantedAmount, false)
			remaining, ok2 := ParseAmount(b.RemainingAmount, true)
			if !ok1 || !ok2 {
				continue
			}
			b.Granted = granted
			b.Remaining = math.Min(remaining, granted)
			b.Consumed = granted - b.Remaining
			b.GrantedAt = time.UnixMilli(b.GrantedAtMS)
			b.ExpiresAt = time.UnixMilli(b.ExpiresAtMS)
		}
		out = append(out, details...)
		if len(out) >= *total || len(details) == 0 {
			if len(out) < *total {
				return nil, nil
			}
			return out, nil
		}
	}
	return nil, nil
}

// PlanWindows reads the plan's windows.
//
// The console's own answer comes first; the open platform's is the
// fallback.
func (c *Client) PlanWindows(ctx context.Context, access, groupID string) ([]Window, error) {
	ws, err := c.planWindowsOnce(ctx, access, groupID, site.PlanRemainsPath)
	if err == nil {
		return ws, nil
	}
	ws2, err2 := c.planWindowsOnce(ctx, access, groupID, site.CodingPlanPath)
	if err2 != nil {
		return nil, err2
	}
	return ws2, nil
}

func (c *Client) planWindowsOnce(ctx context.Context, access, groupID, path string) ([]Window, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.platform+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+access)
	req.Header.Set("Accept-Encoding", "identity")
	if groupID != "" {
		req.Header.Set("X-Group-Id", groupID)
	}
	resp, err := c.up.Do(req)
	if err != nil {
		return nil, err
	}
	body, err := upstream.ReadBody(resp)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("M Plan: HTTP %d", resp.StatusCode)
	}
	return parsePlanWindows(body)
}

// PlanTitle reads the plan's name as the console says it.
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
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", fmt.Errorf("M Plan: HTTP %d", resp.StatusCode)
	}
	var env struct {
		CurrentSubscribe *struct {
			Title string `json:"current_subscribe_title"`
		} `json:"current_subscribe"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return "", fmt.Errorf("M Plan: not JSON")
	}
	if env.CurrentSubscribe == nil {
		return "", nil
	}
	return strings.TrimSpace(env.CurrentSubscribe.Title), nil
}

// Models gives the site's models.
//
// The built-in list is what the site is known to have. The live list
// is asked for by whoever needs it — the gateway's /v1/models — so
// that a fallback and a live read are not confused with each other.
func (c *Client) Models(access string) []site.Model {
	return site.FallbackModels()
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
