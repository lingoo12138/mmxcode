// Package checkin claims the account's daily credits.
//
// The check-in comes from the desktop client, so it says so: the
// device_platform is "web", the desktop version is MiniMax Code's, and
// the account's own user id is in the query. The call is signed the
// same way the rest of the account API is.
package checkin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"

	"github.com/lingoo12138/mmxcode/internal/account"
	"github.com/lingoo12138/mmxcode/internal/site"
	"github.com/lingoo12138/mmxcode/internal/upstream"
)

// The outcomes.
const (
	OutcomeClaimed    = "claimed"
	OutcomeDone       = "done"
	OutcomeIneligible = "ineligible"
	OutcomeInactive   = "inactive"
	OutcomeFailed     = "failed"
)

// Outcome is what a check-in gave.
type Outcome struct {
	// Outcome is one of the constants above.
	Outcome string
	// Credit is what it paid.
	Credit float64
	// Streak is the days in a row.
	Streak int
	// Message is what to show.
	Message string
	// Refused says the site does not take this sign-in.
	//
	// 「签到没成」有两种：一种是站点不认这个号（401、403，或者 200 的
	// 回答里带 MiniMax 的拒绝码），一种是站点自己出了毛病（网络断了，
	// 面板坏了）。第一种要续签一次再问一次 —— 令牌可能只是过期了。第
	// 二种不用：续签跟它没关系，把它推进重登是冤枉一个好号。
	//
	// 所以这个区别得留在结果里，不能压成同一句话。
	Refused bool
}

// Client claims check-ins.
type Client struct {
	acc *account.Client
}

// New makes a client.
func New(acc *account.Client) *Client { return &Client{acc: acc} }

// Run reads the panel and claims today's credits.
//
// The sign-in is the caller's business: the caller renews the token
// before it calls, and Run neither renews nor tries again. What Run
// does with a refused sign-in — an HTTP 401 or 403, or MiniMax's own
// refusal code in the body of a 200 — is to answer OutcomeFailed
// instead of an error: a refused sign-in is one event, however it
// arrives. The refusal is said out loud in Refused, so that the
// caller can tell it apart from a site that is merely having a bad
// moment.
func (c *Client) Run(ctx context.Context, access, userID string) (Outcome, error) {
	if userID == "" {
		id, err := c.acc.Identity(ctx, access, "")
		if err != nil {
			return Outcome{}, err
		}
		userID = id.RealUserID
	}
	out, err := c.run(ctx, access, userID)
	if err == nil {
		return out, nil
	}
	var refused *account.RefusedError
	if errors.As(err, &refused) || errors.Is(err, upstream.ErrSignInRefused) {
		return Outcome{Outcome: OutcomeFailed, Refused: true, Message: err.Error()}, nil
	}
	return Outcome{}, err
}

func (c *Client) run(ctx context.Context, access, userID string) (Outcome, error) {
	raw, err := c.acc.Call(ctx, access, userID, http.MethodGet, site.SigninStatusPath, nil, account.CheckinQuery())
	if err != nil {
		return Outcome{}, err
	}
	days, err := parsePanel(raw)
	if err != nil {
		return Outcome{}, err
	}
	today := findToday(days)
	if today == nil {
		return Outcome{Outcome: OutcomeInactive}, nil
	}
	switch today.Status {
	case 3:
		return Outcome{
			Outcome: OutcomeDone,
			Credit:  today.Points,
			Streak:  streakFrom(days, indexOf(days, today)),
		}, nil
	case 4:
		return Outcome{Outcome: OutcomeIneligible}, nil
	case 2:
		claimRaw, err := c.acc.Call(ctx, access, userID, http.MethodPost, site.SigninClaimPath, []byte("{}"), account.CheckinQuery())
		if err != nil {
			return Outcome{}, err
		}
		var env struct {
			Data struct {
				ClaimResult json.RawMessage `json:"claim_result"`
				Points      *float64        `json:"points"`
			} `json:"data"`
		}
		if err := json.Unmarshal(claimRaw, &env); err != nil {
			return Outcome{}, fmt.Errorf("check-in: not JSON: %w", err)
		}
		out := Outcome{Outcome: OutcomeDone}
		if env.Data.Points != nil && *env.Data.Points > 0 {
			out.Credit = *env.Data.Points
		}
		// The claim answers 1 when it claimed now and 2 when the day
		// was already claimed. Anything else is not a claim: the
		// reference throws for it, and so does this. A check-in that
		// is not known is not a check-in that was done.
		result, ok := claimResult(env.Data.ClaimResult)
		if !ok {
			return Outcome{}, fmt.Errorf("check-in: unknown claim_result %s", claimText(env.Data.ClaimResult))
		}
		switch result {
		case 1:
			out.Outcome = OutcomeClaimed
			out.Streak = streakFrom(days, indexOf(days, today)-1) + 1
		case 2:
			out.Streak = streakFrom(days, indexOf(days, today)-1) + 1
		default:
			return Outcome{}, fmt.Errorf("check-in: unknown claim_result %v", result)
		}
		return out, nil
	default:
		return Outcome{Outcome: OutcomeInactive}, nil
	}
}

// claimResult reads the claim's answer as the reference reads it: a
// number above zero, and nothing else.
func claimResult(raw json.RawMessage) (float64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var f float64
	if json.Unmarshal(raw, &f) != nil {
		return 0, false
	}
	if f <= 0 {
		return 0, false
	}
	return f, true
}

// claimText is what the answer said, for the error's text.
func claimText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "undefined"
	}
	return string(raw)
}

// day is one day of the panel.
type day struct {
	DayNo   int     `json:"day_no"`
	Status  int     `json:"status"`
	IsToday bool    `json:"is_today"`
	Points  float64 `json:"points"`
}

// parsePanel reads the seven-day panel.
func parsePanel(raw []byte) ([]day, error) {
	var env struct {
		Data struct {
			Days []day `json:"days"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("check-in: not JSON: %w", err)
	}
	days := env.Data.Days
	if len(days) == 0 {
		return nil, fmt.Errorf("check-in: no panel in the reply")
	}
	sort.Slice(days, func(i, j int) bool { return days[i].DayNo < days[j].DayNo })
	if len(days) != 7 {
		return nil, fmt.Errorf("check-in: malformed panel")
	}
	for i, d := range days {
		if d.DayNo != i+1 {
			return nil, fmt.Errorf("check-in: malformed panel")
		}
	}
	return days, nil
}

func findToday(days []day) *day {
	for i := range days {
		if days[i].IsToday {
			return &days[i]
		}
	}
	return nil
}

func indexOf(days []day, d *day) int {
	for i := range days {
		if &days[i] == d {
			return i
		}
	}
	return -1
}

// streakFrom counts the claimed days from an index back.
func streakFrom(days []day, i int) int {
	n := 0
	for j := i; j >= 0 && days[j].Status == 3; j-- {
		n++
	}
	return n
}
