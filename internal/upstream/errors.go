package upstream

import (
	"encoding/json"
	"errors"
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

// SchemeDowngradeError is a redirect that took the request from https
// to http.
//
// The host may well be the same one, and that is exactly what makes
// it dangerous: the Authorization header would go out in cleartext, and
// whoever is on the way can read it and replay it. A redirect that
// downgrades the scheme is refused, never followed.
type SchemeDowngradeError struct {
	// From is the address the request was sent to.
	From string
	// To is the address it was sent on to.
	To string
	// Status is the redirect's status code.
	Status int
}

// Error is the error's text.
func (e *SchemeDowngradeError) Error() string {
	return fmt.Sprintf("%s sent the request on to %s (HTTP %d), which downgrades the scheme to http. The sign-in would go out in cleartext, so the redirect is refused.", e.From, e.To, e.Status)
}

// StaleSignatureError is a redirect that would have replayed a
// signature made for another path or query.
//
// MiniMax signs each account API request: "yy" is computed over the
// path and the query, so a signature made for one path is not a
// signature for another. Replaying the old one is refused by the
// upstream, and that refusal reads as a dead token — a false one that
// costs a refresh token. The redirect is refused instead.
type StaleSignatureError struct {
	// From is the address the request was sent to.
	From string
	// To is the address it was sent on to.
	To string
	// Status is the redirect's status code.
	Status int
}

// Error is the error's text.
func (e *StaleSignatureError) Error() string {
	return fmt.Sprintf("%s sent the request on to %s (HTTP %d), which changes the path or the query. The signature is made over the path and the query, so it cannot be replayed; the redirect is refused.", e.From, e.To, e.Status)
}

// TooLargeError says a body was larger than it may be.
//
// 它是「上游发的不是回答」的一种：回包的大小是上游的事，站点答一个正经问
// 题不会超过它自己那个输出上限（524288 个 token，约 2 MB）。所以超了就是
// 有什么不对，不是「这个答案太长」。
type TooLargeError struct {
	// Limit is how many bytes the body may be.
	Limit int64
}

// Error is the error's text.
func (e *TooLargeError) Error() string {
	return fmt.Sprintf("the upstream's answer is larger than %d bytes", e.Limit)
}

// signInRefusedCode is the business code MiniMax answers with when it
// refuses the sign-in.
const signInRefusedCode = 1000048

// ErrSignInRefused says the upstream refused the sign-in.
//
// It is what errors.Is is used with to tell a stale token from any
// other business error.
var ErrSignInRefused = errors.New("the sign-in was refused")

// BusinessError is a 200 answer whose body says it failed.
//
// The body of a successful HTTP status carries MiniMax's own code, and
// that code is what says what went wrong. Keeping it makes it
// possible to react to one code and not to another.
type BusinessError struct {
	// What is what was asked for.
	What string
	// Code is the business code the body carried.
	Code int
	// Message is the text the body carried.
	Message string
}

// Error is the error's text.
func (e *BusinessError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("%s: status %d", e.What, e.Code)
	}
	return fmt.Sprintf("%s: %s", e.What, e.Message)
}

// Is says whether the error is the refusal of the sign-in.
func (e *BusinessError) Is(target error) bool {
	return target == ErrSignInRefused && e.Code == signInRefusedCode
}

// quotaPattern says a refusal is about the account's credits or plan
// running out.
var quotaPattern = regexp.MustCompile(`(?i)insufficient|balance|credit|quota|exhaust|limit|余额|积分|额度|不足|用完|上限`)

// rateLimitPattern says a body is about being sent too fast.
//
// It is taken out before the quota words are looked for: a rate limit
// is a different thing, the account is fine and the answer comes when
// the caller slows down. Rotating away from the account and marking
// its window spent would be the wrong answer.
var rateLimitPattern = regexp.MustCompile(`(?i)rate[\s_-]*limits?|too\s+many\s+requests|throttl\w*`)

// IsQuotaRefusal says whether a body is a refusal for running out of
// credits or plan.
//
// A rate limit is not: the account is fine and the answer comes when
// the caller slows down, so the rate-limit wording is taken out
// before the rest is looked for.
func IsQuotaRefusal(body []byte) bool {
	s := strings.ToLower(string(body))
	s = rateLimitPattern.ReplaceAllString(s, "")
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
// The error that comes back is a *BusinessError, which carries the
// code; errors.Is(err, ErrSignInRefused) tells a refused sign-in from
// any other business error.
func CheckEnvelope(what string, raw []byte) error {
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("%s: not JSON", what)
	}
	if si := env.StatusInfo; si != nil && si.Code != 0 {
		if si.Code == signInRefusedCode {
			return &BusinessError{
				What:    what,
				Code:    si.Code,
				Message: "the sign-in was refused",
			}
		}
		msg := si.Message
		if msg == "" {
			msg = si.Msg
		}
		if msg == "" {
			msg = fmt.Sprintf("status %d", si.Code)
		}
		return &BusinessError{What: what, Code: si.Code, Message: msg}
	}
	if br := env.BaseResp; br != nil && br.StatusCode != 0 {
		msg := br.StatusMsg
		if msg == "" {
			msg = fmt.Sprintf("status %d", br.StatusCode)
		}
		return &BusinessError{What: what, Code: br.StatusCode, Message: msg}
	}
	return nil
}
