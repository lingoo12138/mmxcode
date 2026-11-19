package rotator

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"mmxcode/internal/store"
)

// fakePool is an account pool that does not touch a database.
type fakePool struct {
	accounts  []store.Account
	exhausted []store.Exhausted
	sessions  map[string]string
	bound     []string
	marked    []markedWindow
	remaining map[string]float64

	// sessionErr is what SessionState fails with.
	sessionErr error
	// bindErr is what BindSession fails with.
	bindErr error
	// remainingCalls counts the questions asked for one account's
	// credits.
	remainingCalls int
	// sessionReads counts the reads of the session's row.
	sessionReads int
}

// markedWindow is one window the pool was told to mark spent.
type markedWindow struct {
	accountID string
	windowKey string
	until     time.Time
	reason    string
}

func newFakePool(accounts ...store.Account) *fakePool {
	return &fakePool{accounts: accounts, sessions: map[string]string{}}
}

func (p *fakePool) Accounts(context.Context) ([]store.Account, error) { return p.accounts, nil }

func (p *fakePool) Exhausted(context.Context) ([]store.Exhausted, error) { return p.exhausted, nil }

func (p *fakePool) BindSession(_ context.Context, key, accountID string) error {
	if p.bindErr != nil {
		return p.bindErr
	}
	p.sessions[key] = accountID
	p.bound = append(p.bound, key+"="+accountID)
	return nil
}

// SessionState gives the session's row in one read.
//
// 那一行的两列（钉在哪个号上、上游的 id）一起拿：分开问就是两次往返。
func (p *fakePool) SessionState(_ context.Context, key string) (store.SessionState, error) {
	p.sessionReads++
	if p.sessionErr != nil {
		return store.SessionState{}, p.sessionErr
	}
	id, ok := p.sessions[key]
	if !ok {
		return store.SessionState{Known: true}, nil
	}
	return store.SessionState{Known: true, AccountID: id}, nil
}

func (p *fakePool) Remaining(_ context.Context, accountID string) (float64, bool) {
	p.remainingCalls++
	left, ok := p.remaining[accountID]
	return left, ok
}

func (p *fakePool) MarkExhausted(_ context.Context, accountID, windowKey string, until time.Time, reason string) error {
	p.marked = append(p.marked, markedWindow{
		accountID: accountID, windowKey: windowKey, until: until, reason: reason,
	})
	p.exhausted = append(p.exhausted, store.Exhausted{
		AccountID: accountID, WindowKey: windowKey, Until: until, Reason: reason,
	})
	return nil
}

// batchPool is a pool that answers for a whole list of accounts at
// once.
type batchPool struct {
	*fakePool
	calls int
	ids   []string
}

func (p *batchPool) RemainingAll(_ context.Context, ids []string) map[string]Remaining {
	p.calls++
	p.ids = ids
	out := map[string]Remaining{}
	for _, id := range ids {
		if left, ok := p.remaining[id]; ok {
			out[id] = Remaining{Left: left, Known: true}
		}
	}
	return out
}

// busyPool is a pool that knows which accounts are busy.
//
// 池子知道谁在飞：网关把在飞的计数报给它，rotator 挑账号的时候避开它。
type busyPool struct {
	*fakePool
	// busy holds the accounts that have a request in the air on them.
	busy map[string]bool
}

// Busy says whether a request is in the air on an account.
func (p *busyPool) Busy(_ context.Context, accountID string) bool {
	return p.busy[accountID]
}

func acc(id string) store.Account {
	return store.Account{ID: id, Name: id, Status: store.StatusActive}
}

// TestTheSessionRowIsReadInOneGo says the rotator reads the session's
// row in one read.
//
// 那一行的两列是同一行：分开读就是两次往返，而每个请求都要问这一行。
func TestTheSessionRowIsReadInOneGo(t *testing.T) {
	p := newFakePool(acc("acc_01"))
	r := newRotator(p, Options{Strategy: StrategySticky})

	if _, err := r.Pick(context.Background(), "sess-1", "MiniMax-M3", nil); err != nil {
		t.Fatal(err)
	}
	if got := p.sessionReads; got != 1 {
		t.Errorf("the reads of the session row = %d, want 1", got)
	}
}

func TestPickBindsSessionToAnAccount(t *testing.T) {
	p := newFakePool(acc("acc_01"), acc("acc_02"))
	r := New(p, Options{MaxAttempts: 3})

	first, err := r.Pick(context.Background(), "sess-1", "MiniMax-M3", nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.Pick(context.Background(), "sess-1", "MiniMax-M3", nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.AccountID != second.AccountID {
		t.Fatalf("the session moved: %s then %s", first.AccountID, second.AccountID)
	}
	if len(p.bound) != 1 {
		t.Fatalf("bound %v", p.bound)
	}
}

func TestPickSkipsExhaustedAccounts(t *testing.T) {
	p := newFakePool(acc("acc_01"), acc("acc_02"))
	p.exhausted = []store.Exhausted{{
		AccountID: "acc_01", WindowKey: "general",
		Until: time.Now().Add(time.Hour), Reason: "quota",
	}}
	r := New(p, Options{MaxAttempts: 3})
	got, err := r.Pick(context.Background(), "sess-1", "MiniMax-M3", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccountID != "acc_02" {
		t.Fatalf("picked %s", got.AccountID)
	}
}

func TestPickIgnoresExpiredExhaustion(t *testing.T) {
	p := newFakePool(acc("acc_01"))
	p.exhausted = []store.Exhausted{{
		AccountID: "acc_01", WindowKey: "general",
		Until: time.Now().Add(-time.Minute), Reason: "quota",
	}}
	r := New(p, Options{MaxAttempts: 3})
	got, err := r.Pick(context.Background(), "sess-1", "MiniMax-M3", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccountID != "acc_01" {
		t.Fatalf("picked %s", got.AccountID)
	}
}

func TestPickSkipsSignedOutAccounts(t *testing.T) {
	a := acc("acc_01")
	a.Status = store.StatusSignedOut
	p := newFakePool(a, acc("acc_02"))
	r := New(p, Options{MaxAttempts: 3})
	got, err := r.Pick(context.Background(), "sess-1", "MiniMax-M3", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccountID != "acc_02" {
		t.Fatalf("picked %s", got.AccountID)
	}
}

func TestPickFailsOverToAnotherAccount(t *testing.T) {
	p := newFakePool(acc("acc_01"), acc("acc_02"))
	r := New(p, Options{MaxAttempts: 3})

	first, _ := r.Pick(context.Background(), "sess-1", "MiniMax-M3", nil)
	// 第一个账号在这个请求里已经试过了，重放时不能再选它
	used := map[string]bool{first.AccountID: true}
	second, err := r.Pick(context.Background(), "sess-1", "MiniMax-M3", used)
	if err != nil {
		t.Fatal(err)
	}
	if second.AccountID == first.AccountID {
		t.Fatal("the account that was tried again was picked again")
	}
	// 换的只是这个请求的账号：会话还绑在原来那个上
	if p.sessions["sess-1"] != first.AccountID {
		t.Fatalf("the binding moved to %s", p.sessions["sess-1"])
	}
}

func TestPickKeepsTheBindingWhenTheBoundAccountWasAlreadyTried(t *testing.T) {
	p := newFakePool(acc("acc_01"), acc("acc_02"))
	p.sessions["sess-1"] = "acc_01"
	r := New(p, Options{MaxAttempts: 3})

	// acc_01 已经试过了：换个账号回答这次请求，但不动绑定
	got, err := r.Pick(context.Background(), "sess-1", "MiniMax-M3", map[string]bool{"acc_01": true})
	if err != nil {
		t.Fatal(err)
	}
	if got.AccountID != "acc_02" {
		t.Fatalf("picked %s", got.AccountID)
	}
	if p.sessions["sess-1"] != "acc_01" {
		t.Fatalf("the binding moved to %s", p.sessions["sess-1"])
	}
	if len(p.bound) != 0 {
		t.Fatalf("bound %v", p.bound)
	}
	if got.Rebound {
		t.Fatal("the plan says the session was rebound")
	}
	if !got.Retried {
		t.Fatal("the plan does not say the request was answered by another account")
	}
}

// TestTheBusyAccountIsNotPicked says an account with a request in the
// air on it is not picked.
//
// 在飞的账号再接一个请求，额度掉得就快一倍：它们用的是同一份额度。
// 挑账号的时候要避开在飞的那些（规格 §6.3）。
func TestTheBusyAccountIsNotPicked(t *testing.T) {
	p := &busyPool{
		fakePool: newFakePool(acc("acc_01"), acc("acc_02")),
		busy:     map[string]bool{"acc_01": true},
	}
	r := New(p, Options{MaxAttempts: 3})

	got, err := r.Pick(context.Background(), "sess-1", "MiniMax-M3", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccountID != "acc_02" {
		t.Fatalf("picked %s, want the one that is not busy", got.AccountID)
	}
}

// TestTheRotatorSaysWhetherSomeAccountCanAnswer says the rotator says
// whether an account could answer right now.
//
// 谁在飞、谁能用、谁被标记用完了，只有 rotator 知道：账本自己不知道池
// 子里有多少账号。"等一个账号空出来"问的就是这一句（规格 §6.4）。
//
// 它只看，不挑：挑不动任何东西。
func TestTheRotatorSaysWhetherSomeAccountCanAnswer(t *testing.T) {
	p := &busyPool{
		fakePool: newFakePool(acc("acc_01"), acc("acc_02")),
		busy:     map[string]bool{},
	}
	r := New(p, Options{MaxAttempts: 3})
	ctx := context.Background()

	ok, err := r.AnyUsable(ctx, "MiniMax-M3", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("no account can answer, want one")
	}
	// 一个在飞，另一个空着：还有能答的。
	p.busy["acc_01"] = true
	ok, err = r.AnyUsable(ctx, "MiniMax-M3", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("no account can answer, want the one that is not busy")
	}
	// 两个都在飞：没有了。
	p.busy["acc_02"] = true
	ok, err = r.AnyUsable(ctx, "MiniMax-M3", nil)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("an account can answer, want none")
	}
	// 一个已经试过的账号不算：这个请求不再用它。
	p.busy = map[string]bool{}
	ok, err = r.AnyUsable(ctx, "MiniMax-M3", map[string]bool{"acc_02": true})
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("no account can answer, want acc_01")
	}
	ok, err = r.AnyUsable(ctx, "MiniMax-M3", map[string]bool{"acc_01": true, "acc_02": true})
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("an account can answer, want none")
	}
}

// TestTheRotatorDoesNotPickWhenItIsAsked says the question does not
// pick an account and does not bind a session.
//
// 它是"还有没有能答的账号"这一句：问它的人自己会去挑。
func TestTheRotatorDoesNotPickWhenItIsAsked(t *testing.T) {
	p := &busyPool{
		fakePool: newFakePool(acc("acc_01"), acc("acc_02")),
		busy:     map[string]bool{},
	}
	r := New(p, Options{MaxAttempts: 3})

	if _, err := r.AnyUsable(context.Background(), "MiniMax-M3", nil); err != nil {
		t.Fatal(err)
	}
	if len(p.bound) != 0 {
		t.Fatalf("bound %v", p.bound)
	}
}

// TestEveryAccountBusyIsSaidSo says the rotator says the accounts are
// busy, and not that they are spent.
//
// 都忙跟都花完是两件事：忙的等一等就空出来了，花完的等不来。网关要
// 认得这个差别，才知道该等，而不是报错（规格 §6.4）。
func TestEveryAccountBusyIsSaidSo(t *testing.T) {
	p := &busyPool{
		fakePool: newFakePool(acc("acc_01")),
		busy:     map[string]bool{"acc_01": true},
	}
	r := New(p, Options{MaxAttempts: 3})

	_, err := r.Pick(context.Background(), "sess-1", "MiniMax-M3", nil)
	if !errors.Is(err, ErrAllBusy) {
		t.Fatalf("the error = %v, want the all-busy one", err)
	}
}

// TestTheBindingStaysWhenTheBoundAccountIsBusy says a busy bound
// account does not move the binding.
//
// 绑定是"这个会话的家"：忙不是它不能答了的理由（规格 §6.1）。忙的时候
// 换一个账号答这一次，绑定还在原来那个账号上。
func TestTheBindingStaysWhenTheBoundAccountIsBusy(t *testing.T) {
	p := &busyPool{
		fakePool: newFakePool(acc("acc_01"), acc("acc_02")),
		busy:     map[string]bool{"acc_01": true},
	}
	p.sessions["sess-1"] = "acc_01"
	r := New(p, Options{MaxAttempts: 3})

	got, err := r.Pick(context.Background(), "sess-1", "MiniMax-M3", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccountID != "acc_02" {
		t.Fatalf("picked %s, want the one that is not busy", got.AccountID)
	}
	if p.sessions["sess-1"] != "acc_01" {
		t.Fatalf("the binding moved to %s", p.sessions["sess-1"])
	}
	if len(p.bound) != 0 {
		t.Fatalf("bound %v", p.bound)
	}
	if got.Rebound {
		t.Fatal("the plan says the session was rebound")
	}
	if !got.Retried {
		t.Fatal("the plan does not say the request was answered by another account")
	}
}

// TestAPoolThatCannotSayIsNotAsked says a pool that cannot say which
// accounts are busy is not asked.
//
// 没有那个说法的池子：跟从前一样，一个账号都不算忙（规格 §6.3 的那
// 个查询是可选的）。
func TestAPoolThatCannotSayIsNotAsked(t *testing.T) {
	p := newFakePool(acc("acc_01"))
	r := New(p, Options{MaxAttempts: 3})
	got, err := r.Pick(context.Background(), "sess-1", "MiniMax-M3", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccountID != "acc_01" {
		t.Fatalf("picked %s", got.AccountID)
	}
}

func TestPickRebindsWhenTheBoundAccountIsExhausted(t *testing.T) {
	p := newFakePool(acc("acc_01"), acc("acc_02"))
	p.sessions["sess-1"] = "acc_01"
	p.exhausted = []store.Exhausted{{
		AccountID: "acc_01", WindowKey: "general",
		Until: time.Now().Add(time.Hour), Reason: "quota",
	}}
	r := New(p, Options{MaxAttempts: 3})
	got, err := r.Pick(context.Background(), "sess-1", "MiniMax-M3", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccountID != "acc_02" {
		t.Fatalf("picked %s", got.AccountID)
	}
	if p.sessions["sess-1"] != "acc_02" {
		t.Fatalf("the binding stayed on %s", p.sessions["sess-1"])
	}
	if !got.Rebound {
		t.Fatal("the plan does not say the session was rebound")
	}
}

func TestPickRebindsWhenTheBoundAccountIsSignedOut(t *testing.T) {
	a := acc("acc_01")
	a.Status = store.StatusSignedOut
	p := newFakePool(a, acc("acc_02"))
	p.sessions["sess-1"] = "acc_01"
	r := New(p, Options{MaxAttempts: 3})
	got, err := r.Pick(context.Background(), "sess-1", "MiniMax-M3", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccountID != "acc_02" {
		t.Fatalf("picked %s", got.AccountID)
	}
	if p.sessions["sess-1"] != "acc_02" {
		t.Fatalf("the binding stayed on %s", p.sessions["sess-1"])
	}
}

func TestPickRebindsWhenTheBoundAccountIsGone(t *testing.T) {
	p := newFakePool(acc("acc_02"))
	p.sessions["sess-1"] = "acc_01"
	r := New(p, Options{MaxAttempts: 3})
	got, err := r.Pick(context.Background(), "sess-1", "MiniMax-M3", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccountID != "acc_02" {
		t.Fatalf("picked %s", got.AccountID)
	}
	if p.sessions["sess-1"] != "acc_02" {
		t.Fatalf("the binding stayed on %s", p.sessions["sess-1"])
	}
}

func TestPickFailsWhenTheSessionCannotBeRead(t *testing.T) {
	p := newFakePool(acc("acc_01"), acc("acc_02"))
	p.sessions["sess-1"] = "acc_01"
	p.sessionErr = errors.New("the database is down")
	r := New(p, Options{MaxAttempts: 3})

	if _, err := r.Pick(context.Background(), "sess-1", "MiniMax-M3", nil); err == nil {
		t.Fatal("expected an error")
	}
	// 读不到绑定不是"没有绑定"：原来那个不能被改写
	if p.sessions["sess-1"] != "acc_01" {
		t.Fatalf("the binding moved to %s", p.sessions["sess-1"])
	}
	if len(p.bound) != 0 {
		t.Fatalf("bound %v", p.bound)
	}
}

func TestPickFailsWhenTheBindingCannotBeWritten(t *testing.T) {
	p := newFakePool(acc("acc_01"), acc("acc_02"))
	p.bindErr = errors.New("the database is down")
	r := New(p, Options{MaxAttempts: 3})

	if _, err := r.Pick(context.Background(), "sess-1", "MiniMax-M3", nil); err == nil {
		t.Fatal("expected an error")
	}
	if len(p.bound) != 0 {
		t.Fatalf("bound %v", p.bound)
	}
}

func TestPickFailsWhenEveryAccountIsExhausted(t *testing.T) {
	p := newFakePool(acc("acc_01"))
	p.exhausted = []store.Exhausted{{
		AccountID: "acc_01", WindowKey: "general",
		Until: time.Now().Add(time.Hour), Reason: "quota",
	}}
	r := New(p, Options{MaxAttempts: 3})
	if _, err := r.Pick(context.Background(), "sess-1", "MiniMax-M3", nil); err == nil {
		t.Fatal("expected an error")
	}
}

func TestNoSessionRotates(t *testing.T) {
	// 没有会话就没有可粘的东西，退化为轮询
	p := newFakePool(acc("acc_01"), acc("acc_02"))
	r := New(p, Options{MaxAttempts: 3})
	a, err := r.Pick(context.Background(), "", "MiniMax-M3", nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.Pick(context.Background(), "", "MiniMax-M3", nil)
	if err != nil {
		t.Fatal(err)
	}
	if a.AccountID == b.AccountID {
		t.Fatalf("both requests went to %s", a.AccountID)
	}
}

func TestRoundRobinIgnoresTheBinding(t *testing.T) {
	p := newFakePool(acc("acc_01"), acc("acc_02"))
	p.sessions["sess-1"] = "acc_01"
	r := New(p, Options{MaxAttempts: 3, Strategy: StrategyRoundRobin})
	a, err := r.Pick(context.Background(), "sess-1", "MiniMax-M3", nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.Pick(context.Background(), "sess-1", "MiniMax-M3", nil)
	if err != nil {
		t.Fatal(err)
	}
	if a.AccountID == b.AccountID {
		t.Fatalf("both went to %s", a.AccountID)
	}
}

func TestRoundRobinWalksOverTheAccountsThatCannotAnswer(t *testing.T) {
	// acc_01 用过了，acc_02 退出了：轮询跳过它们
	p := newFakePool(acc("acc_01"), acc("acc_02"), acc("acc_03"))
	p.accounts[1].Status = store.StatusSignedOut
	r := New(p, Options{MaxAttempts: 3, Strategy: StrategyRoundRobin})

	first, err := r.Pick(context.Background(), "", "MiniMax-M3", map[string]bool{"acc_01": true})
	if err != nil {
		t.Fatal(err)
	}
	if first.AccountID != "acc_03" {
		t.Fatalf("picked %s", first.AccountID)
	}
}

func TestRoundRobinTakesTheLeastRecentlyUsedAccount(t *testing.T) {
	// 轮询用的也是"最久没用过的先来"这条规则
	p := newFakePool(acc("acc_01"), acc("acc_02"), acc("acc_03"))
	r := New(p, Options{MaxAttempts: 3, Strategy: StrategyRoundRobin})

	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		got, err := r.Pick(context.Background(), "", "MiniMax-M3", nil)
		if err != nil {
			t.Fatal(err)
		}
		if seen[got.AccountID] {
			t.Fatalf("%s was picked twice in three picks", got.AccountID)
		}
		seen[got.AccountID] = true
	}
}

// TestTheCursorBreaksTheTie says the cursor is what decides between
// the accounts that were used at the same moment.
//
// 最久没用过的时刻一样的账号有好几个时，光看时刻分不出来。游标指着
// 谁就给谁：轮转才转得起来。
func TestTheCursorBreaksTheTie(t *testing.T) {
	p := newFakePool(acc("acc_01"), acc("acc_02"), acc("acc_03"))
	r := New(p, Options{MaxAttempts: 3, Strategy: StrategyRoundRobin})
	// 中间那个用过，另外两个一次也没用过：时刻一样，谁也不比谁老。
	r.lastUsed["acc_02"] = time.Now()
	// 游标停在 acc_02 上：它用过，不在候选里；下一个候选是 acc_03。
	r.cursor = 1

	got, err := r.Pick(context.Background(), "", "MiniMax-M3", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccountID != "acc_03" {
		t.Fatalf("picked %s, want the one the cursor points at", got.AccountID)
	}
}

// TestTheCreditsBreakTheTieBetweenAccountsThatWereUsedTheSame says that
// when two accounts were used the same, the one with more credits wins.
//
// 余额现在是最后一条排队的规矩，不是第一条：先比最久没用过的，用得一样
// 久的时候再看谁剩得多（规格 §6.5）。
func TestTheCreditsBreakTheTieBetweenAccountsThatWereUsedTheSame(t *testing.T) {
	p := newFakePool(acc("acc_01"), acc("acc_02"))
	p.remaining = map[string]float64{"acc_01": 10, "acc_02": 500}
	r := New(p, Options{MaxAttempts: 3})
	got, err := r.Pick(context.Background(), "sess-1", "MiniMax-M3", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccountID != "acc_02" {
		t.Fatalf("picked %s, want the one with credits", got.AccountID)
	}
}

func TestTheCreditsAreAskedForTheWholeListAtOnce(t *testing.T) {
	p := &batchPool{fakePool: newFakePool(acc("acc_01"), acc("acc_02"), acc("acc_03"))}
	p.remaining = map[string]float64{"acc_02": 500}
	r := New(p, Options{MaxAttempts: 3})
	got, err := r.Pick(context.Background(), "sess-1", "MiniMax-M3", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccountID != "acc_02" {
		t.Fatalf("picked %s", got.AccountID)
	}
	if p.calls != 1 {
		t.Fatalf("the pool was asked %d times, want 1", p.calls)
	}
	if len(p.ids) != 3 {
		t.Fatalf("the pool was asked about %v", p.ids)
	}
}

func TestTheCreditsAreAskedForOneAccountAtATimeWithoutTheBatch(t *testing.T) {
	p := newFakePool(acc("acc_01"), acc("acc_02"))
	r := New(p, Options{MaxAttempts: 3})
	if _, err := r.Pick(context.Background(), "sess-1", "MiniMax-M3", nil); err != nil {
		t.Fatal(err)
	}
	if p.remainingCalls != 2 {
		t.Fatalf("the pool was asked %d times, want 2", p.remainingCalls)
	}
}

func TestWindowKey(t *testing.T) {
	r := New(newFakePool(), Options{})
	if got := r.WindowKey("MiniMax-M3"); got != "general" {
		t.Fatalf("window = %q", got)
	}
	if got := r.WindowKey("MiniMax-Video-01"); got != "video" {
		t.Fatalf("window = %q", got)
	}
}

func TestGroupKey(t *testing.T) {
	// 键是计划窗口的那一组，不是一个窗口
	r := New(newFakePool(), Options{})
	if got := r.GroupKey("MiniMax-M3"); got != "general" {
		t.Fatalf("group = %q", got)
	}
	if got := r.GroupKey("MiniMax-Video-01"); got != "video" {
		t.Fatalf("group = %q", got)
	}
}

func TestTheExhaustionIsPerGroup(t *testing.T) {
	// general 这一组用完了，别的组不受影响
	p := newFakePool(acc("acc_01"), acc("acc_02"))
	p.exhausted = []store.Exhausted{{
		AccountID: "acc_01", WindowKey: "general",
		Until: time.Now().Add(time.Hour), Reason: "quota",
	}}
	r := New(p, Options{MaxAttempts: 3})

	general, err := r.Pick(context.Background(), "sess-1", "MiniMax-M3", nil)
	if err != nil {
		t.Fatal(err)
	}
	if general.AccountID != "acc_02" {
		t.Fatalf("picked %s", general.AccountID)
	}
	video, err := r.Pick(context.Background(), "sess-2", "MiniMax-Video-01", nil)
	if err != nil {
		t.Fatal(err)
	}
	if video.AccountID != "acc_01" {
		t.Fatalf("picked %s", video.AccountID)
	}
}

func TestMarkExhaustedReachesThePool(t *testing.T) {
	p := newFakePool(acc("acc_01"))
	r := New(p, Options{MaxAttempts: 3, Cooldown: time.Minute})
	until := time.Now().Add(time.Hour).Truncate(time.Second)

	if err := r.MarkExhausted(context.Background(), "acc_01", "general", "quota", until); err != nil {
		t.Fatal(err)
	}
	if len(p.marked) != 1 {
		t.Fatalf("marked %v", p.marked)
	}
	m := p.marked[0]
	if m.accountID != "acc_01" {
		t.Fatalf("account = %q", m.accountID)
	}
	if m.windowKey != "general" {
		t.Fatalf("window = %q", m.windowKey)
	}
	if m.reason != "quota" {
		t.Fatalf("reason = %q", m.reason)
	}
	if !m.until.Equal(until) {
		t.Fatalf("until = %v, want %v", m.until, until)
	}
}

func TestMarkWindowExhaustedHasThePoolOrder(t *testing.T) {
	p := newFakePool(acc("acc_01"))
	r := New(p, Options{MaxAttempts: 3, Cooldown: time.Minute})
	until := time.Now().Add(time.Hour).Truncate(time.Second)

	if err := r.MarkWindowExhausted(context.Background(), "acc_01", "general", until, "quota"); err != nil {
		t.Fatal(err)
	}
	if len(p.marked) != 1 {
		t.Fatalf("marked %v", p.marked)
	}
	m := p.marked[0]
	if m.accountID != "acc_01" || m.windowKey != "general" {
		t.Fatalf("marked %+v", m)
	}
	if m.reason != "quota" {
		t.Fatalf("reason = %q", m.reason)
	}
	if !m.until.Equal(until) {
		t.Fatalf("until = %v, want %v", m.until, until)
	}
}

func TestMarkExhaustedUsesTheCooldownWhenThereIsNoTime(t *testing.T) {
	p := newFakePool(acc("acc_01"))
	r := New(p, Options{MaxAttempts: 3, Cooldown: 90 * time.Second})
	if err := r.MarkWindowExhausted(context.Background(), "acc_01", "general", time.Time{}, "quota"); err != nil {
		t.Fatal(err)
	}
	if len(p.marked) != 1 {
		t.Fatalf("marked %v", p.marked)
	}
	until := p.marked[0].until
	if until.Before(time.Now().Add(time.Minute)) {
		t.Fatalf("until = %v, want the cooldown", until)
	}
}

func TestTheAttemptsAreCounted(t *testing.T) {
	p := newFakePool(acc("acc_01"), acc("acc_02"), acc("acc_03"))
	r := New(p, Options{MaxAttempts: 2})

	used := map[string]bool{"acc_01": true}
	if _, err := r.Pick(context.Background(), "", "MiniMax-M3", used); err != nil {
		t.Fatalf("one account tried, one attempt left: %v", err)
	}
	used["acc_02"] = true
	if _, err := r.Pick(context.Background(), "", "MiniMax-M3", used); err == nil {
		t.Fatal("expected an error: the budget is used up")
	}
}

func TestTheStrategyIsChecked(t *testing.T) {
	if err := (Options{Strategy: StrategySticky}).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (Options{Strategy: StrategyRoundRobin}).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (Options{Strategy: "sticky-ish"}).Validate(); err == nil {
		t.Fatal("expected an error")
	}
	if _, err := NewChecked(newFakePool(), Options{Strategy: "sticky-ish"}); err == nil {
		t.Fatal("expected an error")
	}
	// New 不会因为策略写错就失败：它退回粘性的那个
	if r := New(newFakePool(), Options{Strategy: "sticky-ish"}); r == nil {
		t.Fatal("no rotator")
	}
}

func TestSessionFromHeaders(t *testing.T) {
	h := map[string][]string{
		"X-Session-Id":      {"abc"},
		"X-Conversation-Id": {"def"},
	}
	s := SessionOf(h, "sys", "hello")
	if s.UpstreamID != "abc" {
		t.Fatalf("upstream id = %q", s.UpstreamID)
	}
	h2 := map[string][]string{"X-Conversation-Id": {"def"}}
	if got := SessionOf(h2, "sys", "hello"); got.UpstreamID != "def" {
		t.Fatalf("upstream id = %q", got.UpstreamID)
	}
}

func TestSessionKeyIsHashed(t *testing.T) {
	// 客户端自己那串字符串不进数据库键
	h := map[string][]string{"X-Session-Id": {"abc"}}
	s := SessionOf(h, "sys", "hello")
	if s.Key == "" {
		t.Fatal("no key")
	}
	if strings.Contains(s.Key, "abc") {
		t.Fatalf("key = %q, it holds the client's own string", s.Key)
	}
	if SessionOf(h, "sys", "hello").Key != s.Key {
		t.Fatal("the same conversation gave two keys")
	}
	other := SessionOf(map[string][]string{"X-Session-Id": {"def"}}, "sys", "hello")
	if other.Key == s.Key {
		t.Fatal("two conversations gave the same key")
	}
}

func TestSessionFromHash(t *testing.T) {
	a := SessionOf(nil, "sys", "hello")
	b := SessionOf(nil, "sys", "hello")
	if a.Key == "" {
		t.Fatal("expected a key")
	}
	if a.Key != b.Key {
		t.Fatal("the same conversation gave two keys")
	}
	c := SessionOf(nil, "sys", "other")
	if a.Key == c.Key {
		t.Fatal("two conversations gave the same key")
	}
	if a.UpstreamID == "" {
		t.Fatal("expected an upstream session id")
	}
}

func TestTwoIdenticalFirstMessagesShareTheUpstreamID(t *testing.T) {
	// 这是已知的碰撞，而且是定义上的碰撞：会话键就是 system prompt 加
	// 第一条 user 消息的哈希，两段一样的开场白是同一个会话。数据库里
	// 存着的上游 id 也是按会话键给的，所以这两个会话共用一个上游会话，
	// 上游分不出是两个会话。服务器分不出来：请求里没有能分开它们的东西，
	// 要分开只能让客户端带上 X-Session-Id。
	a := SessionOf(nil, "sys", "hello")
	b := SessionOf(nil, "sys", "hello")
	if a.UpstreamID != b.UpstreamID {
		t.Fatal("the two conversations got different upstream ids")
	}
}

func TestSessionEmpty(t *testing.T) {
	s := SessionOf(nil, "", "")
	if s.Key != "" || s.UpstreamID != "" {
		t.Fatalf("session = %+v", s)
	}
}

// idPool is a pool that keeps an upstream id for every session.
//
// 有上游 id 的池子：id 是它自己给的，不是请求算出来的。
type idPool struct {
	*fakePool
	ids map[string]string
	// asked holds the keys the pool was asked for.
	asked []string
	// err is what UpstreamID fails with.
	err error
}

func (p *idPool) UpstreamID(_ context.Context, key string) (string, error) {
	p.asked = append(p.asked, key)
	if p.err != nil {
		return "", p.err
	}
	if id, ok := p.ids[key]; ok {
		return id, nil
	}
	id := "up-" + key
	p.ids[key] = id
	return id, nil
}

func newIDPool(accounts ...store.Account) *idPool {
	return &idPool{fakePool: newFakePool(accounts...), ids: map[string]string{}}
}

// TestTheUpstreamIDComesFromThePool says the id the upstream is told
// about is the one the pool keeps.
//
// 上游看到的是数据库里存着的那个 id，不是请求算出来的那个。
func TestTheUpstreamIDComesFromThePool(t *testing.T) {
	p := newIDPool(acc("acc_01"))
	r := New(p, Options{MaxAttempts: 3})
	sess := SessionOf(map[string][]string{"X-Session-Id": {"client-1"}}, "sys", "hello")

	got, err := r.UpstreamID(context.Background(), sess)
	if err != nil {
		t.Fatal(err)
	}
	if got != "up-"+sess.Key {
		t.Fatalf("upstream id = %q", got)
	}
	if len(p.asked) != 1 || p.asked[0] != sess.Key {
		t.Fatalf("the pool was asked %v", p.asked)
	}
}

// TestTheUpstreamIDFallsBackToTheDerivedOne says a pool that keeps no
// id leaves the derived one in place.
//
// 没有上游 id 的池子：退回请求自己算出来的那个，规则和从前一样。
func TestTheUpstreamIDFallsBackToTheDerivedOne(t *testing.T) {
	// fakePool 没有 UpstreamID 方法：它就是那种给不出 id 的池子。
	p := newFakePool(acc("acc_01"))
	r := New(p, Options{MaxAttempts: 3})

	// 客户端带了会话 id：退回的就是客户端自己那串。
	withHeader := SessionOf(map[string][]string{"X-Session-Id": {"client-1"}}, "sys", "hello")
	got, err := r.UpstreamID(context.Background(), withHeader)
	if err != nil {
		t.Fatal(err)
	}
	if got != "client-1" {
		t.Fatalf("upstream id = %q, want the client's own", got)
	}

	// 客户端没带：退回的是第一条消息的哈希。
	fromMessage := SessionOf(nil, "sys", "hello")
	got, err = r.UpstreamID(context.Background(), fromMessage)
	if err != nil {
		t.Fatal(err)
	}
	if got == "" || got != fromMessage.UpstreamID {
		t.Fatalf("upstream id = %q, want the hash of the first message", got)
	}
}

// TestTwoSessionsGetDifferentUpstreamIDs says two conversations do not
// share the id the upstream is told about.
//
// 两个会话拿到两个不同的上游 id。
func TestTwoSessionsGetDifferentUpstreamIDs(t *testing.T) {
	p := newIDPool(acc("acc_01"))
	r := New(p, Options{MaxAttempts: 3})

	a, err := r.UpstreamID(context.Background(),
		SessionOf(map[string][]string{"X-Session-Id": {"a"}}, "", ""))
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.UpstreamID(context.Background(),
		SessionOf(map[string][]string{"X-Session-Id": {"b"}}, "", ""))
	if err != nil {
		t.Fatal(err)
	}
	if a == "" || b == "" {
		t.Fatalf("upstream ids = %q and %q", a, b)
	}
	if a == b {
		t.Fatalf("the two sessions got the same upstream id %q", a)
	}
}

// TestASessionWithNoKeyHasNoUpstreamID says a request with no
// conversation at all names nothing.
//
// 没有会话就没有对话要命名：池子也不问。
func TestASessionWithNoKeyHasNoUpstreamID(t *testing.T) {
	p := newIDPool(acc("acc_01"))
	r := New(p, Options{MaxAttempts: 3})

	got, err := r.UpstreamID(context.Background(), SessionOf(nil, "", ""))
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("upstream id = %q", got)
	}
	if len(p.asked) != 0 {
		t.Fatalf("the pool was asked %v", p.asked)
	}
}

// TestTheUpstreamIDFailsWhenThePoolCannotBeAsked says a pool that has
// the id but cannot read it is an error.
//
// 读不到存着的 id 就不能装作没有：上游会把这段对话当成新的一段。
func TestTheUpstreamIDFailsWhenThePoolCannotBeAsked(t *testing.T) {
	p := newIDPool(acc("acc_01"))
	p.err = errors.New("the database is down")
	r := New(p, Options{MaxAttempts: 3})

	sess := SessionOf(map[string][]string{"X-Session-Id": {"client-1"}}, "sys", "hello")
	if _, err := r.UpstreamID(context.Background(), sess); err == nil {
		t.Fatal("expected an error")
	}
}

func TestDecide(t *testing.T) {
	cases := []struct {
		name string
		f    Failure
		want Action
	}{
		{"配额拒绝", Failure{Status: 429, Body: []byte(`{"message":"insufficient balance"}`)}, ActionFailover},
		{"403 配额", Failure{Status: 403, Body: []byte(`{"message":"额度不足"}`)}, ActionFailover},
		{"402 配额", Failure{Status: 402, Body: []byte(`{"message":"quota exceeded"}`)}, ActionFailover},
		{"429 限流", Failure{Status: 429, Body: []byte(`{"message":"too many requests"}`)}, ActionRetrySame},
		{"500", Failure{Status: 500}, ActionRetrySame},
		{"网络错误", Failure{Network: true}, ActionRetrySame},
		{"跨站重定向", Failure{OtherSite: true}, ActionGiveUp},
		{"401", Failure{Status: 401}, ActionRenew},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Decide(c.f); got != c.want {
				t.Fatalf("Decide = %v, want %v", got, c.want)
			}
		})
	}
}

func TestDecideRenewsOnARefusedSignIn(t *testing.T) {
	cases := []struct {
		name string
		f    Failure
	}{
		{"调用方说的", Failure{Status: 200, Refused: true}},
		{"200 里的 1000048", Failure{Status: 200, Body: []byte(`{"statusInfo":{"code":1000048,"message":"the sign-in was refused"}}`)}},
		{"base_resp 里的 1000048", Failure{Status: 200, Body: []byte(`{"base_resp":{"status_code":1000048,"status_msg":"the sign-in was refused"}}`)}},
		{"403 里的 1000048", Failure{Status: 403, Body: []byte(`{"statusInfo":{"code":1000048,"message":"the sign-in was refused"}}`)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Decide(c.f); got != ActionRenew {
				t.Fatalf("Decide = %v, want %v", got, ActionRenew)
			}
		})
	}
}

func TestDecideIgnoresTheQuotaWordsOutsideTheMessage(t *testing.T) {
	// 身体里有 "credit" 和 "limit"，但那不是上游说的拒绝，
	// 不能因此把这个账号的窗口记成用完。
	f := Failure{Status: 429, Body: []byte(`{"message":"the request failed","detail":{"credit":5,"limit":100}}`)}
	if got := Decide(f); got != ActionRetrySame {
		t.Fatalf("Decide = %v, want %v", got, ActionRetrySame)
	}
}

func TestDecideReadsTheQuotaWordsFromTheNestedMessage(t *testing.T) {
	f := Failure{Status: 429, Body: []byte(`{"error":{"message":"insufficient balance"}}`)}
	if got := Decide(f); got != ActionFailover {
		t.Fatalf("Decide = %v, want %v", got, ActionFailover)
	}
}

func TestDecideReadsTheQuotaWordsFromAPlainErrorField(t *testing.T) {
	// 站点也会这样答：error 是一个字符串，不是对象。那句话就是
	// 上游说的拒绝，配额的话要认。
	f := Failure{Status: 403, Body: []byte(`{"error":"insufficient_balance"}`)}
	if got := Decide(f); got != ActionFailover {
		t.Fatalf("Decide = %v, want %v", got, ActionFailover)
	}
}

func TestDecideUsesTheMessageTheCallerParsed(t *testing.T) {
	f := Failure{Status: 429, Message: "insufficient balance"}
	if got := Decide(f); got != ActionFailover {
		t.Fatalf("Decide = %v, want %v", got, ActionFailover)
	}
}

// TestDecideGivesUpOnAForbiddenThatIsNotAboutTheCredits says a 403
// that does not name the credits is the site's own no.
//
// 同一个账号、同一个令牌再问一次，问出来的还是这个答案。参考实现
// 是原样交给客户端的。
func TestDecideGivesUpOnAForbiddenThatIsNotAboutTheCredits(t *testing.T) {
	f := Failure{Status: 403, Body: []byte(`{"message":"forbidden"}`)}
	if got := Decide(f); got != ActionGiveUp {
		t.Fatalf("Decide = %v, want %v", got, ActionGiveUp)
	}
}

// TestDecideRetriesTheSameAccountOnARateLimit says a 429 that does not
// name the credits is a rate limit: wait, then ask the same account.
func TestDecideRetriesTheSameAccountOnARateLimit(t *testing.T) {
	f := Failure{Status: 429, Body: []byte(`{"message":"too many requests"}`)}
	if got := Decide(f); got != ActionRetrySame {
		t.Fatalf("Decide = %v, want %v", got, ActionRetrySame)
	}
}

// TestTheUpstreamMessageIsReadTheSameWay says the message the client
// is given is the message the decision was made on.
func TestTheUpstreamMessageIsReadTheSameWay(t *testing.T) {
	cases := []struct {
		body string
		want string
	}{
		{`{"message":"insufficient balance"}`, "insufficient balance"},
		{`{"error":{"message":"insufficient balance"}}`, "insufficient balance"},
		{`{"error":"insufficient_balance"}`, "insufficient_balance"},
		{`{"base_resp":{"status_code":1004,"status_msg":"insufficient balance"}}`, "insufficient balance"},
		{`{"nothing":"here"}`, ""},
	}
	for _, c := range cases {
		if got := UpstreamMessage([]byte(c.body)); got != c.want {
			t.Errorf("UpstreamMessage(%s) = %q, want %q", c.body, got, c.want)
		}
	}
}

// TestTheLeastUsedAccountComesFirst says the accounts are used evenly.
//
// 挑号的第一条是「最久没用过的排前面」，不是「余额最多的排前面」：一个号
// 被别的号挤在后面一直闲着，到头来被一口气烧光（规格 §6.5）。
func TestTheLeastUsedAccountComesFirst(t *testing.T) {
	p := newFakePool(acc("acc_01"), acc("acc_02"))
	p.remaining = map[string]float64{"acc_01": 400, "acc_02": 100}
	r := New(p, Options{MaxAttempts: 3})

	// 第一个号两个都没用过过，按余额，余额多的先。
	plan, err := r.Pick(context.Background(), "talk-1", "chat", nil)
	if err != nil {
		t.Fatal(err)
	}
	if plan.AccountID != "acc_01" {
		t.Fatalf("account = %s, want acc_01", plan.AccountID)
	}

	// 第二个号：acc_01 刚用过，acc_02 没用过。用最久没用的那个。
	plan, err = r.Pick(context.Background(), "talk-2", "chat", nil)
	if err != nil {
		t.Fatal(err)
	}
	if plan.AccountID != "acc_02" {
		t.Fatalf("account = %s, want acc_02", plan.AccountID)
	}
}

// TestAnAccountThatIsEmptyComesLast says an account that was read and
// found empty is not picked while another account has credits.
//
// 读到 0 就是知道它空了：挑它就是撞一次空，撞空要一个上游请求，还占着
// 并发。
func TestAnAccountThatIsEmptyComesLast(t *testing.T) {
	p := newFakePool(acc("acc_01"), acc("acc_02"))
	p.remaining = map[string]float64{"acc_01": 0, "acc_02": 400}
	r := New(p, Options{MaxAttempts: 3})

	// 先用有额度的那个。
	if _, err := r.Pick(context.Background(), "talk-1", "chat", nil); err != nil {
		t.Fatal(err)
	}
	// acc_01 从没用过（它最久没用过），但它读到的是 0：它接不了活了。
	plan, err := r.Pick(context.Background(), "talk-2", "chat", nil)
	if err != nil {
		t.Fatal(err)
	}
	if plan.AccountID != "acc_02" {
		t.Fatalf("account = %s, want acc_02", plan.AccountID)
	}
}

// TestTheSameAccountsAlwaysGiveTheSameOne says the choice is stable.
//
// 一样的情况挑出同一个号来：随机的挑法让同一个号被反复烧，缓存命中也
// 说不准。
func TestTheSameAccountsAlwaysGiveTheSameOne(t *testing.T) {
	p := newFakePool(acc("acc_02"), acc("acc_01"))
	p.remaining = map[string]float64{"acc_01": 100, "acc_02": 100}
	r := New(p, Options{MaxAttempts: 3})

	plan, err := r.Pick(context.Background(), "talk-1", "chat", nil)
	if err != nil {
		t.Fatal(err)
	}
	if plan.AccountID != "acc_01" {
		t.Fatalf("account = %s, want acc_01", plan.AccountID)
	}
}
