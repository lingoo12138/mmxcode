package account

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"mmxcode/internal/sign"
	"mmxcode/internal/site"
	"mmxcode/internal/upstream"
)

// QueryKV is one query parameter for a signed call.
type QueryKV = sign.KV

// Call sends one signed account API request.
//
// The body is "{}" for a POST and nil for a GET, as MiniMax Code sends
// it. The answer's business codes are checked before it is handed back.
func (c *Client) Call(ctx context.Context, access, userID, method, path string, body []byte, query []QueryKV) ([]byte, error) {
	now := c.now()
	signed := sign.Sign(c.base, path, sign.Options{
		Access:   access,
		UserID:   userID,
		Body:     body,
		Query:    query,
		Now:      now.UnixMilli(),
		TZOffset: c.tzOffset,
		OSName:   c.osName,
		Lang:     site.Lang,
	})
	var req *http.Request
	var err error
	if method == http.MethodPost {
		req, err = http.NewRequestWithContext(ctx, method, signed.URL, strings.NewReader(string(body)))
	} else {
		req, err = http.NewRequestWithContext(ctx, method, signed.URL, nil)
	}
	if err != nil {
		return nil, err
	}
	for k, v := range signed.Headers() {
		req.Header.Set(k, v)
	}
	req.Header.Set("Accept-Encoding", "identity")

	resp, err := c.up.Do(req)
	if err != nil {
		return nil, err
	}
	raw, err := upstream.ReadBody(resp)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, &RefusedError{Status: resp.StatusCode}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &statusError{status: resp.StatusCode}
	}
	if err := upstream.CheckEnvelope("account", raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// RefusedError is a refusal of the sign-in.
type RefusedError struct {
	Status int
}

// Error is the error's text.
func (e *RefusedError) Error() string {
	return fmt.Sprintf("the sign-in was refused (HTTP %d)", e.Status)
}

// isNotFound says an error is a 404.
func isNotFound(err error) bool {
	var re *statusError
	return asStatus(err, &re) && re.status == http.StatusNotFound
}

type statusError struct{ status int }

func (e *statusError) Error() string { return "HTTP " + strconv.Itoa(e.status) }

// asStatus unwraps an error into a status error.
//
// The call helper returns a plain fmt.Errorf for a bad status, so the
// status is carried in a typed error instead of a formatted string.
func asStatus(err error, target **statusError) bool {
	for err != nil {
		if se, ok := err.(*statusError); ok {
			*target = se
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

// parsePlanWindows reads the console's answer as windows.
func parsePlanWindows(raw []byte) ([]Window, error) {
	var env struct {
		BaseResp *struct {
			StatusCode int    `json:"status_code"`
			StatusMsg  string `json:"status_msg"`
		} `json:"base_resp"`
		ModelRemains []struct {
			ModelName                string   `json:"model_name"`
			CurrentIntervalStatus    *float64 `json:"current_interval_status"`
			CurrentIntervalRemaining *float64 `json:"current_interval_remaining_percent"`
			CurrentIntervalUsed      *float64 `json:"current_interval_used_percent"`
			CurrentIntervalTotal     *float64 `json:"current_interval_total_percent"`
			CurrentIntervalUsedCount *float64 `json:"current_interval_used_count"`
			CurrentIntervalTotalCnt  *float64 `json:"current_interval_total_count"`
			StartTime                int64    `json:"start_time"`
			EndTime                  int64    `json:"end_time"`
			CurrentWeeklyStatus      *float64 `json:"current_weekly_status"`
			CurrentWeeklyRemaining   *float64 `json:"current_weekly_remaining_percent"`
			CurrentWeeklyUsed        *float64 `json:"current_weekly_used_percent"`
			CurrentWeeklyTotal       *float64 `json:"current_weekly_total_percent"`
			CurrentWeeklyUsedCount   *float64 `json:"current_weekly_used_count"`
			CurrentWeeklyTotalCnt    *float64 `json:"current_weekly_total_count"`
			WeeklyStartTime          int64    `json:"weekly_start_time"`
			WeeklyEndTime            int64    `json:"weekly_end_time"`
		} `json:"model_remains"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("no plan in the reply")
	}
	if env.BaseResp != nil && env.BaseResp.StatusCode != 0 {
		return nil, fmt.Errorf("%s", env.BaseResp.StatusMsg)
	}
	var out []Window
	for _, m := range env.ModelRemains {
		name := strings.TrimSpace(m.ModelName)
		if name == "" {
			continue
		}
		// A model whose two windows are both unlimited and both have no
		// count is a bucket the plan does not have.
		if isThree(m.CurrentIntervalStatus) && isThree(m.CurrentWeeklyStatus) &&
			isZeroCount(m.CurrentIntervalTotalCnt) && isZeroCount(m.CurrentWeeklyTotalCnt) {
			continue
		}
		general := strings.EqualFold(name, "general")
		for _, x := range []struct {
			status    *float64
			remaining *float64
			used      *float64
			total     *float64
			usedCnt   *float64
			totalCnt  *float64
			start     int64
			end       int64
			weekly    bool
		}{
			{m.CurrentIntervalStatus, m.CurrentIntervalRemaining, m.CurrentIntervalUsed, m.CurrentIntervalTotal,
				m.CurrentIntervalUsedCount, m.CurrentIntervalTotalCnt, m.StartTime, m.EndTime, false},
			{m.CurrentWeeklyStatus, m.CurrentWeeklyRemaining, m.CurrentWeeklyUsed, m.CurrentWeeklyTotal,
				m.CurrentWeeklyUsedCount, m.CurrentWeeklyTotalCnt, m.WeeklyStartTime, m.WeeklyEndTime, true},
		} {
			status := 0.0
			if x.status != nil {
				status = *x.status
			}
			if status == 3 {
				// An unlimited window is said so on the general model.
				if general {
					out = append(out, Window{Name: windowName(x.weekly), ModelName: name, Unlimited: true})
				}
				continue
			}
			if x.status == nil && x.remaining == nil && x.used == nil {
				continue
			}
			total := 100.0
			if x.total != nil && *x.total > 0 {
				total = *x.total
			}
			used := 0.0
			switch {
			case status == 2:
				used = 100
			case x.used != nil:
				used = clampPercent(*x.used / total * 100)
			case x.remaining != nil:
				used = clampPercent(100 - *x.remaining)
			}
			w := Window{Name: windowName(x.weekly), ModelName: name, Used: used}
			if x.end > 0 {
				w.ResetsAt = time.UnixMilli(x.end)
			}
			// The site counts the items a window has. A count that is
			// not there is zero, the way the reference reads it, so a
			// window with a limit and no count yet is "0 of 150" and
			// not "unknown".
			items, of := 0.0, 0.0
			if x.usedCnt != nil {
				items = *x.usedCnt
			}
			if x.totalCnt != nil {
				of = *x.totalCnt
			}
			if of > 0 && items <= of {
				w.Amount = items
				if status == 2 {
					// A spent window counts its whole limit.
					w.Amount = of
				}
				w.Limit = of
				if strings.EqualFold(name, "video") {
					w.Unit = "videos"
				}
			} else if total != 100 {
				w.Limit = total
				w.Amount = used * total / 100
				w.Unit = "%"
			}
			out = append(out, w)
		}
	}
	return out, nil
}

// isThree says whether a number is 3.
func isThree(v *float64) bool { return v != nil && *v == 3 }

// isZeroCount says whether a count is missing or zero.
func isZeroCount(v *float64) bool { return v == nil || *v == 0 }

func windowName(weekly bool) string {
	if weekly {
		return "7 days"
	}
	return "5 hours"
}

func clampPercent(f float64) float64 {
	if f < 0 {
		return 0
	}
	if f > 100 {
		return 100
	}
	return f
}
