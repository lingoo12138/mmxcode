// Package rotator picks the account a request goes to, and says what
// to do when the account cannot answer.
//
// A conversation is bound to one account and stays there. The binding
// is by session key: the client's own session id when it has one, and
// otherwise a hash of the system prompt and the first user message.
// When the bound account is spent or gone, another is picked and the
// conversation is rebound.
//
// A binding is moved only when the bound account cannot answer at all:
// it is spent, it is signed out, or it is gone. An account that has
// already been tried in this request and failed is another thing: a
// different account answers that one request, and the conversation
// stays where it is. One 5xx does not move a conversation to another
// account for good.
//
// The exhaustion is per account and per plan group. The site's plan
// windows ("5 hours", "7 days") belong to a group, and the models of a
// group spend from the same windows: a group that is spent answers
// none of its models until the window comes back, and a model of
// another group is not touched by it.
//
// The upstream conversation id is the one the database keeps: an
// opaque id, made once and kept, so the upstream sees one conversation
// and the mapping survives a restart. A pool that cannot supply one
// falls back to the id the request derives: the client's own session id
// when it sent one, and a hash of the first message when it did not.
//
// What that does not fix: two conversations whose system prompt and
// first user message are the same, from a client that sends no session
// id of its own, are one session by this design's own definition -- the
// session key is a hash of those two things. They therefore share one
// upstream id, and one upstream conversation, however random that id
// is. The server cannot tell them apart: there is nothing in the
// request that does. The fix for that case is the client sending
// X-Session-Id.
package rotator

import (
	"context"
	"errors"
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
	// SessionState gives the session's row in one read.
	//
	// 那一行的两列（钉在哪个号上、上游的 id）一起拿：分开问就是两次往返。
	SessionState(ctx context.Context, sessionKey string) (store.SessionState, error)
	// MarkExhausted marks an account's window spent.
	//
	// The order is the account, the window key, the time it comes
	// back and the reason. It is the order of
	// Rotator.MarkWindowExhausted.
	MarkExhausted(ctx context.Context, accountID, windowKey string, until time.Time, reason string) error
	// Remaining gives an account's credits left. The second value is
	// false when the credits have not been read yet.
	Remaining(ctx context.Context, accountID string) (float64, bool)
}

// RemainingFor is an optional part of a Pool: it gives the credits
// left for a whole list of accounts at once.
//
// A pool that has it is asked for all the accounts in one question, so
// a pick costs one question and not one per account. A pool without
// it is asked account by account.
//
// The map holds only the accounts whose credits are known: an account
// that is not in it has not been read yet.
type RemainingFor interface {
	RemainingAll(ctx context.Context, accountIDs []string) map[string]Remaining
}

// UpstreamIDs is an optional part of a Pool: it gives the upstream id
// of a session.
//
// The id is the one MiniMax is told about. It belongs to the
// conversation and not to the account the conversation is bound to, so
// it does not change when the conversation moves to another account.
//
// A pool that has it is asked for the id it keeps; a pool without it is
// asked for nothing, and the id the request derives is used instead.
type UpstreamIDs interface {
	UpstreamID(ctx context.Context, sessionKey string) (string, error)
}

// BusyFor is an optional part of a Pool: it says whether an account
// is busy right now — a request is in the air on it.
//
// The accounts that are busy are not picked: a second request on one of
// them spends the same credits twice as fast. A request that finds
// every account that could answer busy gets ErrAllBusy, and waits for
// one of them to come free.
//
// A pool without it is asked nothing: no account is ever busy as far as
// the rotator knows, and everything works as it did before.
type BusyFor interface {
	Busy(ctx context.Context, accountID string) bool
}

// ErrAllBusy says every account that could answer is busy right now.
//
// 它跟「都花完了」是两件事：花完的账号等不来（在飞的会空出来）。网关
// 认得它，就知道该等，而不是报错。
var ErrAllBusy = errors.New("every account that could answer is busy right now")

// Remaining is what is left of an account's credits.
type Remaining struct {
	// Left is how many credits are left.
	Left float64
	// Known says the credits have been read. When it is false, Left
	// says nothing.
	Known bool
}

// The strategies.
const (
	// StrategySticky keeps a conversation on the account it was bound
	// to, and only rotates when there is no conversation to keep.
	StrategySticky = "sticky"
	// StrategyRoundRobin rotates every request, and the binding is
	// not used.
	StrategyRoundRobin = "round-robin"
)

// Options configures a rotator.
type Options struct {
	// MaxAttempts is how many accounts one request may be tried on.
	//
	// Pick counts the accounts the caller has already tried and gives
	// up once there are as many as this, so the budget is the
	// rotator's own and not only the caller's.
	MaxAttempts int
	// Cooldown is how long an account is given up on when the spent
	// window cannot be told apart.
	Cooldown time.Duration
	// WindowGroup says which plan group a model spends from. It
	// defaults to the chat models' group.
	WindowGroup func(model string) string
	// Strategy says how an account is picked. The values are
	// StrategySticky and StrategyRoundRobin.
	//
	// An empty strategy is StrategySticky. Any other value is taken
	// for StrategySticky as well, which is the safe answer but hides
	// the mistake: Validate is where the mistake is seen.
	Strategy string
}

// Validate says whether the options make sense.
//
// A strategy that is neither StrategySticky nor StrategyRoundRobin is
// taken for StrategySticky, so a misspelled strategy does not fail
// loudly on its own. This is where it is caught.
func (o Options) Validate() error {
	switch o.Strategy {
	case "", StrategySticky, StrategyRoundRobin:
		return nil
	}
	return fmt.Errorf("unknown rotation strategy %q: it is %q or %q", o.Strategy, StrategySticky, StrategyRoundRobin)
}

// Plan is the account a request goes to.
type Plan struct {
	// AccountID is the account.
	AccountID string
	// WindowKey is the plan group the request spends.
	//
	// Deprecated: the key is the group of plan windows, not one
	// window. GroupKey says the same thing and is the name to use.
	WindowKey string
	// GroupKey is the plan group the request spends. It is the same
	// key as WindowKey.
	GroupKey string
	// Rebound says the session was moved to another account.
	Rebound bool
	// Retried says another account answered this request because the
	// bound one had already been tried, and the session stayed where
	// it was.
	Retried bool
	// UpstreamID is the id the upstream knows this conversation by.
	//
	// 它是跟着那一行一起读出来的：那一行每个请求都要问一次，所以问出来的
	// 东西就跟着走，不用再问一遍。
	UpstreamID string
}

// Rotator picks accounts.
type Rotator struct {
	pool Pool
	opts Options

	// mu guards the two fields below.
	mu sync.Mutex
	// cursor is where the rotation stands, so two accounts that are
	// equally good are not always taken in the same order.
	cursor int
	// lastUsed says when each account was last handed out, for the
	// "least recently used first" rule.
	lastUsed map[string]time.Time
}

// New makes a rotator.
//
// A strategy that is not one of the known ones is taken for
// StrategySticky. NewChecked is where that is caught.
func New(pool Pool, opts Options) *Rotator {
	r, err := NewChecked(pool, opts)
	if err != nil {
		// The strategy is not one of the known ones. The safe answer is
		// the sticky one; NewChecked is where the mistake is caught.
		opts.Strategy = StrategySticky
		r = newRotator(pool, opts)
	}
	return r
}

// NewChecked makes a rotator and says whether the options make sense.
//
// It is New with the mistake in the options told about instead of
// hidden.
func NewChecked(pool Pool, opts Options) (*Rotator, error) {
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	return newRotator(pool, opts), nil
}

// newRotator makes a rotator with the options filled in.
func newRotator(pool Pool, opts Options) *Rotator {
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
		opts.Strategy = StrategySticky
	}
	return &Rotator{
		pool:     pool,
		opts:     opts,
		lastUsed: map[string]time.Time{},
	}
}

// defaultWindowGroup is the chat models' group.
//
// It is the same rule as manager.WindowGroup, and the two have to be
// kept in step: the manager is the one that reads the site's plan, and
// the key here has to be the one the manager writes down.
func defaultWindowGroup(model string) string {
	if strings.Contains(strings.ToLower(model), "video") {
		return "video"
	}
	return "general"
}

// GroupKey gives the plan group a model spends from.
//
// The key is the group of plan windows and not a single window: the
// windows "5 hours" and "7 days" belong to a group, and the models of
// the group spend from the same windows.
func (r *Rotator) GroupKey(model string) string {
	return r.opts.WindowGroup(model)
}

// WindowKey gives the plan window a model spends from.
//
// Deprecated: the key is the plan group, not one window. Use GroupKey.
func (r *Rotator) WindowKey(model string) string {
	return r.GroupKey(model)
}

// Pick gives the account a request goes to.
//
// used holds the accounts already tried for this request, so a replay
// does not pick the same one again.
//
// Which account it is comes down to these cases:
//
//   - a conversation that is bound to an account stays on it, as long as
//     the account can still answer;
//   - a conversation bound to an account that has already been tried in
//     this request is answered by another account, and the binding is
//     left alone: one failure does not move the conversation;
//   - a conversation bound to a busy account is answered by another
//     account, and the binding is left alone as well: a busy account is
//     not one that cannot answer;
//   - a conversation with no binding takes the least recently used
//     account, and the one with the most credits left when two were
//     used the same, and is bound to it;
//   - a request with no conversation at all rotates: the next account
//     in turn, so the load is spread.
//
// The accounts that are busy right now are not picked. When every
// account that could answer is busy, the error is ErrAllBusy: the
// caller waits for one of them to come free.
//
// The "round-robin" strategy skips the first three cases and rotates
// always.
func (r *Rotator) Pick(ctx context.Context, sessionKey, model string, used map[string]bool) (Plan, error) {
	// The budget of attempts is the rotator's: a request is tried on
	// as many accounts as Options.MaxAttempts says, and not one more.
	if r.opts.MaxAttempts > 0 && len(used) >= r.opts.MaxAttempts {
		return Plan{}, fmt.Errorf("the %d attempts are used up", r.opts.MaxAttempts)
	}

	accounts, err := r.pool.Accounts(ctx)
	if err != nil {
		return Plan{}, err
	}
	exhausted, err := r.pool.Exhausted(ctx)
	if err != nil {
		return Plan{}, err
	}
	usable, group := r.usableAccounts(accounts, exhausted, model, used)
	if len(usable) == 0 {
		return Plan{}, fmt.Errorf("no account can answer: every account is spent for the %s group or has already been tried", group)
	}
	// The accounts that are not busy: a request is in the air on the
	// other ones, and they are not picked.
	free, err := r.freeAccounts(ctx, usable)
	if err != nil {
		return Plan{}, err
	}

	sticky := r.opts.Strategy != StrategyRoundRobin

	// 这一段对话在库里那一行：钉在哪个号上、上游的 id。
	//
	// 那一行以前是分开读的（两次往返），而每个请求都要问它一次。
	var state store.SessionState
	if sessionKey != "" {
		var err error
		state, err = r.pool.SessionState(ctx, sessionKey)
		if err != nil {
			// 读不出来不是「没绑定」：当成没绑定的话，库里那个绑定会被覆
			// 盖掉。
			return Plan{}, fmt.Errorf("the session row could not be read: %w", err)
		}
	}

	// 上游的 id：那一行里的那个。
	//
	// 那一行没有 id（新的对话）：问池子要一个 —— 它会编一个写下去。
	//
	// 只有「那一行还没有 id」的时候才问：问一次就是一次写，而热路径上那一
	// 行已经有 id 了（那一行每个请求都要读一次，所以问出来的东西就跟着
	// 走，不用再问一遍）。
	upstreamID := state.UpstreamID
	if upstreamID == "" && sessionKey != "" {
		if ids, ok := r.pool.(UpstreamIDs); ok {
			var err error
			upstreamID, err = ids.UpstreamID(ctx, sessionKey)
			if err != nil {
				return Plan{}, fmt.Errorf("the upstream id could not be read: %w", err)
			}
		}
	}

	if sticky && sessionKey != "" {
		if state.AccountID != "" {
			boundID := state.AccountID
			bound, found := accountByID(accounts, boundID)
			switch {
			case !found, bound.Status != store.StatusActive:
				// The account is gone or signed out: the binding may
				// be replaced.
			case r.isExhausted(exhausted, boundID, group):
				// The account's group is spent: the binding may be
				// replaced.
			case used[boundID]:
				// The bound account has already been tried in this
				// request and failed. Another account answers this
				// request, and the conversation stays where it is.
				a, err := r.bestAccount(ctx, free)
				if err != nil {
					return Plan{}, err
				}
				r.markUsed(a.ID)
				return r.plan(a, group, false, true, upstreamID), nil
			case r.isBusy(ctx, boundID):
				// The bound account is busy: another account answers
				// this request, and the conversation stays where it
				// is. A busy account is not one that cannot answer.
				a, err := r.bestAccount(ctx, free)
				if err != nil {
					return Plan{}, err
				}
				r.markUsed(a.ID)
				return r.plan(a, group, false, true, upstreamID), nil
			default:
				// The bound account can answer: it answers.
				r.markUsed(boundID)
				return r.plan(store.Account{ID: boundID}, group, false, false, upstreamID), nil
			}
		}
	}

	// No binding, or the bound account cannot answer: another account
	// is picked and the conversation is bound to it.
	if sticky && sessionKey != "" {
		a, err := r.bestAccount(ctx, free)
		if err != nil {
			return Plan{}, err
		}
		if err := r.pool.BindSession(ctx, sessionKey, a.ID); err != nil {
			// The binding was not written. The caller must not be
			// told the conversation has moved: it has not.
			return Plan{}, fmt.Errorf("the session binding could not be written: %w", err)
		}
		r.markUsed(a.ID)
		return r.plan(a, group, true, false, upstreamID), nil
	}

	// No conversation, or the round-robin strategy: the next account
	// in the rotation.
	usableIDs := make(map[string]bool, len(free))
	for _, a := range free {
		usableIDs[a.ID] = true
	}
	a, err := r.nextAccount(accounts, usableIDs)
	if err != nil {
		return Plan{}, err
	}
	r.markUsed(a.ID)
	return r.plan(a, group, false, false, upstreamID), nil
}

// usableAccounts gives the accounts that can answer, and the group the
// request spends.
//
// 能答的账号是签入中的、它们的组没有被标记用完的、而且这个请求里还没
// 有试过的。挑的时候和问"还有没有能答的"的时候用的是同一套口径。
func (r *Rotator) usableAccounts(accounts []store.Account, exhausted []store.Exhausted, model string, used map[string]bool) ([]store.Account, string) {
	group := r.GroupKey(model)
	usable := make([]store.Account, 0, len(accounts))
	for _, a := range accounts {
		if a.Status != store.StatusActive {
			continue
		}
		if used[a.ID] || r.isExhausted(exhausted, a.ID, group) {
			continue
		}
		usable = append(usable, a)
	}
	return usable, group
}

// AnyUsable says whether some account could answer right now.
//
// 它是"等一个账号空出来"问的那一句（规格 §6.4）：能答的账号是签入中
// 的、没有被标记用完的、这个请求里还没有试过的，再减去正在飞的那些。
//
// 谁在飞、谁能用、谁被标记用完了，只有 rotator 知道：账本自己不知道池
// 子里有多少账号，所以"有没有空的账号"这件事只能问它。
//
// 它只看，不挑：不记"用过了"，不绑会话，什么都不改。挑是 Pick 的事。
func (r *Rotator) AnyUsable(ctx context.Context, model string, used map[string]bool) (bool, error) {
	accounts, err := r.pool.Accounts(ctx)
	if err != nil {
		return false, err
	}
	exhausted, err := r.pool.Exhausted(ctx)
	if err != nil {
		return false, err
	}
	usable, _ := r.usableAccounts(accounts, exhausted, model, used)
	if len(usable) == 0 {
		return false, nil
	}
	free, err := r.freeAccounts(ctx, usable)
	if err != nil {
		if errors.Is(err, ErrAllBusy) {
			return false, nil
		}
		return false, err
	}
	return len(free) > 0, nil
}

// freeAccounts gives the accounts that are not busy right now.
//
// 在飞的账号不挑：第二个请求落在它身上，额度掉得就快一倍。
//
// 它们都忙的时候是 ErrAllBusy：那是「等一等」的事，不是「坏了」的事。
func (r *Rotator) freeAccounts(ctx context.Context, usable []store.Account) ([]store.Account, error) {
	busy, ok := r.pool.(BusyFor)
	if !ok {
		// 这个池子说不了谁在飞：跟从前一样，一个都不算忙。
		return usable, nil
	}
	free := make([]store.Account, 0, len(usable))
	for _, a := range usable {
		if busy.Busy(ctx, a.ID) {
			continue
		}
		free = append(free, a)
	}
	if len(free) == 0 {
		return nil, ErrAllBusy
	}
	return free, nil
}

// isBusy says whether a request is in the air on an account.
//
// 池子说不了谁在飞的时候，谁也不忙。
func (r *Rotator) isBusy(ctx context.Context, accountID string) bool {
	busy, ok := r.pool.(BusyFor)
	if !ok {
		return false
	}
	return busy.Busy(ctx, accountID)
}

// plan gives the plan of a request that goes to an account.
func (r *Rotator) plan(a store.Account, group string, rebound, retried bool, upstreamID string) Plan {
	return Plan{
		AccountID:  a.ID,
		WindowKey:  group,
		GroupKey:   group,
		Rebound:    rebound,
		Retried:    retried,
		UpstreamID: upstreamID,
	}
}

// UpstreamID gives the id the upstream is told about for a session.
//
// The pool's id is used when the pool has one: it is kept in the
// database, so it is the same for every request of the conversation,
// and it survives a restart.
//
// A pool that cannot supply one is asked for nothing, and the derived
// id the session carries is used: the client's own session id when it
// sent one, and the hash of the first message when it did not.
//
// A session with no key names no conversation: the id is empty and the
// upstream is told nothing.
//
// A pool that has the id but cannot read it is an error. The derived id
// would be a working id, but it is not the one the conversation was
// sent with before, and the upstream would take the conversation for a
// new one. Saying so is better than that.
func (r *Rotator) UpstreamID(ctx context.Context, sess Session) (string, error) {
	if sess.Key == "" {
		// 没有会话就没有对话要命名：池子也不问，空的会话行不该有。
		return "", nil
	}
	pool, ok := r.pool.(UpstreamIDs)
	if !ok {
		// 这个池子没有存着的上游 id：退回请求自己算出来的那个。
		return sess.UpstreamID, nil
	}
	id, err := pool.UpstreamID(ctx, sess.Key)
	if err != nil {
		return "", fmt.Errorf("the upstream id could not be read: %w", err)
	}
	if id == "" {
		return sess.UpstreamID, nil
	}
	return id, nil
}

// accountByID finds an account in a list.
func accountByID(accounts []store.Account, id string) (store.Account, bool) {
	for _, a := range accounts {
		if a.ID == id {
			return a, true
		}
	}
	return store.Account{}, false
}

// bestAccount gives the account that was used least recently, and the
// one with the most credits left when two were used the same.
//
// An account whose credits have not been read is taken after the ones
// whose credits are known: a known balance is the better guide, and
// the account is still tried, unlike a spent one.
func (r *Rotator) bestAccount(ctx context.Context, usable []store.Account) (store.Account, error) {
	left := r.remaining(ctx, usable)
	var best *scored
	for _, a := range usable {
		rem := left[a.ID]
		s := scored{acc: a, known: rem.Known, left: rem.Left, usedAt: r.usedAt(a.ID)}
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

// remaining gives the credits left of every account in the list.
//
// A pool that can answer for the whole list in one go is asked to; a
// pool that cannot is asked account by account.
func (r *Rotator) remaining(ctx context.Context, accounts []store.Account) map[string]Remaining {
	out := make(map[string]Remaining, len(accounts))
	ids := make([]string, 0, len(accounts))
	for _, a := range accounts {
		ids = append(ids, a.ID)
	}
	if batch, ok := r.pool.(RemainingFor); ok {
		for id, rem := range batch.RemainingAll(ctx, ids) {
			out[id] = rem
		}
		return out
	}
	for _, id := range ids {
		left, known := r.pool.Remaining(ctx, id)
		out[id] = Remaining{Left: left, Known: known}
	}
	return out
}

// scored is an account with the numbers the choice is made on.
type scored struct {
	acc    store.Account
	known  bool
	left   float64
	usedAt time.Time
}

// better says one account is a better pick than another.
//
// 挑号的规矩，从要紧的到不要紧的：
//
//  1. 读到余额的排前面 —— 那是能拿来比较的数字。没读到的不算零，它排在
//     有数的号后面（规格不变量 2）。
//  2. 读到「0」的排到有数的号后面 —— 它接不了活了，挑它就是撞一次空，
//     撞空要一个上游请求，还占着并发。
//  3. 最久没用过的排前面 —— 把用量摊开：别让一个号被别的号挤在后面一直
//     闲着，到头来被一口气烧光（规格 §6.5）。
//  4. 一样的话余额多的排前面。
//  5. 再一样就按 ID 排 —— 稳定：同样的情况挑出同一个号来。
func better(a, b scored) bool {
	if a.known != b.known {
		// 读到余额的排前面：那是能拿来比较的数字
		return a.known
	}
	if a.known {
		aEmpty := a.left <= 0
		bEmpty := b.left <= 0
		if aEmpty != bEmpty {
			// 读到 0 的排后面：它撞空是必然的。
			return bEmpty
		}
	}
	if !a.usedAt.Equal(b.usedAt) {
		// 最久没用过的排前面
		return a.usedAt.Before(b.usedAt)
	}
	if a.known && a.left != b.left {
		return a.left > b.left
	}
	return a.acc.ID < b.acc.ID
}

// nextAccount gives the next account in the rotation.
//
// The rule is the one the other path uses as well: the account that
// was handed out least recently comes next. An account that has never
// been handed out has never been used, so it comes before one that
// has.
//
// When several accounts share the oldest moment, the one the cursor
// points at is taken, so the rotation keeps moving.
func (r *Rotator) nextAccount(all []store.Account, usable map[string]bool) (store.Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	var bestAt time.Time
	var candidates []int
	found := false
	for i, a := range all {
		if !usable[a.ID] {
			continue
		}
		at := r.lastUsed[a.ID]
		switch {
		case !found, at.Before(bestAt):
			bestAt = at
			candidates = []int{i}
			found = true
		case at.Equal(bestAt):
			candidates = append(candidates, i)
		}
	}
	if !found {
		return store.Account{}, fmt.Errorf("no account can answer")
	}

	idx := candidates[0]
	if len(candidates) > 1 {
		start := r.cursor % len(all)
		for _, c := range candidates {
			if c >= start {
				idx = c
				break
			}
		}
	}
	r.cursor = idx + 1
	// 给出去的就是游标挑的那个：候选都是同一个时刻，差别只在游标。
	return all[idx], nil
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

// MarkExhausted marks an account's group spent.
//
// The arguments are the account, the group, the reason and the time
// the group comes back.
//
// Deprecated: the last two are the other way round from the Pool's
// MarkExhausted, and the two are easy to mix up. MarkWindowExhausted
// has the Pool's order.
func (r *Rotator) MarkExhausted(ctx context.Context, accountID, windowKey, reason string, until time.Time) error {
	return r.MarkWindowExhausted(ctx, accountID, windowKey, until, reason)
}

// MarkWindowExhausted marks an account's group spent.
//
// The arguments are the account, the group, the time the group comes
// back and the reason: the same order as the Pool's MarkExhausted.
//
// A time of zero means the group comes back after the cooldown.
func (r *Rotator) MarkWindowExhausted(ctx context.Context, accountID, windowKey string, until time.Time, reason string) error {
	if until.IsZero() {
		until = time.Now().Add(r.opts.Cooldown)
	}
	return r.pool.MarkExhausted(ctx, accountID, windowKey, until, reason)
}

// isExhausted says an account's group is spent.
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
