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

	// sessionErr is what SessionAccount fails with.
	sessionErr error
	// bindErr is what BindSession fails with.
	bindErr error
	// remainingCalls counts the questions asked for one account's
	// credits.
	remainingCalls int
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

func (p *fakePool) SessionAccount(_ context.Context, key string) (string, bool, error) {
	if p.sessionErr != nil {
		return "", false, p.sessionErr
	}
	id, ok := p.sessions[key]
	return id, ok, nil
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

func acc(id string) store.Account {
	return store.Account{ID: id, Name: id, Status: store.StatusActive}
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

func TestPrefersTheAccountWithCredits(t *testing.T) {
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
	// 这是已知的碰撞：两个用户的第一条消息一样，就共用一个上游会话，
	// 上游分不出是两个会话。给每个会话一个随机的上游 id 需要 sessions
	// 表多一列，现在没有。
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

func TestDecideUsesTheMessageTheCallerParsed(t *testing.T) {
	f := Failure{Status: 429, Message: "insufficient balance"}
	if got := Decide(f); got != ActionFailover {
		t.Fatalf("Decide = %v, want %v", got, ActionFailover)
	}
}
