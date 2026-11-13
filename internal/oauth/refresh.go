package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sync"
	"time"

	"mmxcode/internal/site"
	"mmxcode/internal/upstream"
)

// Refresh trades a refresh token for a new access token.
//
// The caller's refresh token is kept when the answer gives no new one.
func Refresh(ctx context.Context, c *upstream.Client, endpoint, refresh string) (*Token, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refresh)
	form.Set("client_id", site.ClientID)
	form.Set("scope", site.Scope)
	form.Set("audience", site.Audience)

	resp, err := c.PostForm(ctx, endpoint, form)
	if err != nil {
		return nil, fmt.Errorf("refresh: %w", err)
	}
	body, err := upstream.ReadBody(resp)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, oauthError(resp.StatusCode, body)
	}
	var b struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Error        string `json:"error"`
	}
	if err := json.Unmarshal(body, &b); err != nil {
		return nil, &Error{Code: "invalid_json_response", HTTPStatus: resp.StatusCode}
	}
	if b.Error != "" {
		return nil, oauthError(resp.StatusCode, body)
	}
	if b.AccessToken == "" || b.ExpiresIn <= 0 {
		return nil, &Error{Code: "invalid_token_response", HTTPStatus: resp.StatusCode}
	}
	newRefresh := b.RefreshToken
	if newRefresh == "" {
		newRefresh = refresh
	}
	tok := &Token{
		Access:  b.AccessToken,
		Refresh: newRefresh,
		Expires: time.Now().Add(expiresDuration(b.ExpiresIn)),
	}
	tok.Subject, tok.AccountID = claims(b.AccessToken)
	return tok, nil
}

// SpendFunc spends a refresh token and gives what it bought.
type SpendFunc func(ctx context.Context, refresh string) (*Token, error)

// Renewer spends refresh tokens, once each.
//
// MiniMax spends a refresh token once: sent a second time it is
// invalid_grant, which signs the account out. So a refresh token that
// was already spent is never sent again from here, whatever the
// concurrency. A refresh that failed for a network reason is the one
// exception: it may not have reached the site, so it is sent again.
//
// The renewer keeps, per account, the newest token it got and the
// refresh tokens already spent. A request that comes while a renewal
// is running waits for it and uses what it gave.
type Renewer struct {
	mu       sync.Mutex
	inflight map[string]*renewCall
	latest   map[string]*Token
	// spent holds, per account, the refresh tokens already spent.
	// Only the last few are kept: a caller can only be holding one
	// that was recent, and a list that grew with every refresh would
	// grow for as long as the process runs.
	spent map[string][]string
}

type renewCall struct {
	done  chan struct{}
	token *Token
	err   error
}

// NewRenewer makes a renewer.
func NewRenewer() *Renewer {
	return &Renewer{
		inflight: map[string]*renewCall{},
		latest:   map[string]*Token{},
		spent:    map[string][]string{},
	}
}

// Renew gives an account a fresh token, spending the refresh token at
// hand once.
//
// When the renewal is already running for the account, the caller waits
// for it and takes what it gave. When the refresh token was already
// spent, the newest token this renewer has is given instead, and the
// spent token is not sent again.
func (r *Renewer) Renew(ctx context.Context, accountID string, current *Token, spend SpendFunc) (*Token, error) {
	if current == nil {
		return nil, fmt.Errorf("no token to renew")
	}
	r.mu.Lock()
	if c, ok := r.inflight[accountID]; ok {
		r.mu.Unlock()
		select {
		case <-c.done:
			return c.token, c.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if isSpent(r.spent[accountID], current.Refresh) {
		newest := r.latest[accountID]
		r.mu.Unlock()
		if newest != nil {
			return newest, nil
		}
		return nil, fmt.Errorf("the refresh token was already spent and no newer token is held")
	}
	if l, ok := r.latest[accountID]; ok && l != nil && l.Refresh != current.Refresh {
		// 已经刷新过了，用新的那个
		r.mu.Unlock()
		return l, nil
	}
	c := &renewCall{done: make(chan struct{})}
	r.inflight[accountID] = c
	r.mu.Unlock()

	tok, err := spend(ctx, current.Refresh)

	r.mu.Lock()
	delete(r.inflight, accountID)
	if err == nil && tok != nil {
		r.latest[accountID] = tok
		r.spent[accountID] = append(r.spent[accountID], current.Refresh)
		if len(r.spent[accountID]) > spentCount {
			r.spent[accountID] = r.spent[accountID][len(r.spent[accountID])-spentCount:]
		}
	}
	c.token, c.err = tok, err
	close(c.done)
	r.mu.Unlock()
	return tok, err
}

// Latest gives the newest token this renewer holds for an account.
//
// The renewer keeps only the newest one per account, so this is that
// one, or the caller's own when there is none.
func (r *Renewer) Latest(accountID string, current *Token) *Token {
	r.mu.Lock()
	defer r.mu.Unlock()
	if l, ok := r.latest[accountID]; ok && l != nil {
		return l
	}
	return current
}

// spentCount is how many spent refresh tokens are kept per account.
const spentCount = 4

// isSpent says whether a refresh token is one of the spent ones.
func isSpent(list []string, refresh string) bool {
	for _, s := range list {
		if s == refresh {
			return true
		}
	}
	return false
}

// Forget drops an account's tokens, for when it is signed out.
func (r *Renewer) Forget(accountID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.latest, accountID)
}
