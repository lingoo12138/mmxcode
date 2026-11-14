// Package webauth signs in to MiniMax's account site the way the web
// page does.
//
// The device flow the CLI uses needs a person to approve the code in
// a browser. This package does the browser's part without a browser:
// it signs in with an email and a password, keeps the session cookie
// the site hands back, and uses that session to approve the code.
//
// The steps it takes are the ones the web page takes:
//   - register a device, so that there is a device number to send;
//   - ask the site which way this account signs in;
//   - sign in with the password, encrypted with the site's own key;
//   - approve the device code with the session that sign-in gave.
//
// The evidence for each step is in the design document.
package webauth

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"time"

	"mmxcode/internal/sign"
	"mmxcode/internal/site"
)

// Paths of the calls the login makes.
const (
	// DeviceRegisterPath registers the device.
	DeviceRegisterPath = "/v1/api/user/device/register"
	// LoginPath signs in.
	LoginPath = "/oauth2/login"
	// DeviceAuthorizePath shows and approves a device code.
	DeviceAuthorizePath = "/oauth2/device/authorize"
)

// AppID is the application the device code belongs to.
const AppID = "3001"

// LoginTypePassword is the site's "email and password" way in.
const LoginTypePassword = "20"

// Options is how the client reaches the site.
type Options struct {
	// BaseURL is the account site's origin.
	BaseURL string
	// HTTP is the client to use. It carries the proxy. A nil one is
	// a client with a thirty second timeout.
	HTTP *http.Client
	// Now is the clock. A nil one is time.Now.
	Now func() time.Time
	// TZOffset is the client's timezone offset in seconds.
	TZOffset int
	// Lang is the site's language.
	Lang string
}

// Client talks to the account site as the web page does.
type Client struct {
	base string
	http *http.Client
	now  func() time.Time
	tz   int
	lang string

	// uuid is the long-lived device id the browser keeps in its
	// local storage, and deviceID is the number the site handed out.
	uuid     string
	deviceID string
}

// New makes a client.
//
// The client gets its own cookie jar: the session that signs in
// belongs to one account, and two accounts must never share one.
func New(o Options) *Client {
	c := &Client{
		base: strings.TrimRight(o.BaseURL, "/"),
		now:  o.Now,
		tz:   o.TZOffset,
		lang: o.Lang,
		uuid: newUUID(),
	}
	if c.now == nil {
		c.now = time.Now
	}
	if c.lang == "" {
		c.lang = site.Lang
	}
	hc := o.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		jar = nil
	}
	clone := *hc
	clone.Jar = jar
	c.http = &clone
	return c
}

// DeviceID is the number the site handed out for this device.
func (c *Client) DeviceID() string { return c.deviceID }

// RegisterDevice tells the site about this device.
//
// The browser does it once and keeps the number; the site puts it in
// the fingerprint it sends with every request afterwards.
func (c *Client) RegisterDevice(ctx context.Context) (string, error) {
	if c.uuid == "" {
		c.uuid = newUUID()
	}
	body, err := jsonBody(map[string]any{"uuid": c.uuid})
	if err != nil {
		return "", err
	}
	// The device registration is the one call whose own query is the
	// uuid (design document 3.2). The web client puts the caller's
	// query first, so the url starts with it.
	s := c.sign(DeviceRegisterPath, body, []sign.KV{{K: "uuid", V: c.uuid}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	c.setHeaders(req, s)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("device register: %w", err)
	}
	defer resp.Body.Close()
	var answer struct {
		Data struct {
			DeviceIDStr string `json:"deviceIDStr"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&answer); err != nil {
		return "", fmt.Errorf("device register: %w", err)
	}
	if answer.Data.DeviceIDStr == "" {
		return "", errors.New("device register: the site gave no device number")
	}
	c.deviceID = answer.Data.DeviceIDStr
	return c.deviceID, nil
}

// Probe asks the site which way this account signs in.
//
// The site answers with a code. Zero means the account has a password
// and can sign in with it; the other codes say what is missing.
func (c *Client) Probe(ctx context.Context, email, deviceID, redirect string) error {
	_, err := c.login(ctx, email, nil, deviceID, redirect)
	return err
}

// Password signs in with a password.
//
// The password is encrypted with the site's own public key, the way
// the web page encrypts it, and the site answers with a session
// cookie.
func (c *Client) Password(ctx context.Context, email, password, deviceID, redirect string) error {
	token, err := encryptPassword(password)
	if err != nil {
		return err
	}
	_, err = c.login(ctx, email, &token, deviceID, redirect)
	return err
}

// login posts to the site's login endpoint.
//
// A nil authToken is the probe: it asks the site how this account
// signs in. A token is the password, encrypted with the site's key.
func (c *Client) login(ctx context.Context, email string, authToken *string, deviceID, redirect string) (map[string]any, error) {
	body := map[string]any{
		"loginType":      LoginTypePassword,
		"email":          email,
		"authToken":      nil,
		"deviceID":       deviceID,
		"login_redirect": redirect,
	}
	if authToken != nil {
		body["authToken"] = *authToken
		body["countryCode"] = "+86"
	}
	raw, err := jsonBody(body)
	if err != nil {
		return nil, err
	}
	s := c.sign(LoginPath, raw, nil)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	c.setHeaders(req, s)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("login: %w", err)
	}
	defer resp.Body.Close()
	var answer struct {
		Code       int            `json:"code"`
		Data       map[string]any `json:"data"`
		StatusInfo *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"statusInfo"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&answer); err != nil {
		return nil, fmt.Errorf("login: %w", err)
	}
	if answer.StatusInfo != nil {
		if answer.StatusInfo.Code != 0 {
			return nil, &SiteError{Code: answer.StatusInfo.Code, Message: answer.StatusInfo.Message}
		}
		return answer.Data, nil
	}
	// No statusInfo: the site answered with the top-level code, which
	// is how the mcode side refuses a sign-in (1000048).
	if answer.Code != 0 {
		return nil, &SiteError{Code: answer.Code}
	}
	return answer.Data, nil
}

// Approve lets the device code in.
//
// The session that Password got is the permission: without it the
// site answers 401 and says a login is needed.
//
// The approve call is not signed and carries no common query: the web
// page makes it with the browser's native fetch, which does not go
// through the interceptor that signs the rest.
func (c *Client) Approve(ctx context.Context, userCode string) error {
	group, err := c.defaultGroup(ctx, userCode)
	if err != nil {
		return err
	}
	body, err := jsonBody(map[string]any{
		"user_code": userCode,
		"decision":  "approve",
		"group_id":  group,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+DeviceAuthorizePath, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("approve: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return errors.New("approve: the session is not good any more")
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// A refusal that is not a refusal must not look like an
		// approval: the caller would go on waiting for a token that
		// is never going to come.
		return fmt.Errorf("approve: the site answered %s", resp.Status)
	}
	var answer struct {
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
		Message          string `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&answer); err == nil {
		if answer.Error != "" {
			msg := answer.ErrorDescription
			if msg == "" {
				msg = answer.Message
			}
			return &SiteError{Message: msg, Reason: answer.Error}
		}
	}
	return nil
}

// defaultGroup asks the site which group the code belongs to.
//
// This call is not signed either, for the same reason the approve is
// not.
func (c *Client) defaultGroup(ctx context.Context, userCode string) (string, error) {
	// The query is the one the web page sends, in its order. This call
	// is not signed, so the order does not matter to the signature.
	query := "user_code=" + url.QueryEscape(userCode) + "&app_id=" + AppID
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.base+DeviceAuthorizePath+"?"+query, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("device authorize: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return "", errors.New("device authorize: the site wants a login")
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", fmt.Errorf("device authorize: the site answered %s", resp.Status)
	}
	var answer any
	if err := json.NewDecoder(resp.Body).Decode(&answer); err != nil {
		return "", fmt.Errorf("device authorize: %w", err)
	}
	groups := findGroups(answer)
	for _, g := range groups {
		if def, ok := g["is_default"].(bool); ok && def {
			if id, ok := g["id"].(string); ok && id != "" {
				return id, nil
			}
		}
	}
	return "", errors.New("device authorize: there is no default group to approve")
}

// findGroups finds the groups in the site's answer.
//
// The shape of the answer is not documented, so the groups are looked
// for anywhere in it: any array whose entries have an "id" and an
// "is_default".
func findGroups(v any) []map[string]any {
	var out []map[string]any
	var walk func(any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			for _, child := range t {
				walk(child)
			}
		case []any:
			for _, child := range t {
				walk(child)
			}
			items := make([]map[string]any, 0, len(t))
			for _, item := range t {
				m, ok := item.(map[string]any)
				if !ok {
					return
				}
				if _, hasID := m["id"]; !hasID {
					return
				}
				items = append(items, m)
			}
			if len(items) > 0 {
				out = append(out, items...)
			}
		}
	}
	walk(v)
	return out
}

// sign makes a signed request for the web flavour.
//
// The query is the caller's own query, which the web client puts in
// front of the common one.
func (c *Client) sign(path string, body []byte, query []sign.KV) sign.Signed {
	return sign.Sign(c.base, path, sign.Options{
		Flavor:   sign.FlavorWeb,
		Now:      c.now().UnixMilli(),
		TZOffset: c.tz,
		Lang:     c.lang,
		Body:     body,
		UUID:     c.uuid,
		DeviceID: c.deviceID,
		Query:    query,
	})
}

// setHeaders puts the signed headers on a request.
func (c *Client) setHeaders(req *http.Request, s sign.Signed) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("yy", s.YY)
	req.Header.Set("x-timestamp", s.Timestamp)
	req.Header.Set("x-signature", s.Signature)
}

// SiteError is an answer the site refused with.
type SiteError struct {
	// Code is the site's own code.
	Code int
	// Message is what the site said.
	Message string
	// Reason is the site's own word for the refusal ("login_required"
	// and the like). A refusal that comes without a number of its own
	// carries the word, and that is what ExplainReason explains.
	Reason string
}

// ExplainText says what the refusal means, in a human's words.
//
// It is Explain for a code the site gave, and ExplainReason for a
// refusal that came without one. There is always a Chinese sentence
// here: the site's own words are English and say nothing a person can
// act on.
func (e *SiteError) ExplainText() string {
	if e.Code == 0 {
		return ExplainReason(e.Reason)
	}
	return Explain(e.Code)
}

func (e *SiteError) Error() string {
	// The site's own message is in English and says nothing a person
	// can act on; the code is what says what to do. The site's words
	// come along, but they do not replace the explanation.
	msg := e.ExplainText()
	if e.Message != "" {
		msg = fmt.Sprintf("%s（站点说：%s）", msg, e.Message)
	}
	if e.Code == 0 {
		return "the site refused the sign-in: " + msg
	}
	return fmt.Sprintf("the site refused the sign-in (%d): %s", e.Code, msg)
}

// ExplainReason says what the site's own word for a refusal means.
//
// Some refusals come without a number of their own — the site just
// says "login_required" or the like. That word is English and says
// nothing a person can act on, so the explanation comes from the word.
func ExplainReason(reason string) string {
	switch reason {
	case "login_required", "login_required_2":
		return "要先登录才能批准"
	case "invalid_grant":
		return "这个设备码已经不能用了"
	case "access_denied":
		return "站点没有让这个设备码进来"
	}
	return "站点拒绝了，没有说为什么"
}

// Explain says what a site code means in a human's words.
func Explain(code int) string {
	switch code {
	case 11:
		return "这个账号没有权限登录"
	case 1200007:
		return "登录状态无效"
	case 1200044:
		return "这个账号还没绑手机，去网站绑了再来"
	case 1200046:
		return "密码不对"
	case 1200056:
		return "这个账号还没设密码，去网站设了再来"
	case 1200057:
		return "这个账号的密码太旧了，去网站改过再来"
	case 1200062:
		return "子账号第一次登录要改密码"
	case 1000048:
		return "站点拒绝了这个登录"
	default:
		return "站点说了 " + strconv.Itoa(code)
	}
}

// jsonBody builds a body the way the browser's JSON.stringify does:
// compact, and with no HTML escaping.
//
// The bytes are what gets signed, so they have to be the bytes that
// go on the wire. A map's keys are sorted by Go's encoder, which is
// not what JSON.stringify does, but the site does not care about the
// order of a JSON object's keys, and the signature is over exactly
// these bytes — so the sorted order is fine.
func jsonBody(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// newUUID makes a version 4 UUID.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
