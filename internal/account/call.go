package account

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
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
// it. What is signed is exactly what goes on the wire: a POST whose
// body was not given sends "{}" and signs "{}". The answer's business
// codes are checked before it is handed back.
func (c *Client) Call(ctx context.Context, access, userID, method, path string, body []byte, query []QueryKV) ([]byte, error) {
	now := c.now()
	// The reference signs the bytes it sends: a POST's body, or "{}"
	// when it has none. A signature over anything else is refused.
	wire := wireBody(method, body)
	signed := sign.Sign(c.base, path, sign.Options{
		Access:   access,
		UserID:   userID,
		Body:     wire,
		Query:    query,
		Now:      now.UnixMilli(),
		TZOffset: c.tzOffset,
		OSName:   c.osName,
		Lang:     site.Lang,
	})
	var req *http.Request
	var err error
	if method == http.MethodPost {
		req, err = http.NewRequestWithContext(ctx, method, signed.URL, strings.NewReader(string(wire)))
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

// wireBody is the body that goes on the wire and into the signature.
//
// The reference's signing sends a GET without a body and a POST with
// the body it is given, an empty one as "{}". The two have to be the
// same bytes: a signature made over something else is refused.
func wireBody(method string, body []byte) []byte {
	if method != http.MethodPost {
		return nil
	}
	if len(body) == 0 {
		return []byte("{}")
	}
	return body
}

// RefusedError is a refusal of the sign-in.
type RefusedError struct {
	// Status is the HTTP status the refusal came with.
	Status int
}

// Error is the error's text.
func (e *RefusedError) Error() string {
	return fmt.Sprintf("the sign-in was refused (HTTP %d)", e.Status)
}

// Is says whether the error is the upstream's refusal of the sign-in.
//
// A refused sign-in is one event, whatever shape it arrives in: an
// HTTP 401 or 403, or MiniMax's own code 1000048 in the body of a
// 200. errors.Is(err, upstream.ErrSignInRefused) tells it apart from
// every other error.
func (e *RefusedError) Is(target error) bool {
	return target == upstream.ErrSignInRefused
}

// isNotFound says an error is a 404.
func isNotFound(err error) bool {
	var se *statusError
	return errors.As(err, &se) && se.status == http.StatusNotFound
}

type statusError struct{ status int }

func (e *statusError) Error() string { return "HTTP " + strconv.Itoa(e.status) }

// parsePlanWindows reads the console's answer as windows.
//
// listed says the answer had a list of windows in it. The reference
// decides on that list and not on the windows it can make of it: an
// answer with a list is the console's answer, even when every entry in
// it is a bucket the plan does not have.
func parsePlanWindows(raw []byte) (windows []Window, listed bool, err error) {
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
		return nil, false, fmt.Errorf("no plan in the reply: %w", err)
	}
	if env.BaseResp != nil && env.BaseResp.StatusCode != 0 {
		// The reference names the status code when the answer has no
		// message of its own: an error with an empty text says nothing.
		msg := strings.TrimSpace(env.BaseResp.StatusMsg)
		if msg == "" {
			msg = fmt.Sprintf("MiniMax said %d", env.BaseResp.StatusCode)
		}
		return nil, false, errors.New(msg)
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
			// The window is named after its own length. A window of
			// three hours is "3 hours" and not "5 hours": the length is
			// the window's own, and the site does not always give
			// five.
			span := windowSpan(x.start, x.end, x.weekly)
			own := windowName(x.weekly, span)
			if status == 3 {
				// An unlimited window is said so on the general model.
				if general {
					out = append(out, Window{Name: own, ModelName: name, Unlimited: true})
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
			w := Window{Name: own, ModelName: name, Used: used}
			if x.end > 0 {
				// The end of the window is a time the site wrote, and
				// the site does not say in which unit: a number
				// below a trillion is seconds. The reference reads it
				// the same way, and the window's own length is read
				// that way too — so the two agree on how long the
				// window is.
				w.ResetsAt = time.UnixMilli(planMS(x.end))
			}
			// The site counts the items a window has. A count that is
			// not there is zero, the way the reference reads it, so a
			// window with a limit and no count yet is "0 of 150" and
			// not "unknown". A count that is negative is not a count
			// at all: the reference wants the count to be at least
			// zero, or the window has no limit to show.
			items, of := 0.0, 0.0
			if x.usedCnt != nil {
				items = *x.usedCnt
			}
			if x.totalCnt != nil {
				of = *x.totalCnt
			}
			if items >= 0 && of > 0 && items <= of {
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
	return out, len(env.ModelRemains) > 0, nil
}

// isThree says whether a number is 3.
func isThree(v *float64) bool { return v != nil && *v == 3 }

// isZeroCount says whether a count is missing or zero.
func isZeroCount(v *float64) bool { return v == nil || *v == 0 }

// windowSpan is how long a plan window is.
//
// The site writes the two ends of a window in seconds or in
// milliseconds, and the reference reads them the same way: a number
// that is too long to be a time in seconds is in milliseconds.
//
// A weekly window without times of its own is a week long. Every other
// window without times is of no length at all, and says so.
func windowSpan(start, end int64, weekly bool) time.Duration {
	ms := int64(0)
	switch {
	case start > 0 && end > start:
		ms = planMS(end) - planMS(start)
	case weekly:
		ms = 7 * 86400_000
	}
	return time.Duration(ms) * time.Millisecond
}

// planMS reads a time the site wrote.
//
// A time in seconds is shorter than a time in milliseconds by a
// thousand, and the site does not say which it wrote. The reference
// tells them apart by their length: a number below a trillion is in
// seconds.
func planMS(v int64) int64 {
	if v < 1e12 {
		return v * 1000
	}
	return v
}

// windowName is the name of a plan window.
//
// The name says how long the window is, and the length is the
// window's own: a window of three hours is "3 hours", and a window
// that is a day long is "24 hours" and not "1 days", the way the
// reference reads it.
//
// A weekly window is "7 days" whatever its times say, and a window
// without a length is "Allowance".
func windowName(weekly bool, span time.Duration) string {
	if weekly {
		return "7 days"
	}
	ms := span.Milliseconds()
	switch {
	case ms > 86400_000 && ms%86400_000 == 0:
		return fmt.Sprintf("%d days", ms/86400_000)
	case ms >= 3600_000 && ms%3600_000 == 0:
		return fmt.Sprintf("%d hours", ms/3600_000)
	case ms > 0:
		// The reference floors the minutes, and Go's division rounds
		// towards zero. The span is never negative here, but the
		// floor is what the reference does.
		return fmt.Sprintf("%d minutes", int64(math.Floor(float64(ms)/60_000)))
	default:
		return "Allowance"
	}
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
