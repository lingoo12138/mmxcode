// Package manager owns the accounts' running state: which token each
// one has, which way out its requests go, what its credits are and
// whether it has checked in.
//
// Everything above it — the gateway, the WebUI, the scheduler — goes
// through this package rather than touching the store and the upstream
// client on its own, so that a token is renewed in one place and a
// refresh token is never spent twice.
//
// There are two account ids, and they are not the same thing. The
// store's id is the manager's own key for an account, and the renewer
// is keyed by it. The site's own id is the one an access token carries,
// in oauth.Token.AccountID. No account id goes into a token in this
// package: the store's id there would have the renewer walk a second
// chain for one account and spend two refresh tokens, which signs the
// account out.
package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"mmxcode/internal/account"
	"mmxcode/internal/checkin"
	"mmxcode/internal/egress"
	"mmxcode/internal/oauth"
	"mmxcode/internal/site"
	"mmxcode/internal/store"
	"mmxcode/internal/upstream"
)

// renewWindow is how long before a token ends it is renewed.
//
// The reference plugin renews ten minutes early; the site hands out
// access tokens that live an hour.
const renewWindow = 10 * time.Minute

// accountTimeout is how long the account API is waited for.
//
// The account API answers in a moment: the membership, the ledger and
// the plan's windows are three small answers. A stalled one gets
// little of that, so that one account's reading does not hold up the
// others'.
const accountTimeout = 5 * time.Second

// The reasons an account's last error says.
//
// They say which case it is, because the two are mended in different
// ways: a broken chain needs a human, an unknown outcome does not.
const (
	// chainBrokenReason says the renewal chain cannot go on.
	chainBrokenReason = "the refresh chain is broken: the refresh token was spent already and there is no newer one, so the account has to be signed in again"
	// unknownOutcomeReason says a refresh was sent and its answer was lost.
	unknownOutcomeReason = "the outcome of the last refresh is not known: the site may have spent the refresh token, so it was not sent again"
)

// Manager holds the gateway's accounts.
type Manager struct {
	// Store is the database.
	Store *store.Store
	// Up is the client for MiniMax's hosts.
	Up *upstream.Client
	// Acc reads the account API.
	Acc *account.Client
	// Egress resolves the way out and hands out clients.
	Egress *egress.Resolver
	// Renewer spends refresh tokens, once each.
	Renewer *oauth.Renewer
	// TokenURL is where a refresh token is traded in.
	TokenURL string
}

// New makes a manager.
func New(s *store.Store, up *upstream.Client, acc *account.Client, egr *egress.Resolver, rw *oauth.Renewer) *Manager {
	return &Manager{
		Store: s, Up: up, Acc: acc, Egress: egr, Renewer: rw,
		TokenURL: site.AccountURL(site.TokenPath),
	}
}

// Accounts gives every account.
func (m *Manager) Accounts(ctx context.Context) ([]store.Account, error) {
	return m.Store.ListAccounts(ctx)
}

// Account gives one account.
func (m *Manager) Account(ctx context.Context, id string) (store.Account, error) {
	return m.Store.GetAccount(ctx, id)
}

// Token gives an account's access token, renewing it when it is about
// to end.
//
// The renewal goes through the renewer, so a burst of requests finds
// one renewal in flight rather than several, and the one-time refresh
// token is sent once.
func (m *Manager) Token(ctx context.Context, accountID string) (*oauth.Token, error) {
	stored, err := m.Store.GetToken(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("account %s is not signed in", accountID)
	}
	if stored.Access == "" && stored.Refresh == "" {
		// The sign-in was taken away: the site has refused it. There
		// is nothing to send, so the account fails here instead of
		// paying for a renewal that cannot work.
		return nil, fmt.Errorf("account %s is signed out", accountID)
	}
	cur := tokenFromStore(stored)
	if cur.Expires.After(time.Now().Add(renewWindow)) {
		return cur, nil
	}
	return m.Renew(ctx, accountID, cur)
}

// tokenFromStore gives the token the store holds.
//
// The site's own account id is not in the store, so the field stays
// empty: the store's id is the manager's key for the account, the
// renewer is keyed by it, and a token that carried it would be a token
// whose account id is the wrong one. The site puts its own id in the
// tokens it hands out, and that one is kept.
func tokenFromStore(stored store.Token) *oauth.Token {
	return &oauth.Token{
		Access:  stored.Access,
		Refresh: stored.Refresh,
		Expires: stored.ExpiresAt,
		Subject: stored.Subject,
	}
}

// Renew trades an account's refresh token for a new one.
//
// The failures are told apart, because they are mended in different
// ways:
//
//   - The site refuses the refresh token (invalid_grant). The account
//     is signed out — unless the renewer holds a newer token: a token
//     that is spent already is refused the same way a revoked sign-in
//     is, and a stale token must not sign out an account that is alive.
//   - The chain cannot go on (oauth.ErrRefreshChainBroken). No token
//     is left that the site would take, so the account has to be signed
//     in again by hand.
//   - The answer was lost (oauth.ErrRefreshOutcomeUnknown). The site
//     may have spent the token, so it is not sent again, but the site
//     never said the sign-in is no good: the account is not signed out.
func (m *Manager) Renew(ctx context.Context, accountID string, cur *oauth.Token) (*oauth.Token, error) {
	if cur == nil {
		return nil, fmt.Errorf("no token to renew")
	}
	// What the renewer holds is asked for before the renewal: a
	// renewal that walks the chain drops what the renewer holds, so
	// afterwards there may be nothing left to ask about.
	held, _ := m.Renewer.Newest(accountID)
	// The token that really goes to the site is remembered: the
	// renewer may walk the chain and spend a token other than the
	// caller's, and a refusal is about the token that was sent.
	var sent spentRefresh
	tok, err := m.Renewer.Renew(ctx, accountID, cur, func(ctx context.Context, refresh string) (*oauth.Token, error) {
		sent.set(refresh)
		return oauth.Refresh(ctx, m.Up, m.TokenURL, refresh)
	})
	if err != nil {
		return m.renewFailed(ctx, accountID, cur, sent.get(), held, err)
	}
	if err := m.saveToken(ctx, accountID, tok); err != nil {
		return nil, err
	}
	return tok, nil
}

// renewFailed says what a failed renewal means for the account.
//
// It gives the token to use when there is one — a refused token that
// was only stale is not the end of the account — and the error to give
// back when there is not.
func (m *Manager) renewFailed(ctx context.Context, accountID string, cur *oauth.Token,
	refused string, held *oauth.Token, err error) (*oauth.Token, error) {
	switch {
	case errors.Is(err, oauth.ErrRefreshChainBroken):
		// The chain cannot go on: the token in hand is spent and there
		// is no newer one the site would take. Only a human can mend
		// this, by signing the account in again.
		if mErr := m.markSignedOut(ctx, accountID, chainBrokenReason); mErr != nil {
			return nil, errors.Join(err, fmt.Errorf("the broken chain could not be saved: %w", mErr))
		}
		return nil, err
	case errors.Is(err, oauth.ErrRefreshOutcomeUnknown):
		// The request went out and the answer was lost: the site may
		// have spent the token, so it is not sent again — the renewer
		// knows that — but the site never said the sign-in is no good,
		// so the account is not signed out. The error is written down
		// all the same, so that the WebUI shows that this account is
		// in trouble.
		if mErr := m.markError(ctx, accountID, unknownOutcomeReason); mErr != nil {
			return nil, errors.Join(err, fmt.Errorf("the error could not be saved: %w", mErr))
		}
		return nil, err
	}
	var oe *oauth.Error
	if !asOAuthError(err, &oe) || !oe.IsSignedOut() {
		return nil, err
	}
	// The site refused a refresh token. The account is signed out —
	// unless the refused token was stale and the renewer holds a newer
	// one: a token that is spent already is refused the same way a
	// revoked sign-in is, and an account that is alive must not be
	// signed out over a token nobody uses any more.
	//
	// What the renewer holds is asked for again: a renewal that walks
	// the chain may have left it with nothing, and what it held before
	// is then the only thing there is to say.
	newest, ok := m.Renewer.Newest(accountID)
	if !ok {
		newest = held
	}
	if newer := m.newerToken(cur, refused, newest); newer != nil {
		if sErr := m.saveToken(ctx, accountID, newer); sErr != nil {
			return nil, errors.Join(err, sErr)
		}
		return newer, nil
	}
	// The sign-out has to be written down. An account that stays active
	// with a token the site has refused is handed out again and again,
	// and every request pays for a renewal that cannot work.
	if mErr := m.markSignedOut(ctx, accountID, err.Error()); mErr != nil {
		return nil, errors.Join(err, fmt.Errorf("the sign-out could not be saved: %w", mErr))
	}
	return nil, err
}

// newerToken gives the token to use when the token that was refused was
// only stale.
//
// The site answers invalid_grant for a refresh token that is spent
// already, and that is what a revoked sign-in looks like too. So a
// refusal is only the end of the account when there is nothing newer
// to try: the renewer may hold a token from a sign-in or from another
// process, and then the refused token was merely stale.
//
// The token the renewer holds has to be a different one — the refused
// token is the one it holds too when the chain was walked, and that one
// is dead — and a newer one: the renewer may hold the older of the two,
// and a fresh sign-in in the store is newer than what the renewer has.
// The expiry says which one is newer, the same way it says it for the
// store.
func (m *Manager) newerToken(cur *oauth.Token, refused string, held *oauth.Token) *oauth.Token {
	if held == nil || held.Refresh == "" {
		return nil
	}
	if held.Refresh == refused {
		return nil
	}
	if !held.Expires.IsZero() && !cur.Expires.IsZero() && held.Expires.Before(cur.Expires) {
		return nil
	}
	return held
}

// saveToken writes an account's token to the store.
//
// The token is written only when it is newer than the one the store
// holds. The chain advances on its own, so two renewals can answer
// with two different tokens, and the older one can arrive second: the
// older token's refresh has been spent already — the newer one was
// bought with it — and writing it down would leave the store with a
// refresh token the site refuses, which signs the account out at the
// next renewal.
//
// The expiry says which token is newer: the site hands out access
// tokens that live an hour, so the token of the later renewal ends
// later. Two tokens that end at the same time are the same step of the
// chain, and the one in the store is kept: writing the other one down
// buys nothing and risks a refresh token that is spent already.
func (m *Manager) saveToken(ctx context.Context, accountID string, tok *oauth.Token) error {
	stored, err := m.Store.GetToken(ctx, accountID)
	if err == nil && !stored.ExpiresAt.Before(tok.Expires) {
		// The store already holds a token that is at least as new.
		return nil
	}
	// A token that could not be read is written over: the store is
	// the only place the token lives, and a renewal that is not
	// written down is a renewal that is lost.
	return m.Store.SaveToken(ctx, store.Token{
		AccountID: accountID,
		Access:    tok.Access,
		Refresh:   tok.Refresh,
		ExpiresAt: tok.Expires,
		Subject:   tok.Subject,
		UpdatedAt: time.Now(),
	})
}

// ForceRenew renews an account's token without waiting for it to be
// near its end.
//
// It is what the gateway does when the site answers 401.
//
// The renewer is not told to forget the account, and it is not told to
// reset the chain either. The token it holds may be newer than the one
// in the store -- a renewal that worked but whose writing failed leaves
// the store behind -- and it is the only way out of that: the refresh
// token in the store is spent, and a spent refresh token must not be
// sent again. A reset would drop the token that is the only way out, and
// a forget would drop the record of the tokens already spent, so that
// one of them would be sent again.
func (m *Manager) ForceRenew(ctx context.Context, accountID string) (*oauth.Token, error) {
	stored, err := m.Store.GetToken(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("account %s is not signed in", accountID)
	}
	if stored.Access == "" && stored.Refresh == "" {
		return nil, fmt.Errorf("account %s is signed out", accountID)
	}
	return m.Renew(ctx, accountID, tokenFromStore(stored))
}

// markSignedOut records that an account's sign-in is gone.
//
// The sign-in is taken away as well: an account whose sign-in the site
// has refused must not keep a token that every request would try to
// renew.
//
// The error is not swallowed: a sign-out that could not be written
// down is an account that stays active with a token the site refuses.
func (m *Manager) markSignedOut(ctx context.Context, accountID, reason string) error {
	a, err := m.Store.GetAccount(ctx, accountID)
	if err != nil {
		return err
	}
	a.Status = store.StatusSignedOut
	a.LastError = reason
	a.UpdatedAt = time.Now()
	if err := m.Store.UpdateAccount(ctx, a); err != nil {
		return err
	}
	// The token row is blanked: the sign-in is gone, so the token is
	// of no use, and keeping it would send the account back to the
	// site for a renewal that was already refused.
	if err := m.Store.SaveToken(ctx, store.Token{
		AccountID: accountID,
		UpdatedAt: time.Now(),
	}); err != nil {
		return err
	}
	// The renewer forgets the account too: a signed-out account must
	// not be able to renew, and a token of the old sign-in must not be
	// waiting when the account signs in again. This is the one place
	// where forgetting is right: the account is signed out for good,
	// so nothing of the old chain is of use any more.
	m.Renewer.Forget(accountID)
	return nil
}

// markError writes an account's last error down, without signing it
// out.
//
// An account whose refresh answer was lost is not a signed-out
// account: the site never said the sign-in is no good, and the access
// token may work until it ends. The error is written down all the same
// and the account is put in the error state, so that the WebUI shows
// that this account is in trouble instead of showing it as a normal
// one.
//
// The renewer is not told anything: it has to keep the record of the
// token whose outcome is not known, because that token must not be sent
// again.
func (m *Manager) markError(ctx context.Context, accountID, reason string) error {
	a, err := m.Store.GetAccount(ctx, accountID)
	if err != nil {
		return err
	}
	a.Status = store.StatusError
	a.LastError = reason
	a.UpdatedAt = time.Now()
	return m.Store.UpdateAccount(ctx, a)
}

// spentRefresh remembers the refresh token that really went to the
// site.
//
// The renewer walks the chain, so the token that is spent is not always
// the one the caller asked about. The renewal runs in a goroutine of
// its own and the caller may stop waiting for it, so the remembering
// has to be safe.
type spentRefresh struct {
	mu  sync.Mutex
	tok string
}

// set remembers the refresh token that was sent.
func (s *spentRefresh) set(tok string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tok = tok
}

// get gives the refresh token that was sent.
func (s *spentRefresh) get() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tok
}

// accFor gives the account API client an account's calls go out with.
//
// Every fetch an account makes goes through the account's own way out,
// or the global one. An account behind a proxy that is not used for
// its usage reading is an account whose reading never arrives.
//
// The transport is cached by the resolver, so making a client for it is
// cheap.
func (m *Manager) accFor(a store.Account) (*account.Client, error) {
	tr, err := m.Egress.Transport(m.Egress.Resolve(a))
	if err != nil {
		return nil, err
	}
	return m.Acc.With(upstream.New(upstream.Options{Timeout: accountTimeout, Transport: tr})), nil
}

// Client gives the HTTP client an account's requests go out with, and
// the address of that way out.
//
// The address is redacted: it is shown to the user, and the proxy's
// credentials are a secret. The client itself carries the real
// address, so nothing is lost by hiding it here.
func (m *Manager) Client(ctx context.Context, accountID string) (*http.Client, string, error) {
	a, err := m.Store.GetAccount(ctx, accountID)
	if err != nil {
		return nil, "", err
	}
	url := m.Egress.Resolve(a)
	c, err := m.Egress.Client(url)
	if err != nil {
		return nil, egress.Redacted(url), err
	}
	return c, egress.Redacted(url), nil
}

// RefreshUsage reads an account's credits and windows and saves them.
//
// The membership comes first: it names the group the plan's windows
// are under, and the console asks for the windows of that group.
//
// A refused sign-in is not a missing reading: the account is marked
// signed out, so that a plan that could not be read is not mistaken
// for a plan without windows.
//
// A reading that failed is written down as well: the snapshot of the
// failed reading says so, so the numbers of the reading before it are
// not passed off as current.
func (m *Manager) RefreshUsage(ctx context.Context, accountID string) (store.UsageSnapshot, error) {
	a, err := m.Store.GetAccount(ctx, accountID)
	if err != nil {
		return store.UsageSnapshot{}, err
	}
	tok, err := m.Token(ctx, accountID)
	if err != nil {
		return store.UsageSnapshot{}, err
	}
	snap := store.UsageSnapshot{AccountID: accountID, CreatedAt: time.Now()}

	acc, err := m.accFor(a)
	if err != nil {
		return store.UsageSnapshot{}, err
	}

	// The membership is read before the plan, because it names the
	// group the plan's windows are under.
	group := ""
	if mem, err := acc.Membership(ctx, tok.Access, a.UserID); err == nil {
		snap.Balance = mem.Balance
		snap.PlanUntil = mem.ExpiresAt
		group = mem.GroupID
	} else {
		// The error is kept: a membership that could not be read is
		// not a membership without a group, and the windows are
		// asked for under that group.
		snap.MembershipError = err.Error()
		if isRefusal(err) {
			if err := m.markSignedOut(ctx, accountID, err.Error()); err != nil {
				return store.UsageSnapshot{}, err
			}
		}
	}

	batches, err := acc.Ledger(ctx, tok.Access, a.UserID)
	if err != nil {
		// The reading failed. It is written down, so that a consumer
		// can say "the last reading of this account failed" instead
		// of showing the numbers of the reading before it as if they
		// were current.
		snap.HasAmounts = false
		if sErr := m.Store.SaveUsageSnapshot(ctx, snap); sErr != nil {
			return store.UsageSnapshot{}, errors.Join(err, sErr)
		}
		return snap, err
	}
	if batches == nil {
		// The ledger could not be read. The credits are unknown, and
		// an unknown share is not a share of zero. The reading is
		// written down all the same, so that it is known to have
		// failed.
		snap.HasAmounts = false
		if sErr := m.Store.SaveUsageSnapshot(ctx, snap); sErr != nil {
			return store.UsageSnapshot{}, sErr
		}
		return snap, nil
	}
	now := time.Now()
	var granted, consumed, remaining float64
	live := false
	unknown := false
	expired := false
	var soonest time.Time
	var lastExpiry time.Time
	for _, b := range batches {
		if b.CreditType != account.CreditTypePurchase && b.CreditType != account.CreditTypeCheckin {
			// A bucket the plan does not count.
			continue
		}
		if !b.HasExpiresAt {
			// The credits cannot be read: the batch does not say
			// when it runs out, so a live batch cannot be told
			// from a spent one, and the balance would be wrong.
			unknown = true
			break
		}
		if !b.ExpiresAt.After(now) {
			// The batch has run out. That is not a loss: the
			// account's credits ran out, which is not the same as
			// the credits being unknown.
			expired = true
			if b.ExpiresAt.After(lastExpiry) {
				lastExpiry = b.ExpiresAt
			}
			continue
		}
		granted += b.Granted
		consumed += b.Consumed
		remaining += b.Remaining
		live = true
		if soonest.IsZero() || b.ExpiresAt.Before(soonest) {
			soonest = b.ExpiresAt
		}
	}
	switch {
	case unknown:
		// A part of the ledger would tell a wrong share, so no
		// share is told at all.
		snap.HasAmounts = false
	default:
		// The ledger was read: the account has what it has. A ledger
		// with nothing live in it means the account has no credits
		// left, which is not the same as a ledger that could not be
		// read.
		snap.HasAmounts = true
		snap.Granted, snap.Consumed, snap.Remaining = granted, consumed, remaining
		switch {
		case live:
			snap.ExpiresAt = soonest
		case expired:
			// Every batch had run out: the account ran out at this
			// time, and the summary can say so.
			snap.ExpiresAt = lastExpiry
		}
	}

	if ws, err := acc.PlanWindows(ctx, tok.Access, group); err == nil {
		if len(ws) > 0 {
			if b, err := json.Marshal(ws); err == nil {
				snap.WindowsJSON = string(b)
			}
		}
	} else {
		// The error is kept: a plan whose windows could not be read
		// is not a plan without windows, and the caller decides what
		// to do with the account.
		snap.WindowsError = err.Error()
		if isRefusal(err) {
			if err := m.markSignedOut(ctx, accountID, err.Error()); err != nil {
				return store.UsageSnapshot{}, err
			}
		}
	}
	if title, err := acc.PlanTitle(ctx, tok.Access, group); err == nil {
		snap.Plan = title
	} else if isRefusal(err) {
		if err := m.markSignedOut(ctx, accountID, err.Error()); err != nil {
			return store.UsageSnapshot{}, err
		}
	}

	if err := m.Store.SaveUsageSnapshot(ctx, snap); err != nil {
		return store.UsageSnapshot{}, err
	}
	return snap, nil
}

// Checkin claims an account's daily credits.
func (m *Manager) Checkin(ctx context.Context, accountID string) (checkin.Outcome, error) {
	a, err := m.Store.GetAccount(ctx, accountID)
	if err != nil {
		return checkin.Outcome{}, err
	}
	tok, err := m.Token(ctx, accountID)
	if err != nil {
		return checkin.Outcome{}, err
	}
	acc, err := m.accFor(a)
	if err != nil {
		return checkin.Outcome{}, err
	}
	out, err := checkin.New(acc).Run(ctx, tok.Access, a.UserID)
	if err != nil {
		return checkin.Outcome{}, err
	}
	day := store.DayKey(time.Now())
	_ = m.Store.SaveCheckin(ctx, store.Checkin{
		AccountID: accountID,
		Day:       day,
		Outcome:   out.Outcome,
		Credit:    out.Credit,
		Streak:    out.Streak,
		Message:   out.Message,
		CreatedAt: time.Now(),
	})
	return out, nil
}

// LatestWindows gives an account's saved windows.
//
// It is the windows of the account's newest reading: an earlier
// reading's windows are not looked at. When that reading could not
// read the windows, the error is given: a plan whose windows could
// not be read is not a plan without windows, and the caller decides
// what to do with the account.
func (m *Manager) LatestWindows(ctx context.Context, accountID string) ([]account.Window, error) {
	snap, err := m.Store.LatestUsageSnapshot(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if snap.WindowsError != "" {
		return nil, fmt.Errorf("the plan's windows could not be read: %s", snap.WindowsError)
	}
	if snap.WindowsJSON == "" {
		return nil, nil
	}
	var ws []account.Window
	if err := json.Unmarshal([]byte(snap.WindowsJSON), &ws); err != nil {
		return nil, err
	}
	return ws, nil
}

// asOAuthError unwraps an error into an OAuth error.
func asOAuthError(err error, target **oauth.Error) bool {
	return errors.As(err, target)
}

// isRefusal says an error is a refusal of the sign-in.
//
// A refused sign-in is one event, however it arrives: an HTTP 401 or
// 403, or MiniMax's own code 1000048 in the body of a 200.
func isRefusal(err error) bool {
	if err == nil {
		return false
	}
	var refused *account.RefusedError
	return errors.As(err, &refused) || errors.Is(err, upstream.ErrSignInRefused)
}

// WindowGroup says which plan window a model spends from.
//
// The plan's windows are per model group: the chat models spend the
// "general" group, the video models spend another.
func WindowGroup(model string) string {
	if strings.Contains(strings.ToLower(model), "video") {
		return "video"
	}
	return "general"
}
