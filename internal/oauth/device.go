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
	"net/url"
	"strings"
	"time"

	"mmxcode/internal/site"
	"mmxcode/internal/upstream"
)

// DeviceGrant is the grant type of the device flow.
const DeviceGrant = site.DeviceGrant

// signInTimeout is how long a device code is waited for when the
// answer gives no end of its own.
const signInTimeout = 10 * time.Minute

// slowDownWait is how much longer to wait when the site says slow
// down. It is a variable so that a test does not have to wait five
// seconds to see it happen.
var slowDownWait = 5 * time.Second

// expiresDuration reads an expires_in that may be seconds or
// milliseconds.
//
// The site sends both: a token that lasts an hour comes as 3600, and a
// token that lasts an hour in milliseconds comes as 3600000. Read as
// seconds, the second one lasts a hundred years: the token never
// ends, the account is never refreshed, and every request is answered
// with 401.
//
// A duration in seconds that is over a million is longer than eleven
// days, which no token is; that is the tell.
func expiresDuration(v int64) time.Duration {
	if v <= 0 {
		return 0
	}
	if v > 1e6 {
		return time.Duration(v) * time.Millisecond
	}
	return time.Duration(v) * time.Second
}

// Error is an answer from MiniMax's sign-in server.
type Error struct {
	// Code is the "error" the answer carried.
	Code string
	// HTTPStatus is the status the answer came with.
	HTTPStatus int
	// Message is the text to show.
	Message string
}

// Error is the error's text.
//
// The code comes first: it is what says what went wrong, and the
// message is the site's own words for it.
func (e *Error) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("%s: %s", e.Code, e.Message)
	}
	if e.HTTPStatus != 0 {
		return fmt.Sprintf("MiniMax's sign-in server said %s (HTTP %d)", e.Code, e.HTTPStatus)
	}
	return fmt.Sprintf("MiniMax's sign-in server said %s", e.Code)
}

// IsSignedOut says the answer is the one that signs an account out.
//
// Only invalid_grant with HTTP 400 is a sign-out. Any other failure is
// passing, and the sign-in is kept.
func (e *Error) IsSignedOut() bool {
	return e.Code == "invalid_grant" && e.HTTPStatus == 400
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
	// AccountID is the account id the access token carries.
	AccountID string
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
		return nil, oauthError(resp.StatusCode, body)
	}
	var b struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURL         string `json:"verification_url"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               int64  `json:"expires_in"`
		ExpiredIn               int64  `json:"expired_in"`
		Interval                int64  `json:"interval"`
	}
	if err := json.Unmarshal(body, &b); err != nil {
		return nil, &Error{Code: "invalid_json_response", HTTPStatus: resp.StatusCode}
	}
	userCode := strings.TrimSpace(b.UserCode)
	deviceCode := strings.TrimSpace(b.DeviceCode)
	uri := strings.TrimSpace(b.VerificationURI)
	if uri == "" {
		uri = strings.TrimSpace(b.VerificationURL)
	}
	if userCode == "" || uri == "" {
		return nil, &Error{Code: "invalid_device_authorization_response", HTTPStatus: resp.StatusCode}
	}

	d := &Device{UserCode: userCode, verifier: verifier}
	// MiniMax's own variant has no device_code: the user code is polled
	// for, and the end may be an absolute time in milliseconds.
	byUser := deviceCode == "" && b.ExpiredIn != 0
	switch {
	case byUser:
		d.pollField, d.pollValue = "user_code", userCode
	case deviceCode != "":
		d.pollField, d.pollValue = "device_code", deviceCode
	default:
		return nil, &Error{Code: "invalid_device_authorization_response", HTTPStatus: resp.StatusCode}
	}

	var expires time.Duration
	switch {
	case b.ExpiresIn > 0:
		expires = expiresDuration(b.ExpiresIn)
	case b.ExpiredIn > 0:
		// expired_in 有时是还剩多少秒，有时是一个绝对的毫秒时间戳。
		// 靠大小分：毫秒时间戳不会是 1970 年。
		if b.ExpiredIn < 1e12 {
			expires = time.Duration(b.ExpiredIn) * time.Second
		} else {
			expires = time.Until(time.UnixMilli(b.ExpiredIn))
		}
	}
	if expires <= 0 {
		expires = signInTimeout
	}
	d.deadline = time.Now().Add(expires)

	interval := 5 * time.Second
	if b.Interval > 0 {
		// The site sends the interval in seconds and in milliseconds.
		// A poll that waits more than a minute is not a poll, so a
		// number that big is milliseconds.
		if b.Interval > 60 {
			interval = time.Duration(b.Interval) * time.Millisecond
		} else {
			interval = time.Duration(b.Interval) * time.Second
		}
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
			return nil, &Error{Code: "expired_token"}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
		if time.Now().After(d.deadline) {
			return nil, &Error{Code: "expired_token"}
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
			return nil, &Error{Code: "invalid_json_response", HTTPStatus: resp.StatusCode}
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
			wait += slowDownWait
			continue
		}
		if !httpOK {
			return nil, oauthError(resp.StatusCode, body)
		}
		switch status {
		case "pending":
			continue
		case "slow_down":
			wait += slowDownWait
			continue
		case "denied", "access_denied":
			return nil, &Error{Code: "access_denied"}
		case "expired", "expired_token":
			return nil, &Error{Code: "expired_token"}
		}
		if b.Error != "" {
			return nil, oauthError(resp.StatusCode, body)
		}
		if b.AccessToken == "" || b.ExpiresIn <= 0 {
			return nil, &Error{Code: "invalid_token_response", HTTPStatus: resp.StatusCode}
		}
		tok := &Token{
			Access:  b.AccessToken,
			Refresh: b.RefreshToken,
			Expires: time.Now().Add(expiresDuration(b.ExpiresIn)),
		}
		tok.Subject, tok.AccountID = claims(b.AccessToken)
		return tok, nil
	}
}

// claims reads the JWT's payload, when the access token is one.
func claims(access string) (subject, accountID string) {
	parts := strings.Split(access, ".")
	if len(parts) < 2 {
		return "", ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", ""
	}
	var p struct {
		Sub       string `json:"sub"`
		AccountID string `json:"account_id"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", ""
	}
	return p.Sub, p.AccountID
}

// oauthError reads an error answer.
func oauthError(status int, body []byte) *Error {
	var b struct {
		Error       string `json:"error"`
		ErrorDesc   string `json:"error_description"`
		Description string `json:"description"`
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
	return &Error{Code: code, HTTPStatus: status, Message: msg}
}

// randomVerifier makes the PKCE verifier.
func randomVerifier() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
