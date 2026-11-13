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
	"fmt"
	"net/http"
	"sort"

	"mmxcode/internal/account"
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
}

// Client claims check-ins.
type Client struct {
	acc *account.Client
}

// New makes a client.
func New(acc *account.Client) *Client { return &Client{acc: acc} }

// Run reads the panel and claims today's credits.
//
// A 401 is answered the way MiniMax Code answers it: the sign-in is
// renewed and the check-in is asked once more. The renewal is the
// caller's job, so Run asks through the renewer it is given.
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
	if !asRefused(err, &refused) {
		return Outcome{}, err
	}
	return Outcome{Outcome: OutcomeFailed, Message: err.Error()}, nil
}

func (c *Client) run(ctx context.Context, access, userID string) (Outcome, error) {
	raw, err := c.acc.Call(ctx, access, userID, http.MethodGet, "/minimax-cloud/api/v1/signin/status", nil, checkinQuery())
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
		claimRaw, err := c.acc.Call(ctx, access, userID, http.MethodPost, "/minimax-cloud/api/v1/signin/claim", []byte("{}"), checkinQuery())
		if err != nil {
			return Outcome{}, err
		}
		var env struct {
			Data struct {
				ClaimResult *int     `json:"claim_result"`
				Points      *float64 `json:"points"`
			} `json:"data"`
		}
		if err := json.Unmarshal(claimRaw, &env); err != nil {
			return Outcome{}, fmt.Errorf("check-in: not JSON")
		}
		out := Outcome{Outcome: OutcomeDone}
		if env.Data.Points != nil {
			out.Credit = *env.Data.Points
		}
		if env.Data.ClaimResult != nil {
			switch *env.Data.ClaimResult {
			case 1:
				out.Outcome = OutcomeClaimed
				out.Streak = streakFrom(days, indexOf(days, today)-1) + 1
			case 2:
				out.Streak = streakFrom(days, indexOf(days, today)-1) + 1
			default:
				return Outcome{}, fmt.Errorf("check-in: unknown claim_result %d", *env.Data.ClaimResult)
			}
		}
		return out, nil
	default:
		return Outcome{Outcome: OutcomeInactive}, nil
	}
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
		return nil, fmt.Errorf("check-in: not JSON")
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

// checkinQuery is the query MiniMax Code's own check-in sends.
func checkinQuery() []account.QueryKV {
	return []account.QueryKV{
		{K: "device_platform", V: "web"},
		{K: "is_desktop", V: "1"},
		{K: "desktop_version", V: "0.6.12"},
	}
}

// asRefused unwraps an error into a refusal.
func asRefused(err error, target **account.RefusedError) bool {
	for err != nil {
		if re, ok := err.(*account.RefusedError); ok {
			*target = re
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
