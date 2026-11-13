package upstream

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// OtherSiteError is a redirect that left the host we asked.
//
// MiniMax's international hosts sit behind Akamai, which answers a
// request from a mainland-China network with a 307 to the China site.
// The two sites' tokens do not work on each other, so following such a
// redirect reads as a refused sign-in on every call. It is never
// followed: the caller gets this error instead, and no refresh token is
// spent on it.
type OtherSiteError struct {
	// From is the host that redirected.
	From string
	// To is the host it sent the request to.
	To string
	// Status is the redirect's status code.
	Status int
}

// Error is the error's text.
func (e *OtherSiteError) Error() string {
	return fmt.Sprintf("%s sent the request on to %s (HTTP %d), which doesn't take this site's sign-in. Set a proxy outside mainland China for it.", e.From, e.To, e.Status)
}

// quotaPattern says a refusal is about the account's credits or plan
// running out.
var quotaPattern = regexp.MustCompile(`(?i)insufficient|balance|credit|quota|exhaust|limit|余额|积分|额度|不足|用完|上限`)

// IsQuotaRefusal says whether a body is a refusal for running out of
// credits or plan.
//
// A rate limit is a different thing: the account is fine and the
// answer comes when the caller slows down. Rotating away from the
// account and marking its window spent would be the wrong answer, so
// the rate-limit words are taken out before the rest are looked for.
func IsQuotaRefusal(body []byte) bool {
	s := strings.ToLower(string(body))
	for _, w := range []string{"rate limit", "rate_limit", "too many requests"} {
		s = strings.ReplaceAll(s, w, "")
	}
	return quotaPattern.MatchString(s)
}

// envelope is the shape MiniMax's account API answers with.
type envelope struct {
	StatusInfo *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Msg     string `json:"msg"`
	} `json:"statusInfo"`
	BaseResp *struct {
		StatusCode int    `json:"status_code"`
		StatusMsg  string `json:"status_msg"`
	} `json:"base_resp"`
}

// CheckEnvelope throws when a 200 answer says in its body that it
// failed.
//
// A successful HTTP status is not a successful answer here: MiniMax's
// gateway checks its own business codes and puts a refusal in the body.
func CheckEnvelope(what string, raw []byte) error {
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("%s: not JSON", what)
	}
	if si := env.StatusInfo; si != nil && si.Code != 0 {
		if si.Code == 1000048 {
			return fmt.Errorf("%s: the sign-in was refused", what)
		}
		msg := si.Message
		if msg == "" {
			msg = si.Msg
		}
		if msg == "" {
			msg = fmt.Sprintf("status %d", si.Code)
		}
		return fmt.Errorf("%s: %s", what, msg)
	}
	if br := env.BaseResp; br != nil && br.StatusCode != 0 {
		msg := br.StatusMsg
		if msg == "" {
			msg = fmt.Sprintf("status %d", br.StatusCode)
		}
		return fmt.Errorf("%s: %s", what, msg)
	}
	return nil
}
