// Package oauth signs in to MiniMax Code with its own device flow.
//
// The flow is OAuth 2's device flow with PKCE, client "mcode-public":
// the user opens MiniMax's page, types a code, and the plugin polls
// until the sign-in is done. Nothing of MiniMax Code's own sign-in
// (~/.minimax) is read or written.
package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/lingoo12138/mmxcode/internal/site"
	"github.com/lingoo12138/mmxcode/internal/upstream"
)

// DeviceGrant is the grant type of the device flow.
const DeviceGrant = site.DeviceGrant

// What an answer came from.
//
// The text of an error says which endpoint answered, so a failure of
// the refresh does not read as a failure of the sign-in.
const (
	// WhatSignIn is the device flow: the sign-in server.
	WhatSignIn = "sign-in"
	// WhatRefresh is the token endpoint, where a refresh token is
	// traded in.
	WhatRefresh = "refresh"
)

// signInTimeout is how long a device code is waited for when the
// answer gives no end of its own.
const signInTimeout = 10 * time.Minute

// slowDownWait is how much longer to wait when the site says slow
// down.
//
// It is a constant and not a variable: a test hook that production
// behaviour depends on is one t.Parallel() away from a race, and a
// test that needs a shorter wait sets the field on its own device.
const slowDownWait = 5 * time.Second

// The units of the numbers the site gives.
//
// The site is not consistent about the unit: some of its answers are in
// seconds and some in milliseconds, and nothing in the answer says
// which. One rule is used for every number, and it lives in one place:
//
//	a number that is longer than the longest length of time the field
//	can plausibly hold is in milliseconds; every other number is in
//	seconds.
//
// The longest plausible length is given by the field that reads the
// number, so the rule is applied the same way everywhere and every
// field says what it believes.
const (
	// maxTokenSeconds is the longest an access token can live: a day.
	// A number longer than that is not a number of seconds.
	maxTokenSeconds = 24 * 60 * 60
	// epochSecondsFloor is the first Unix second of this century
	// (2001-09-09). A number above it is a timestamp, not a length of
	// time.
	epochSecondsFloor = 1_000_000_000
	// epochMillisFloor is the first Unix millisecond of a time in this
	// century (1973-03-03). A timestamp above it is in milliseconds.
	epochMillisFloor = 100_000_000_000
)

// siteDuration reads a length of time the site gave.
//
// The number is in seconds, unless it is longer than the longest
// length of time the field can plausibly hold, in which case it is in
// milliseconds. See the unit rules above.
func siteDuration(v int64, longestSeconds int64) time.Duration {
	if v <= 0 {
		return 0
	}
	if v > longestSeconds {
		return time.Duration(v) * time.Millisecond
	}
	return time.Duration(v) * time.Second
}

// expiresDuration reads how long an access token lives.
//
// The site sends an hour as 3600 and an hour as 3600000; the rule in
// siteDuration tells the two apart. A ten-minute token sent as 600000
// is ten minutes and not a hundred and sixty-six hours.
func expiresDuration(v int64) time.Duration {
	return siteDuration(v, maxTokenSeconds)
}

// pollInterval reads how long to wait between two polls.
//
// The ordinary device-code flow says the interval in seconds. The
// reference reads it that way, and so do we: 5 is five seconds, and
// 3000 is fifty minutes, not three seconds. The site's own variant
// says the interval in milliseconds and is read separately, in
// StartDevice.
func pollInterval(v int64) time.Duration {
	return time.Duration(v) * time.Second
}

// siteDeadline reads when a device code ends.
//
// The site gives either a remaining time or an absolute timestamp, and
// the timestamp comes in seconds or in milliseconds. The rule: a
// number at or below epochSecondsFloor is a remaining time — in
// seconds, or in milliseconds when it is too long to be seconds — and a
// bigger one is an absolute timestamp, in milliseconds when it is above
// epochMillisFloor and in seconds otherwise.
func siteDeadline(v int64) (time.Time, bool) {
	if v <= 0 {
		return time.Time{}, false
	}
	if v > epochSecondsFloor {
		if v > epochMillisFloor {
			return time.UnixMilli(v), true
		}
		return time.Unix(v, 0), true
	}
	return time.Now().Add(siteDuration(v, maxTokenSeconds)), true
}

// Error is an answer from MiniMax.
type Error struct {
	// Code is the "error" the answer carried.
	Code string
	// HTTPStatus is the status the answer came with.
	HTTPStatus int
	// Message is the text to show.
	Message string
	// What says which endpoint answered: WhatSignIn for the device
	// flow, WhatRefresh for the token endpoint.
	What string
}

// Error is the error's text.
//
// The code comes first: it is what says what went wrong, and the
// message is the site's own words for it.
func (e *Error) Error() string {
	who := "MiniMax's sign-in server"
	if e.What == WhatRefresh {
		who = "MiniMax's token endpoint"
	}
	if e.Message != "" {
		return fmt.Sprintf("%s said %s: %s", who, e.Code, e.Message)
	}
	if e.HTTPStatus != 0 {
		return fmt.Sprintf("%s said %s (HTTP %d)", who, e.Code, e.HTTPStatus)
	}
	return fmt.Sprintf("%s said %s", who, e.Code)
}

// IsSignedOut says the answer is the one that signs an account out.
//
// The sign-out is invalid_grant with a 400, the way MiniMax Code
// reads it. The status is part of the answer and not a detail: a 401
// or a 500 that carries the same code is passing, and the sign-in is
// kept.
//
// A sign-out is expensive to get wrong — the account loses its refresh
// token chain — so the answer has to be the one the site means.
func (e *Error) IsSignedOut() bool {
	return e.Code == "invalid_grant" && e.HTTPStatus == http.StatusBadRequest
}

// Token is an account's sign-in.
type Token struct {
	// Access is the access token.
	Access string
	// Refresh is the refresh token. It works once.
	Refresh string
	// Expires is when the access token ends.
	Expires time.Time
	// Subject is the MiniMax user id the access token carries.
	Subject string
	// AccountID is the account id the access token carries, as the
	// site says it.
	//
	// It is the site's own id and not the store's: the renewer is
	// keyed by the store's account id, and the two are not the same
	// thing. A caller that keyed the renewer by this id would walk a
	// second renewal chain for one account and spend two refresh
	// tokens, which signs the account out. The store's id is the one
	// to use.
	//
	// The name is kept as it is: renaming an exported field breaks
	// every caller that reads it, and nothing reads it today. Use
	// SiteAccountID to make the difference plain.
	AccountID string
	// HasClaims says the access token's claims could be read.
	//
	// It is false for a token that is not a JWT and for one whose
	// payload cannot be decoded. The claim names themselves are
	// guesses, so a true here does not mean they are the right ones.
	HasClaims bool
}

// SiteAccountID is the account id the site's own token carries.
//
// It is not the store's account id, which is what the renewer is keyed
// by. See Token.AccountID.
func (t *Token) SiteAccountID() string {
	if t == nil {
		return ""
	}
	return t.AccountID
}

// Device is a sign-in waiting for the user.
type Device struct {
	// UserCode is the code the user types.
	UserCode string
	// URL is where the user goes.
	URL string

	verifier  string
	pollField string
	pollValue string
	deadline  time.Time
	interval  time.Duration
	slowDown  time.Duration
}

// String is the device's text.
//
// The PKCE verifier is a secret: it is what lets anybody holding it
// finish this sign-in. So it is not in the text, and %+v prints this
// instead of the fields, so a debug log cannot leak it.
func (d Device) String() string {
	return fmt.Sprintf("Device{UserCode:%q URL:%q}", d.UserCode, d.URL)
}

// StartDevice asks MiniMax for a sign-in code.
//
// The caller gives the device-code endpoint, so a test can point it at a
// stand-in.
func StartDevice(ctx context.Context, c *upstream.Client, endpoint string) (*Device, error) {
	verifier, err := randomVerifier()
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	form := url.Values{}
	form.Set("client_id", site.ClientID)
	form.Set("scope", site.Scope)
	form.Set("audience", site.Audience)
	form.Set("code_challenge", challenge)
	form.Set("code_challenge_method", "S256")

	resp, err := c.PostForm(ctx, endpoint, form)
	if err != nil {
		return nil, fmt.Errorf("device code: %w", err)
	}
	body, err := upstream.ReadBody(resp)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, oauthError(resp.StatusCode, body, WhatSignIn)
	}
	var b struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURL         string `json:"verification_url"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               int64  `json:"expires_in"`
		// ExpiredIn is a pointer, so that an answer without the field
		// is told from an answer whose field is zero. The two are not
		// the same: the field says the site is polling for the user
		// code, and a zero is a field that is there.
		//
		// A zero, though, is not a field the site means: the reference
		// reads it as absent (its pos() only takes a value above
		// zero), and so do we.
		ExpiredIn *int64 `json:"expired_in"`
		Interval  int64  `json:"interval"`
	}
	if err := json.Unmarshal(body, &b); err != nil {
		return nil, &Error{Code: "invalid_json_response", HTTPStatus: resp.StatusCode, What: WhatSignIn}
	}
	userCode := strings.TrimSpace(b.UserCode)
	deviceCode := strings.TrimSpace(b.DeviceCode)
	uri := strings.TrimSpace(b.VerificationURI)
	if uri == "" {
		uri = strings.TrimSpace(b.VerificationURL)
	}
	if userCode == "" || uri == "" {
		return nil, &Error{Code: "invalid_device_authorization_response", HTTPStatus: resp.StatusCode, What: WhatSignIn}
	}

	d := &Device{UserCode: userCode, verifier: verifier, slowDown: slowDownWait}
	// MiniMax's own variant has no device_code: the user code is polled
	// for, and the end may be an absolute time in milliseconds.
	//
	// The field being there is what says so, and the user code has to
	// be there to be polled for. A field that is not there at all is
	// not a user-code poll, however the answer looks. A field that is
	// there but says zero says nothing: the reference reads it as
	// absent, and a zero end is not an end.
	byUser := deviceCode == "" && userCode != "" && b.ExpiredIn != nil && *b.ExpiredIn > 0
	switch {
	case byUser:
		d.pollField, d.pollValue = "user_code", userCode
	case deviceCode != "":
		d.pollField, d.pollValue = "device_code", deviceCode
	default:
		return nil, &Error{Code: "invalid_device_authorization_response", HTTPStatus: resp.StatusCode, What: WhatSignIn}
	}

	// The end of the code comes in two shapes: a lifetime (expires_in)
	// and a deadline (expired_in). The fallback is only for the case
	// where the site said nothing at all: a deadline that is already
	// in the past is a dead code, and waiting ten minutes for it would
	// be waiting for nothing.
	var deadline time.Time
	switch {
	case b.ExpiresIn > 0:
		deadline = time.Now().Add(expiresDuration(b.ExpiresIn))
	case b.ExpiredIn != nil && *b.ExpiredIn > 0:
		deadline, _ = siteDeadline(*b.ExpiredIn)
	}
	if deadline.IsZero() {
		deadline = time.Now().Add(signInTimeout)
	}
	if !deadline.After(time.Now()) {
		return nil, &Error{Code: "expired_token", What: WhatSignIn}
	}
	d.deadline = deadline

	// The wait between two polls. The site's own variant says the
	// interval in milliseconds and the ordinary one in seconds, and
	// the reference reads each one in its own unit: 3000 is three
	// seconds on one path and three thousand milliseconds on the
	// other.
	interval := 5 * time.Second
	switch {
	case b.Interval <= 0:
		// 没有这个字段（或者是零）：五秒一轮。
	case byUser:
		interval = time.Duration(b.Interval) * time.Millisecond
	default:
		interval = pollInterval(b.Interval)
	}
	d.interval = interval

	complete := strings.TrimSpace(b.VerificationURIComplete)
	if complete != "" {
		d.URL = complete
	} else {
		d.URL = uri
	}
	return d, nil
}

// Poll waits for the sign-in to finish.
//
// It answers when the sign-in is done, when it is turned down, or when
// the code runs out.
func (d *Device) Poll(ctx context.Context, c *upstream.Client, endpoint string) (*Token, error) {
	wait := d.interval
	for {
		if time.Now().After(d.deadline) {
			return nil, &Error{Code: "expired_token", What: WhatSignIn}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(clampToDeadline(wait, d.deadline)):
		}
		if time.Now().After(d.deadline) {
			return nil, &Error{Code: "expired_token", What: WhatSignIn}
		}

		form := url.Values{}
		form.Set("grant_type", DeviceGrant)
		form.Set(d.pollField, d.pollValue)
		form.Set("client_id", site.ClientID)
		form.Set("code_verifier", d.verifier)

		resp, err := c.PostForm(ctx, endpoint, form)
		if err != nil {
			return nil, fmt.Errorf("device token: %w", err)
		}
		body, err := upstream.ReadBody(resp)
		if err != nil {
			return nil, err
		}

		var b struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			ExpiresIn    int64  `json:"expires_in"`
			Error        string `json:"error"`
			Status       string `json:"status"`
		}
		if err := json.Unmarshal(body, &b); err != nil {
			return nil, &Error{Code: "invalid_json_response", HTTPStatus: resp.StatusCode, What: WhatSignIn}
		}
		httpOK := resp.StatusCode >= 200 && resp.StatusCode <= 299
		code := strings.ToLower(b.Error)
		status := strings.ToLower(b.Status)

		// 用户还没点授权是正常状态，不是错误。RFC 8628 就是这么说的：
		// token 端点用 400 加 authorization_pending 回答。这不能当失败，
		// 得接着等 —— 当成失败的话，每一次登录都死在第一轮轮询上。
		if code == "authorization_pending" {
			continue
		}
		if code == "slow_down" {
			wait += d.slowDownWait()
			continue
		}
		if !httpOK {
			return nil, oauthError(resp.StatusCode, body, WhatSignIn)
		}
		switch status {
		case "pending":
			continue
		case "slow_down":
			wait += d.slowDownWait()
			continue
		case "denied", "access_denied":
			return nil, &Error{Code: "access_denied", What: WhatSignIn}
		case "expired", "expired_token":
			return nil, &Error{Code: "expired_token", What: WhatSignIn}
		}
		if b.Error != "" {
			return nil, oauthError(resp.StatusCode, body, WhatSignIn)
		}
		if b.AccessToken == "" || b.ExpiresIn <= 0 {
			return nil, &Error{Code: "invalid_token_response", HTTPStatus: resp.StatusCode, What: WhatSignIn}
		}
		if b.RefreshToken == "" {
			// 没有 refresh token 的账号活不过一小时：它再也换不到
			// 新的 access token 了。与其建一个这样的账号，不如让这次
			// 登录失败，用户再登一次。
			return nil, &Error{Code: "missing_refresh_token", HTTPStatus: resp.StatusCode, What: WhatSignIn}
		}
		tok := &Token{
			Access:  b.AccessToken,
			Refresh: b.RefreshToken,
			Expires: time.Now().Add(expiresDuration(b.ExpiresIn)),
		}
		tok.Subject, tok.AccountID, tok.HasClaims = claims(b.AccessToken)
		return tok, nil
	}
}

// clampToDeadline cuts a wait down to the time the sign-in code has
// left.
//
// The reference sleeps for the shorter of the interval and the time
// left, and then gives up if the code is over:
//
//	await sleep(Math.min(delay, Math.max(0, d.deadline - Date.now())))
//	if (Date.now() >= d.deadline) break
//
// So a code that says "this one is good for ten minutes" but "come
// back in fifty" is told at ten minutes that it is over. Sleeping the
// whole interval would keep a caller waiting long past the point
// where the code could still be approved — with a background context
// that is fifty minutes of nothing, and with a shorter context a
// cancellation instead of the "expired" the reference reports.
func clampToDeadline(wait time.Duration, deadline time.Time) time.Duration {
	left := time.Until(deadline)
	if left <= 0 {
		return 0
	}
	if wait > left {
		return left
	}
	return wait
}

// slowDownWait is how much longer to wait when the site says slow
// down.
func (d *Device) slowDownWait() time.Duration {
	if d.slowDown > 0 {
		return d.slowDown
	}
	return slowDownWait
}

// claims reads the JWT's payload, when the access token is one.
//
// The claim names are guesses: nobody has shown what MiniMax puts in
// its access tokens, so "sub" and "account_id" may be the wrong names,
// and the claim may be missing from a token that is a JWT all the
// same. The answer says whether the payload could be read at all, so a
// caller can tell "not a JWT" from "a JWT without that claim". The
// names have to be checked against a real token before they are
// trusted.
//
// The signature is not checked either: the token is only read, never
// trusted for anything.
func claims(access string) (subject, accountID string, ok bool) {
	parts := strings.Split(access, ".")
	if len(parts) < 2 {
		return "", "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// A payload may be padded, and a padded one is a JWT all the
		// same.
		raw, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return "", "", false
		}
	}
	var p struct {
		Sub       string `json:"sub"`
		AccountID string `json:"account_id"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", "", false
	}
	return p.Sub, p.AccountID, true
}

// oauthError reads an error answer.
//
// The text is taken from error_description, then description, then
// message: a 4xx that carries only "message" would lose its
// explanation otherwise, and that explanation is what the account's
// last error says.
func oauthError(status int, body []byte, what string) *Error {
	var b struct {
		Error       string `json:"error"`
		ErrorDesc   string `json:"error_description"`
		Description string `json:"description"`
		Message     string `json:"message"`
	}
	_ = json.Unmarshal(body, &b)
	code := b.Error
	if code == "" {
		code = "oauth_request_failed"
	}
	msg := b.ErrorDesc
	if msg == "" {
		msg = b.Description
	}
	if msg == "" {
		msg = b.Message
	}
	return &Error{Code: code, HTTPStatus: status, Message: msg, What: what}
}

// randomVerifier makes the PKCE verifier.
func randomVerifier() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
