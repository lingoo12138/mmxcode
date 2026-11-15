// Package manager owns the accounts' running state: which token each
// one has, which way out its requests go, what its credits are and
// whether it has checked in.
//
// The layers above it — the gateway, the WebUI, the scheduler — come
// here for an account's token and for its readings, but they do reach
// the store on their own, so this package is not the only way into the
// database. What it does hold is that a token is renewed in one place
// and a refresh token is never spent twice.
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
	"mmxcode/internal/secret"
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

// defaultHotWindow is how long an account counts as hot when nobody says.
//
// 五个分钟：刚用过一次的账号，它的额度最可能刚变。
const defaultHotWindow = 5 * time.Minute

// The reasons an account's last error says.
//
// They say which case it is, because the two are mended in different
// ways: a broken chain needs a human, an unknown outcome does not.
const (
	// chainBrokenReason says the renewal chain cannot go on.
	chainBrokenReason = "the refresh chain is broken: the refresh token was spent already and there is no newer one, so the account has to be signed in again"
	// unknownOutcomeReason says a refresh was sent and its answer was lost.
	unknownOutcomeReason = "the outcome of the last refresh is not known: the site may have spent the refresh token, so it was not sent again"
	// tokenNotWrittenReason says a renewal worked and its token could
	// not be written down.
	tokenNotWrittenReason = "the token was renewed but it could not be written down, so the token in the database is older than the token in use"
	// checkinNotWrittenReason says a check-in worked and its record
	// could not be written down.
	checkinNotWrittenReason = "the check-in was claimed but its record could not be written down, so the credit and the streak are recorded nowhere"
	// renewalFailedReason says a renewal failed but the token in hand
	// is still good.
	renewalFailedReason = "the renewal failed but the token in hand is still good"
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
	// Key opens the passwords the accounts were brought in with.
	//
	// It is empty when there is no key: then an account cannot be
	// signed in again, and saying so is better than pretending.
	Key secret.Key
	// ReloginFn signs an account in again. It is a field so that a
	// test can stand in for the network.
	ReloginFn ReloginFunc
	// reloginLocks serialises the relogin of one account.
	reloginLocks reloginLocks
	// hot counts the accounts that were used lately.
	hot *hotCounter
	// HotWindow is how long an account counts as hot after it was used.
	//
	// 它是配置说了算，所以它是公开的：网关起的时候会把它设成配置里的
	// 那个。没有人设它的时候是五分钟。
	//
	// 它是起来的时候写一次、之后只读的东西，所以没有锁：写它要在服务
	// 起来之前。
	HotWindow time.Duration
}

// reloginLocks serialises the relogin of one account.
//
// 调度器的一轮和人在命令行里的那一次可能撞上。同一个账号被登两次，
// 站点那边就是两个设备码、两个会话、两条新链；后失败的那一次还会把
// 先成功那一次存好的令牌抹掉。
type reloginLocks struct {
	mu sync.Mutex
	m  map[string]*sync.Mutex
}

// lock takes the lock of one account and gives back the way to let it
// go.
func (l *reloginLocks) lock(id string) func() {
	l.mu.Lock()
	if l.m == nil {
		l.m = map[string]*sync.Mutex{}
	}
	mu, ok := l.m[id]
	if !ok {
		mu = &sync.Mutex{}
		l.m[id] = mu
	}
	l.mu.Unlock()
	mu.Lock()
	return mu.Unlock
}

// New makes a manager.
func New(s *store.Store, up *upstream.Client, acc *account.Client, egr *egress.Resolver, rw *oauth.Renewer) *Manager {
	return &Manager{
		Store: s, Up: up, Acc: acc, Egress: egr, Renewer: rw,
		TokenURL:  site.AccountURL(site.TokenPath),
		hot:       newHotCounter(),
		HotWindow: defaultHotWindow,
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
//
// The token the renewer holds is asked for before the renewal: a
// renewal that worked but whose writing failed leaves the renewer with
// a newer token than the store has, and renewing again would spend
// another refresh token — one the site may have taken already.
func (m *Manager) Token(ctx context.Context, accountID string) (*oauth.Token, error) {
	stored, err := m.Store.GetToken(ctx, accountID)
	if err != nil {
		return nil, tokenReadError(accountID, err)
	}
	if stored.Access == "" && stored.Refresh == "" {
		// The sign-in was taken away: the site has refused it. There
		// is nothing to send, so the account fails here instead of
		// paying for a renewal that cannot work.
		return nil, fmt.Errorf("account %s is signed out", accountID)
	}
	cur := tokenFromStore(stored)
	if held, ok := m.Renewer.Newest(accountID); ok && newerThanStored(held, stored) {
		// The renewer holds a token that is newer than the one in the
		// store, so it is the one to use: renewing again would spend
		// a refresh token that the site may have taken already.
		cur = held
		// It is written down as well, so that the next reader of the
		// store finds the same token.
		if err := m.saveToken(ctx, accountID, cur); err != nil {
			m.note(ctx, accountID, tokenNotWrittenReason+": "+err.Error())
		}
	}
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

// newerThanStored says the token is newer than the one the store has.
//
// The expiry says which one is newer, the same way saveToken says it:
// the site hands out access tokens that live an hour, so the token of
// the later renewal ends later. Two tokens that end at the same time
// are the same step of the chain, and the one in the store is kept.
func newerThanStored(tok *oauth.Token, stored store.Token) bool {
	if tok == nil || tok.Expires.IsZero() {
		return false
	}
	return stored.ExpiresAt.Before(tok.Expires)
}

// tokenReadError says what it is that the token could not be read.
//
// A token that is not there is an account that is not signed in, and
// that is one thing. Anything else is the database in trouble, and
// saying "not signed in" about it hides the cause: the account looks
// signed out in the WebUI while the truth is that the file could not
// be read at all.
func tokenReadError(accountID string, err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("account %s is not signed in", accountID)
	}
	return fmt.Errorf("account %s: the token could not be read: %w", accountID, err)
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
//   - Anything else — the site is having a bad moment, or the network
//     is. The token in hand may still do, so it is kept when it has
//     not ended.
func (m *Manager) Renew(ctx context.Context, accountID string, cur *oauth.Token) (*oauth.Token, error) {
	return m.renew(ctx, accountID, cur, false)
}

// renew trades a refresh token for a new one.
//
// force says whether the renewal was forced. A forced renewal is what
// the gateway does when the site has refused the access token, and then
// the token in hand is no good whatever its expiry says.
func (m *Manager) renew(ctx context.Context, accountID string, cur *oauth.Token, force bool) (*oauth.Token, error) {
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
		return m.renewFailed(ctx, accountID, cur, sent.get(), held, err, force)
	}
	if err := m.saveToken(ctx, accountID, tok); err != nil {
		// The site has just handed out a token that is good for an
		// hour. Throwing it away because the database would not take
		// it burns the quota of another account for nothing: the
		// caller fails the request over, and the refresh token in the
		// store is spent already, so every request after this one
		// renews again and spends another one.
		//
		// So the token is kept and the caller gets it, and no error
		// comes with it. An error would send the request over to
		// another account, and the gateway cannot make use of a token
		// that arrives with one: it fails the request over and drops
		// the token.
		//
		// The failed write is written down on the account instead, so
		// that the WebUI shows why the token in the database is older
		// than the token in use.
		m.note(ctx, accountID, tokenNotWrittenReason+": "+err.Error())
	}
	return tok, nil
}

// renewFailed says what a failed renewal means for the account.
//
// It gives the token to use when there is one — a refused token that
// was only stale is not the end of the account — and the error to give
// back when there is not.
func (m *Manager) renewFailed(ctx context.Context, accountID string, cur *oauth.Token,
	refused string, held *oauth.Token, err error, force bool) (*oauth.Token, error) {
	switch {
	case errors.Is(err, oauth.ErrRefreshChainBroken):
		// The chain cannot go on: the token in hand is spent and there
		// is no newer one the site would take. Only a human can mend
		// this, by signing the account in again.
		if mErr := m.markSignedOut(ctx, accountID, cur.Access, chainBrokenReason); mErr != nil {
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
		//
		// The cause goes with it: "the outcome is not known" says
		// which case it is, but the transport's own words say what
		// happened, and the sign-out branch keeps those too.
		if mErr := m.markError(ctx, accountID, cur.Access, unknownOutcomeReason+": "+err.Error()); mErr != nil {
			return nil, errors.Join(err, fmt.Errorf("the error could not be saved: %w", mErr))
		}
		return nil, err
	}
	var oe *oauth.Error
	if !asOAuthError(err, &oe) || !oe.IsSignedOut() {
		// Not a refusal: the site is having a bad moment, or the
		// network is. The token in hand may still do — the reference
		// keeps it — so it is kept when it has not ended yet.
		//
		// A forced renewal does not keep it: that is what the gateway
		// does when the site has refused the access token, and then
		// the token in hand is no good whatever its expiry says.
		if !force && cur != nil && cur.Expires.After(time.Now()) {
			m.note(ctx, accountID, renewalFailedReason+": "+err.Error())
			return cur, nil
		}
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
	if mErr := m.markSignedOut(ctx, accountID, cur.Access, err.Error()); mErr != nil {
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
//
// A forced renewal does not keep the token in hand when it fails: the
// site has refused the token that is in hand, so it is no good
// whatever its expiry says.
func (m *Manager) ForceRenew(ctx context.Context, accountID string) (*oauth.Token, error) {
	stored, err := m.Store.GetToken(ctx, accountID)
	if err != nil {
		return nil, tokenReadError(accountID, err)
	}
	if stored.Access == "" && stored.Refresh == "" {
		return nil, fmt.Errorf("account %s is signed out", accountID)
	}
	return m.renew(ctx, accountID, tokenFromStore(stored), true)
}

// markSignedOut records that an account's sign-in is gone.
//
// The sign-in is taken away as well: an account whose sign-in the site
// has refused must not keep a token that every request would try to
// renew.
//
// The write is **conditional**: it happens only while the token row
// still holds the access token that was refused. Another process may
// have signed the account in again in the meantime, and writing the
// sign-out then would undo that sign-in — the fresh token would be
// taken away, and the account would be shown as signed out while it
// holds a token that works.
//
// access is the access token of the chain in hand: the one the refusal
// is about. The state is written with the narrow setter, and not with
// the one that writes every field: the name, the proxy and the rest of
// the account are not this method's business, and writing them back from a
// copy that was read a moment ago would undo a change another writer
// made in between.
//
// The error is not swallowed: a sign-out that could not be written
// down is an account that stays active with a token the site refuses.
func (m *Manager) markSignedOut(ctx context.Context, accountID, access, reason string) error {
	written, err := m.Store.SignOutIfTheTokenIsStillThere(ctx, accountID, access, reason, time.Now())
	if err != nil {
		return err
	}
	if !written {
		// 有人在这中间把账号救回来了：库里那份令牌已经不是手里这一份
		// 了，这次失败说的已经不是它了。这一边不动它。
		return nil
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
// The write is conditional, for the same reason as in markSignedOut: the
// error is about the chain in hand, and a chain that has moved on since
// is somebody else's business.
//
// The renewer is not told anything: it has to keep the record of the
// token whose outcome is not known, because that token must not be sent
// again.
func (m *Manager) markError(ctx context.Context, accountID, access, reason string) error {
	_, err := m.Store.MarkErrorIfTheTokenIsStillThere(ctx, accountID, access, reason, time.Now())
	return err
}

// note writes a note on the account without changing its state.
//
// The note is about something that went wrong that the account is not
// broken by: a renewal that could not be written down, a check-in
// whose record was lost. The account itself is still good, so the
// state it has is kept and only the note is written.
//
// The note is written with a setter that writes the note alone: the
// status column is not touched, and the account is not read back to be
// written again. A sign-out that happens while the note is being
// written keeps its status — the two writers share the note column,
// and that is all they share.
//
// The note is best effort. A note that could not be written down must
// not make a working token unusable, and the fault that stopped the
// note is the very fault that stopped the thing the note is about.
func (m *Manager) note(ctx context.Context, accountID, reason string) {
	a, err := m.Store.GetAccount(ctx, accountID)
	if err != nil {
		return
	}
	if a.Status == store.StatusSignedOut {
		// The sign-out has its own reason, and that is the one that
		// matters: it says why the site does not take this account
		// any more.
		return
	}
	// The error is written on its own: the status is not written, and
	// not read back to be written again. A sign-out that happened
	// while this was being written must not be undone by it.
	_ = m.Store.SetAccountLastError(ctx, accountID, reason, time.Now())
}

// ErrNotSignedOut says the account is not signed out.
//
// 重登是给签出的账号准备的。状态不是签出的账号，替它登录没有意义：
// 它手里那条链还能用，重登一次只会把还能用的令牌换掉。
var ErrNotSignedOut = errors.New("这个账号不是签出状态，不用重登")

// ReloginFunc signs an account in again with the password it was
// brought in with.
type ReloginFunc func(ctx context.Context, acc store.Account, password string) error

// Relogin signs an account in again with the password it was brought
// in with.
//
// An account without a password cannot be signed in again: there is
// nothing to sign in with. That is an error, not a silent skip — the
// person who asked wants to know.
//
// Only a signed-out account is signed in again. An account that is
// still active has a token that works, and signing it in again would
// replace that token with a new one for no reason.
func (m *Manager) Relogin(ctx context.Context, accountID string) error {
	acc, err := m.Store.GetAccount(ctx, accountID)
	if err != nil {
		return err
	}
	if acc.Status != store.StatusSignedOut {
		return ErrNotSignedOut
	}
	// 同一个账号的重登要一个一个来：调度器的一轮和命令行里的那一次
	// 可能撞上，两个都登的话，站点那边就是两个设备码、两个会话。
	unlock := m.reloginLocks.lock(accountID)
	defer unlock()
	// 等锁的时候别人可能已经把这个账号登回去了。再登一次是白登，
	// 而且会把刚换好的令牌再换一次。
	acc, err = m.Store.GetAccount(ctx, accountID)
	if err != nil {
		return err
	}
	if acc.Status != store.StatusSignedOut {
		return ErrNotSignedOut
	}
	cipher, err := m.Store.Password(ctx, accountID)
	if err != nil {
		return err
	}
	if cipher == "" {
		return errors.New("这个账号没有存密码，没法自己登回去")
	}
	if len(m.Key) == 0 {
		// 没有密钥，密文就开不了。密文在，说明当初是存过的，是密钥
		// 现在没了——说清楚，别让人以为是账号的问题。
		return errors.New("没有密码的密钥，存着的密文开不了")
	}
	password, err := m.Key.Open(cipher)
	if err != nil {
		return err
	}
	if m.ReloginFn == nil {
		return errors.New("没有地方能重新登录")
	}
	if err := m.ReloginFn(ctx, acc, password); err != nil {
		// 登不回去说的是这次登录失败，不是手里那条链的事。所以令牌
		// 不动：清空它只能毁掉一个还能用的凭据，什么也救不回来。
		// 这里写下的只是失败的原因，账号还是签出的。
		//
		// 写是**有条件**的：另一个进程可能刚把这个账号登回去了，那一
		// 行现在是 active 了，这时候把它写回 signed_out，就是把别人刚
		// 换好的令牌作废掉，还让这个账号显示成登出的状态——手里却握着
		// 一份能用的凭据，轮转池不用它，下一轮又要登一次。
		written, mErr := m.Store.MarkSignedOut(ctx, accountID, err.Error(), time.Now())
		if mErr != nil {
			return errors.Join(err, fmt.Errorf("the failure could not be saved: %w", mErr))
		}
		if !written {
			// 别人已经把它登回去了：原因照样要说给听着的人，但状态
			// 是别人的，不能由这一边改回去。
			return errors.Join(err, ErrNotSignedOut)
		}
		return err
	}
	// 旧的那条链就此作废：这次登录换来的令牌是新的链，旧的刷新令牌
	// 再也不会被发出去了。
	m.Renewer.Forget(accountID)
	return m.Store.UpdateAccountStatus(ctx, accountID, store.StatusActive, "", time.Now())
}

// ReloginResult says what one round of re-logins came to.
type ReloginResult struct {
	// Fixed is how many accounts were signed in again.
	Fixed int
	// Failed is how many could not be signed in again.
	Failed int
	// Reasons says why the ones that failed failed: one line per account.
	Reasons []string
}

// ReloginAll signs in again every account that is signed out and has
// a password.
//
// It gives back how many it fixed, how many it did not, and why.
//
// 理由是要的：只说「失败 2」，人得自己去一个一个看是哪个账号、为什么。
// 而「一个都没试成」（连账号都没列出来）和「都试了，都登回去了」在只
// 看着两个数字的时候是一样的——上一次那个错就是这样被藏起来的。
//
// An account without a password is skipped and is not a failure: it
// was never brought in with one, so there is nothing to sign in with,
// and that is not this round's fault.
func (m *Manager) ReloginAll(ctx context.Context, limit int) (ReloginResult, error) {
	var out ReloginResult
	accounts, err := m.Store.ListAccounts(ctx)
	if err != nil {
		// 账号都没列出来，这一趟就根本没跑。把它说成「成功 0，失败 0」，
		// 人会以为没什么要登的。
		return out, fmt.Errorf("the accounts could not be listed: %w", err)
	}
	for _, acc := range accounts {
		if acc.Status != store.StatusSignedOut {
			continue
		}
		if limit > 0 && out.Fixed+out.Failed >= limit {
			break
		}
		cipher, err := m.Store.Password(ctx, acc.ID)
		if err != nil || cipher == "" {
			continue
		}
		if err := m.Relogin(ctx, acc.ID); err != nil {
			if errors.Is(err, ErrNotSignedOut) {
				// 别人刚把它登回去了：这不算失败。
				continue
			}
			out.Failed++
			out.Reasons = append(out.Reasons, accountReason(acc, err))
			continue
		}
		out.Fixed++
	}
	return out, nil
}

// accountReason says which account it was, and what went wrong.
//
// 谁和为什么都要说：一个账号读不到，人得知道是哪一个，才知道去看哪
// 一行。
func accountReason(acc store.Account, err error) string {
	who := acc.Name
	if who == "" {
		who = acc.ID
	}
	return who + "（" + acc.ID + "）：" + err.Error()
}

// UsageRefreshResult says what one round of usage readings came to.
type UsageRefreshResult struct {
	// Refreshed counts the accounts whose credits were read.
	Refreshed int
	// Failed counts the accounts whose credits could not be read.
	Failed int
	// Reasons says why each of those failed, one line per account.
	Reasons []string
}

// RefreshAllUsage reads every account's credits now.
//
// 这是页面上那个「刷新」按钮按下去以后做的事：所有账号的账本现读一
// 遍，不等调度器下一趟。一个账号读不到不影响别的——但要说出来是哪一
// 个，人才知道去看哪一行。
//
// 账号都没列出来的时候，这一趟就根本没跑：把它说成「读了 0 个」，人
// 会以为账本是空的。
func (m *Manager) RefreshAllUsage(ctx context.Context) (UsageRefreshResult, error) {
	var out UsageRefreshResult
	accounts, err := m.Store.ListAccounts(ctx)
	if err != nil {
		return out, fmt.Errorf("the accounts could not be listed: %w", err)
	}
	for _, acc := range accounts {
		if _, err := m.RefreshUsage(ctx, acc.ID); err != nil {
			out.Failed++
			out.Reasons = append(out.Reasons, accountReason(acc, err))
			continue
		}
		out.Refreshed++
	}
	return out, nil
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
			if err := m.markSignedOut(ctx, accountID, tok.Access, err.Error()); err != nil {
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
			if err := m.markSignedOut(ctx, accountID, tok.Access, err.Error()); err != nil {
				return store.UsageSnapshot{}, err
			}
		}
	}

	title, err := acc.PlanTitle(ctx, tok.Access, group)
	switch {
	case err == nil:
		snap.Plan = title
	case isRefusal(err):
		if err := m.markSignedOut(ctx, accountID, tok.Access, err.Error()); err != nil {
			return store.UsageSnapshot{}, err
		}
	default:
		// The plan's name could not be read. The failure is kept:
		// a plan whose name could not be read is not a plan without
		// a name, and Plan: "" would read as "this account has no
		// plan" rather than "the plan could not be read".
		snap.PlanError = err.Error()
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
	if err := m.Store.SaveCheckin(ctx, store.Checkin{
		AccountID: accountID,
		Day:       day,
		Outcome:   out.Outcome,
		Credit:    out.Credit,
		Streak:    out.Streak,
		Message:   out.Message,
		CreatedAt: time.Now(),
	}); err != nil {
		// The check-in worked: the credits are claimed and the day
		// cannot be claimed again. The record of it is lost, so the
		// credit and the streak are recorded nowhere.
		//
		// The failure is written down on the account rather than
		// returned: the caller would tell the user that the check-in
		// failed, and it did not.
		m.note(ctx, accountID, checkinNotWrittenReason+": "+err.Error())
	}
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

// MarkUsed says an account was used.
//
// 聊天成功一次就算一次"用过"：热账号是"刚刚"的账，而这一笔就是刚刚。
func (m *Manager) MarkUsed(accountID string) {
	m.hot.mark(accountID, m.hotWindow(), time.Now())
}

// HotAccounts gives the accounts that were used lately, the most used
// first.
//
// 它给的是"刚刚"的那批账号：读额度的时候它们在热的那一组里。
func (m *Manager) HotAccounts() []string {
	return m.hot.list(m.hotWindow(), time.Now())
}

// hotWindow says how long an account counts as hot.
//
// 配 0 的时候按默认算：0 分钟的窗口等于没有热账号，那不是关掉，是坏了。
func (m *Manager) hotWindow() time.Duration {
	if m.HotWindow <= 0 {
		return defaultHotWindow
	}
	return m.HotWindow
}
