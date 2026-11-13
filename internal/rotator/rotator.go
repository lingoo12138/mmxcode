// Package rotator picks the account a request goes to, and says what
// to do when the account cannot answer.
//
// A conversation is bound to one account and stays there. The binding
// is by session key: the client's own session id when it has one, and
// otherwise a hash of the system prompt and the first user message.
// When the bound account is spent or gone, another is picked and the
// conversation is rebound.
//
// The exhaustion is per account and per plan window: an account whose
// five-hour window is full can still serve another window.
package rotator

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"mmxcode/internal/store"
)

// Pool is the account pool the rotator picks from.
type Pool interface {
	// Accounts gives every account.
	Accounts(ctx context.Context) ([]store.Account, error)
	// Exhausted gives the windows that are spent.
	Exhausted(ctx context.Context) ([]store.Exhausted, error)
	// BindSession points a session at an account.
	BindSession(ctx context.Context, sessionKey, accountID string) error
	// SessionAccount gives the account a session is bound to.
	SessionAccount(ctx context.Context, sessionKey string) (string, bool, error)
	// MarkExhausted marks an account's window spent.
	MarkExhausted(ctx context.Context, accountID, windowKey string, until time.Time, reason string) error
	// Remaining gives an account's credits left. The second value is
	// false when the credits have not been read yet.
	Remaining(ctx context.Context, accountID string) (float64, bool)
}

// Options configures a rotator.
type Options struct {
	// MaxAttempts is how many accounts one request may be tried on.
	MaxAttempts int
	// Cooldown is how long an account is given up on when the spent
	// window cannot be told apart.
	Cooldown time.Duration
	// WindowGroup says which plan window a model spends from. It
	// defaults to the chat models' group.
	WindowGroup func(model string) string
	// Strategy says how an account is picked.
	//
	// "sticky" keeps a conversation on the account it was bound to, and
	// only rotates when there is no conversation to keep.
	// "round-robin" rotates every request, and the binding is not used.
	Strategy string
}

// Plan is the account a request goes to.
type Plan struct {
	// AccountID is the account.
	AccountID string
	// WindowKey is the plan window the request spends.
	WindowKey string
	// Rebound says the session was moved to another account.
	Rebound bool
}

// Rotator picks accounts.
type Rotator struct {
	pool Pool
	opts Options

	// mu guards the two fields below.
	mu sync.Mutex
	// cursor is where the rotation stands, so two requests in a row do
	// not land on the same account.
	cursor int
	// lastUsed says when each account was last handed out, for the
	// "least recently used first" rule.
	lastUsed map[string]time.Time
}

// New makes a rotator.
func New(pool Pool, opts Options) *Rotator {
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = 3
	}
	if opts.Cooldown <= 0 {
		opts.Cooldown = 5 * time.Minute
	}
	if opts.WindowGroup == nil {
		opts.WindowGroup = defaultWindowGroup
	}
	if opts.Strategy == "" {
		opts.Strategy = "sticky"
	}
	return &Rotator{
		pool:     pool,
		opts:     opts,
		lastUsed: map[string]time.Time{},
	}
}

// defaultWindowGroup is the chat models' group.
func defaultWindowGroup(model string) string {
	if strings.Contains(strings.ToLower(model), "video") {
		return "video"
	}
	return "general"
}

// WindowKey gives the plan window a model spends from.
func (r *Rotator) WindowKey(model string) string {
	return r.opts.WindowGroup(model)
}

// Pick gives the account a request goes to.
//
// used holds the accounts already tried for this request, so a replay
// does not pick the same one again.
//
// Which account it is comes down to three cases:
//
//   - a conversation that is bound to an account stays on it, as long as
//     the account can still answer;
//   - a conversation with no binding takes the account with the most
//     credits left, and the least recently used one when those are
//     equal;
//   - a request with no conversation at all rotates: the next account
//     in turn, so the load is spread.
//
// The "round-robin" strategy skips the first case and rotates always.
func (r *Rotator) Pick(ctx context.Context, sessionKey, model string, used map[string]bool) (Plan, error) {
	accounts, err := r.pool.Accounts(ctx)
	if err != nil {
		return Plan{}, err
	}
	exhausted, err := r.pool.Exhausted(ctx)
	if err != nil {
		return Plan{}, err
	}
	window := r.WindowKey(model)

	usable := make([]store.Account, 0, len(accounts))
	for _, a := range accounts {
		if a.Status != store.StatusActive {
			continue
		}
		if used[a.ID] || r.isExhausted(exhausted, a.ID, window) {
			continue
		}
		usable = append(usable, a)
	}
	if len(usable) == 0 {
		return Plan{}, fmt.Errorf("every account is spent for the %s window", window)
	}

	sticky := r.opts.Strategy != "round-robin"

	// 会话已绑定的账号能用就用
	if sticky && sessionKey != "" {
		if id, ok, err := r.pool.SessionAccount(ctx, sessionKey); err == nil && ok {
			for _, a := range usable {
				if a.ID != id {
					continue
				}
				r.markUsed(id)
				return Plan{
					AccountID: id,
					WindowKey: window,
				}, nil
			}
		}
	}

	// 会话没有绑定：挑余额最多的，其次最久没用的
	if sticky && sessionKey != "" {
		a, err := r.bestAccount(ctx, usable)
		if err != nil {
			return Plan{}, err
		}
		_ = r.pool.BindSession(ctx, sessionKey, a.ID)
		r.markUsed(a.ID)
		return Plan{
			AccountID: a.ID,
			WindowKey: window,
			Rebound:   true,
		}, nil
	}

	// 没有会话，或者就是要轮询：下一个
	a := r.nextAccount(usable)
	r.markUsed(a.ID)
	return Plan{
		AccountID: a.ID,
		WindowKey: window,
	}, nil
}

// bestAccount gives the account with the most credits left, and the
// least recently used one when two have the same.
//
// An account whose credits have not been read is not treated as empty:
// it would be pushed to the back of the queue every time, and it is
// more likely to be an account that was just added.
func (r *Rotator) bestAccount(ctx context.Context, usable []store.Account) (store.Account, error) {
	var best *scored
	for _, a := range usable {
		left, known := r.pool.Remaining(ctx, a.ID)
		s := scored{acc: a, known: known, left: left, usedAt: r.usedAt(a.ID)}
		if best == nil {
			best = &s
			continue
		}
		if better(s, *best) {
			best = &s
		}
	}
	if best == nil {
		return store.Account{}, fmt.Errorf("no account is usable")
	}
	return best.acc, nil
}

// scored is an account with the numbers the choice is made on.
type scored struct {
	acc    store.Account
	known  bool
	left   float64
	usedAt time.Time
}

// better says one account is a better pick than another.
func better(a, b scored) bool {
	if a.known != b.known {
		// 余额没读到的排前面：它多半是刚加进来的账号
		return a.known
	}
	if a.known && a.left != b.left {
		return a.left > b.left
	}
	// 一样就用最久没用的那个
	return a.usedAt.Before(b.usedAt)
}

// nextAccount gives the next account in the rotation.
//
// The cursor moves on every call, so two requests in a row do not land
// on the same account.
func (r *Rotator) nextAccount(usable []store.Account) store.Account {
	r.mu.Lock()
	defer r.mu.Unlock()
	i := r.cursor % len(usable)
	r.cursor++
	return usable[i]
}

// markUsed records that an account was handed out.
func (r *Rotator) markUsed(accountID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastUsed[accountID] = time.Now()
}

// usedAt says when an account was last handed out.
func (r *Rotator) usedAt(accountID string) time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastUsed[accountID]
}

// MarkExhausted marks an account's window spent.
func (r *Rotator) MarkExhausted(ctx context.Context, accountID, windowKey, reason string, until time.Time) error {
	if until.IsZero() {
		until = time.Now().Add(r.opts.Cooldown)
	}
	return r.pool.MarkExhausted(ctx, accountID, windowKey, until, reason)
}

// isExhausted says an account's window is spent.
func (r *Rotator) isExhausted(exhausted []store.Exhausted, accountID, windowKey string) bool {
	now := time.Now()
	for _, e := range exhausted {
		if e.AccountID != accountID {
			continue
		}
		if !e.Until.After(now) {
			continue
		}
		// 账号级耗尽（窗口键为空）对所有窗口都算
		if e.WindowKey == "" || e.WindowKey == windowKey {
			return true
		}
	}
	return false
}
