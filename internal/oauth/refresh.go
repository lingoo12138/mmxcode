package oauth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"sync"
	"time"

	"github.com/lingoo12138/mmxcode/internal/site"
	"github.com/lingoo12138/mmxcode/internal/upstream"
)

// ErrRefreshOutcomeUnknown says a refresh may have been spent.
//
// The request reached the site, or may have, and the answer was lost on
// the way back. The site may have spent the refresh token already, and
// sending it again would be answered with invalid_grant, which signs
// the account out. So a token whose outcome is unknown is not sent
// again.
var ErrRefreshOutcomeUnknown = errors.New("the outcome of the refresh is not known")

// ErrRefreshChainBroken says the renewal chain cannot go on.
//
// The chain is the run of refresh tokens one account walks: each token
// is spent once and buys the next. The chain breaks when the token at
// hand was spent already and there is no newer one to walk from, and
// when the account was signed out while a renewal was running. No
// token is left that the site would take, so the account has to be
// signed in again by hand.
var ErrRefreshChainBroken = errors.New("the refresh chain is broken")

// spendTimeout is how long a renewal may take.
//
// The renewal runs under a context of its own, so that the cancellation
// of one caller does not kill the renewal the other callers wait for.
// The deadline is what stops it from running forever.
const spendTimeout = 30 * time.Second

// Refresh trades a refresh token for a new access token.
//
// The caller's refresh token is kept when the answer gives no new one.
//
// The failure says which kind it is. A failure before the request left
// — a name that does not resolve, a connection that is refused — is
// safe to try again with the same token. A failure after the request
// was written — a read that times out, a connection that is cut —
// leaves the answer unknown: the site may have spent the token
// already. That error is ErrRefreshOutcomeUnknown, and the token is
// not sent again.
func Refresh(ctx context.Context, c *upstream.Client, endpoint, refresh string) (*Token, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refresh)
	form.Set("client_id", site.ClientID)
	form.Set("scope", site.Scope)
	form.Set("audience", site.Audience)

	resp, err := c.PostForm(ctx, endpoint, form)
	if err != nil {
		if requestNeverLeft(err) {
			return nil, fmt.Errorf("refresh: %w", err)
		}
		return nil, &UnknownOutcomeError{Err: err}
	}
	body, err := upstream.ReadBody(resp)
	if err != nil {
		// The site answered and the answer could not be read. The
		// token may have been spent all the same.
		return nil, &UnknownOutcomeError{Err: err}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, oauthError(resp.StatusCode, body, WhatRefresh)
	}
	var b struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Error        string `json:"error"`
	}
	if err := json.Unmarshal(body, &b); err != nil {
		return nil, &Error{Code: "invalid_json_response", HTTPStatus: resp.StatusCode, What: WhatRefresh}
	}
	if b.Error != "" {
		return nil, oauthError(resp.StatusCode, body, WhatRefresh)
	}
	if b.AccessToken == "" || b.ExpiresIn <= 0 {
		return nil, &Error{Code: "invalid_token_response", HTTPStatus: resp.StatusCode, What: WhatRefresh}
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
	tok.Subject, tok.AccountID, tok.HasClaims = claims(b.AccessToken)
	return tok, nil
}

// UnknownOutcomeError is a refresh whose answer was lost.
//
// The request reached the site, or may have, and the answer was lost on
// the way back. The site may have spent the refresh token, so the
// token must not be sent again. errors.Is says so: the error is
// ErrRefreshOutcomeUnknown.
type UnknownOutcomeError struct {
	// Err is what the transport said.
	Err error
}

// Error is the error's text.
func (e *UnknownOutcomeError) Error() string {
	return fmt.Sprintf("the refresh answer was lost: %v", e.Err)
}

// Unwrap gives the transport's error and ErrRefreshOutcomeUnknown.
func (e *UnknownOutcomeError) Unwrap() []error {
	return []error{e.Err, ErrRefreshOutcomeUnknown}
}

// requestNeverLeft says the request provably did not reach the site.
//
// A name that does not resolve and a connection that is refused are
// the two failures that happen before anything is written: the site
// cannot have spent a token it never heard of. Everything else — a
// read that times out, a connection that is cut halfway — leaves the
// answer unknown.
func requestNeverLeft(err error) bool {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return true
	}
	return false
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
// The renewer keeps, per account, the newest token it got, the refresh
// tokens already spent, and the refresh tokens whose outcome is not
// known. A request that comes while a renewal is running waits for it
// and uses what it gave.
type Renewer struct {
	mu       sync.Mutex
	inflight map[string]*renewCall
	latest   map[string]*Token
	// spent holds, per account, the hashes of the refresh tokens
	// already spent. A token in here is never sent again.
	//
	// The tokens are remembered by their hash and not by themselves: a
	// refresh token is a secret, and a hash keeps no secret in memory
	// longer than it has to be there. The set grows with every
	// renewal, which is what makes it right: a set that kept only the
	// last few would let an old token be sent again.
	spent map[string]map[string]bool
	// unknown holds, per account, the hashes of the refresh tokens
	// whose outcome is not known. The site may have spent them, so
	// they are not sent again either.
	unknown map[string]map[string]bool
	// gen is the generation of each account's chain. A renewal takes
	// the generation it started in and writes nothing when it has
	// moved on, so a renewal that was running when the account was
	// signed out does not bring it back.
	gen map[string]uint64
}

// renewCall is one renewal in flight.
type renewCall struct {
	done chan struct{}
	// token and err are what the renewal gave.
	token *Token
	err   error
	// accountID is the account the renewal is for.
	accountID string
	// refresh is the refresh token the renewal spends.
	refresh string
	// gen is the generation the renewal started in.
	gen uint64
}

// NewRenewer makes a renewer.
func NewRenewer() *Renewer {
	return &Renewer{
		inflight: map[string]*renewCall{},
		latest:   map[string]*Token{},
		spent:    map[string]map[string]bool{},
		unknown:  map[string]map[string]bool{},
		gen:      map[string]uint64{},
	}
}

// Renew gives an account a fresh token, spending the refresh token at
// hand once.
//
// When the renewal is already running for the account and the token,
// the caller waits for it and takes what it gave. When the refresh
// token was already spent, the chain goes on from the newest token this
// renewer holds: that is the only way past a store that did not get
// the last token, and without it the chain never advances on its own.
//
// When the renewer does not know the token the caller holds, the
// caller has a newer one — a fresh sign-in, or a token another process
// wrote — so the caller is the one who is right and what this renewer
// holds is dropped.
//
// When the chain cannot go on at all, the error is
// ErrRefreshChainBroken, and the caller has to sign the account in
// again by hand.
func (r *Renewer) Renew(ctx context.Context, accountID string, current *Token, spend SpendFunc) (*Token, error) {
	if current == nil {
		return nil, fmt.Errorf("no token to renew")
	}
	if current.Refresh == "" {
		return nil, fmt.Errorf("no refresh token to renew")
	}
	if spend == nil {
		return nil, fmt.Errorf("no way to spend the refresh token")
	}

	key := tokenHash(current.Refresh)

	r.mu.Lock()
	// A renewal with this token is already running for this account:
	// wait for it rather than sending the token twice.
	if c, ok := r.inflight[flightKey(accountID, key)]; ok {
		r.mu.Unlock()
		return waitFor(ctx, c)
	}
	// The outcome of the last renewal with this token is not known:
	// the site may have spent it. It is not sent again.
	if r.unknown[accountID][key] {
		r.mu.Unlock()
		return nil, fmt.Errorf("the outcome of the last refresh with this token is not known, so it is not sent again: %w", ErrRefreshOutcomeUnknown)
	}
	head := r.latest[accountID]
	if head != nil && head.Subject != "" && current.Subject != "" && head.Subject != current.Subject {
		// The token is of another account: renewing it here would put
		// its token under this account.
		r.mu.Unlock()
		return nil, fmt.Errorf("the token belongs to another account")
	}
	switch {
	case r.spent[accountID][key]:
		// The caller's token was spent already. The chain goes on from
		// the newest token this renewer holds.
		if head == nil || head.Refresh == current.Refresh {
			r.mu.Unlock()
			return nil, fmt.Errorf("the refresh token was already spent and no newer token is held: %w", ErrRefreshChainBroken)
		}
		headKey := tokenHash(head.Refresh)
		if r.spent[accountID][headKey] || r.unknown[accountID][headKey] {
			r.mu.Unlock()
			return nil, fmt.Errorf("the refresh token was already spent and no newer token is held: %w", ErrRefreshChainBroken)
		}
		current, key = head, headKey
	case head == nil || head.Refresh != current.Refresh:
		// The renewer does not know the caller's token, so the caller
		// has a newer one. The caller is the one who is right, so what
		// this renewer holds is dropped.
		delete(r.latest, accountID)
	}

	// The flight takes the account's chain over: any renewal running
	// for this account is left behind, and its answer does not become
	// the chain's.
	r.gen[accountID]++
	c := &renewCall{
		done:      make(chan struct{}),
		accountID: accountID,
		refresh:   current.Refresh,
		gen:       r.gen[accountID],
	}
	r.inflight[flightKey(accountID, key)] = c
	r.mu.Unlock()

	go r.run(ctx, c, spend)
	return waitFor(ctx, c)
}

// run spends a refresh token and puts what it bought into the renewer.
//
// The spend runs under a context of its own: the caller's cancellation
// must not kill the renewal that every other caller is waiting for.
// The caller's context is only for waiting.
func (r *Renewer) run(ctx context.Context, c *renewCall, spend SpendFunc) {
	spendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), spendTimeout)
	defer cancel()

	tok, err := spendSafely(spend, spendCtx, c.refresh)

	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.inflight, flightKey(c.accountID, tokenHash(c.refresh)))
	if err == nil && tok != nil {
		// The site took the token, whatever happens to the chain now,
		// so it is never sent again. A site that does not rotate the
		// refresh token is the exception: it hands the same token
		// back, so it did not take it.
		if tok.Refresh != c.refresh {
			r.markSpent(c.accountID, c.refresh)
		}
		if r.gen[c.accountID] == c.gen {
			// A copy: the caller may change what it holds, and what
			// the renewer holds is not the caller's to change.
			held := *tok
			r.latest[c.accountID] = &held
		}
	}
	if err != nil && errors.Is(err, ErrRefreshOutcomeUnknown) {
		// The site may have taken the token. It is not sent again.
		r.markUnknown(c.accountID, c.refresh)
	}
	c.token, c.err = tok, err
	close(c.done)
}

// spendSafely spends a refresh token, and a panic is an error like any
// other.
//
// The spend runs in a goroutine of its own, and a panic there would
// take the whole process down with it.
func spendSafely(spend SpendFunc, ctx context.Context, refresh string) (tok *Token, err error) {
	defer func() {
		if p := recover(); p != nil {
			tok, err = nil, fmt.Errorf("refresh: %v", p)
		}
	}()
	return spend(ctx, refresh)
}

// waitFor waits for a renewal to finish.
//
// The caller's context is only for waiting: the renewal runs under a
// context of its own, so a caller that goes away does not kill the
// renewal the other callers are waiting for.
func waitFor(ctx context.Context, c *renewCall) (*Token, error) {
	select {
	case <-c.done:
		return c.token, c.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Latest gives the newest token this renewer holds for an account.
//
// The renewer keeps only the newest one per account, so this is that
// one, or the caller's own when there is none.
//
// Newest says the same thing without the guessing: it says whether
// there is one.
func (r *Renewer) Latest(accountID string, current *Token) *Token {
	tok, ok := r.Newest(accountID)
	if ok {
		return tok
	}
	return current
}

// Newest gives the newest token this renewer holds for an account.
//
// The second answer says whether there is one: without it a caller
// cannot tell "the renewer holds nothing" from "the renewer holds the
// token you already have". The token is a copy, so the caller cannot
// change what the renewer holds.
func (r *Renewer) Newest(accountID string) (*Token, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	tok := r.latest[accountID]
	if tok == nil {
		return nil, false
	}
	c := *tok
	return &c, true
}

// Forget drops an account's tokens, for when it is signed out.
//
// Everything about the account goes: the newest token, the record of
// the tokens already spent, and the renewal that may be running. A
// renewal that is running when the account is signed out is abandoned:
// its answer is not written back, so a token does not come back from
// the dead.
//
// A renewal that gets a token while the account is being signed out
// still has that token spent at the site, so the record of it stays:
// a token the site took is never sent again.
func (r *Renewer) Forget(accountID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.latest, accountID)
	delete(r.spent, accountID)
	delete(r.unknown, accountID)
	r.gen[accountID]++
}

// Reset forgets an account's chain, so that the next renewal walks it
// again from the token the caller holds.
//
// The tokens already spent stay spent: a token the site took must never
// be sent again, whatever happens to the chain around it. So Reset is
// for "the chain I was walking is wrong, start again from here", and
// not for "this account is signed out" — that is Forget.
//
// A renewal that is running is abandoned, like in Forget: its answer
// belongs to the chain that was walked, not to the new one.
func (r *Renewer) Reset(accountID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.latest, accountID)
	r.gen[accountID]++
}

// markSpent remembers that a refresh token was spent.
func (r *Renewer) markSpent(accountID, refresh string) {
	set := r.spent[accountID]
	if set == nil {
		set = map[string]bool{}
		r.spent[accountID] = set
	}
	set[tokenHash(refresh)] = true
}

// markUnknown remembers that the outcome of a refresh is not known.
func (r *Renewer) markUnknown(accountID, refresh string) {
	set := r.unknown[accountID]
	if set == nil {
		set = map[string]bool{}
		r.unknown[accountID] = set
	}
	set[tokenHash(refresh)] = true
}

// tokenHash is what a refresh token is remembered by.
//
// The tokens are remembered by their hash and not by themselves: a
// refresh token is a secret, and a hash keeps no secret in memory
// longer than it has to be there. The hash is of the token alone, so a
// hash of one account's token says nothing about another's.
func tokenHash(refresh string) string {
	sum := sha256.Sum256([]byte(refresh))
	return hex.EncodeToString(sum[:])
}

// flightKey says which renewal a request belongs to.
//
// The key is the account and the token, not the account alone: a
// caller that holds another token has another renewal to make, and
// giving it the answer of somebody else's renewal loses its token.
func flightKey(accountID, key string) string {
	return accountID + "\x00" + key
}
