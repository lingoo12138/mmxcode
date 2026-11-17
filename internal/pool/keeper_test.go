package pool

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"mmxcode/internal/store"
)

// fakeSource is a set of candidates.
type fakeSource struct {
	mu sync.Mutex
	// all are the candidates it knows.
	all []store.Candidate
	// asked counts the questions it was asked.
	asked int
	// err is what it answers with, when it is set.
	err error
}

func (f *fakeSource) Candidates(_ context.Context, limit int, exclude map[string]bool) ([]store.Candidate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked++
	if f.err != nil {
		return nil, f.err
	}
	out := make([]store.Candidate, 0, limit)
	for _, c := range f.all {
		if exclude[c.Account.ID] {
			continue
		}
		out = append(out, c)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// fakeReader reads credits and counts what it was asked.
type fakeReader struct {
	mu sync.Mutex
	// left is how many credits each account has.
	left map[string]float64
	// err is what it answers with, when it is set.
	err error
	// fail are the accounts whose reading always fails.
	fail map[string]bool
	// asked counts the accounts it was asked about.
	asked int
}

func (f *fakeReader) RefreshUsage(_ context.Context, accountID string) (store.UsageSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked++
	if f.err != nil {
		return store.UsageSnapshot{}, f.err
	}
	if f.fail[accountID] {
		return store.UsageSnapshot{}, errors.New("the site did not answer")
	}
	left, ok := f.left[accountID]
	if !ok {
		return store.UsageSnapshot{AccountID: accountID}, nil
	}
	return store.UsageSnapshot{
		AccountID:  accountID,
		Granted:    400,
		Remaining:  left,
		HasAmounts: true,
	}, nil
}

// fakeMarker records the marks that were written.
type fakeMarker struct {
	mu sync.Mutex
	// marks are the marks that were written.
	marks []store.Exhausted
	// err is what it answers with, when it is set.
	err error
	// ctxErr is what the context said when the mark was written.
	ctxErr error
	// called counts the times it was called.
	called int
}

func (f *fakeMarker) SetExhausted(ctx context.Context, accountID, windowKey string, until time.Time, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.called++
	f.ctxErr = ctx.Err()
	if f.err != nil {
		return f.err
	}
	f.marks = append(f.marks, store.Exhausted{
		AccountID: accountID, WindowKey: windowKey, Until: until, Reason: reason,
	})
	return nil
}

// blockingReader reads the credits only when the reading is over.
//
// 它是卡住的那种读：上游不答，网关等的是它自己那一趟的时间到。
type blockingReader struct {
	mu sync.Mutex
	// asked counts the accounts it was asked about.
	asked int
}

func (b *blockingReader) RefreshUsage(ctx context.Context, accountID string) (store.UsageSnapshot, error) {
	b.mu.Lock()
	b.asked++
	b.mu.Unlock()
	<-ctx.Done()
	return store.UsageSnapshot{}, ctx.Err()
}

// spentOnTimeout reads the credits only when the pass is over.
//
// 它是那种"读回来了，但这一趟的时间已经用完了"的读：读到的数说这个号
// 没有额度，而写标记的时候这一趟的时限已经到了。
type spentOnTimeout struct{}

func (s *spentOnTimeout) RefreshUsage(ctx context.Context, accountID string) (store.UsageSnapshot, error) {
	<-ctx.Done()
	return store.UsageSnapshot{
		AccountID:  accountID,
		Granted:    400,
		Remaining:  0,
		HasAmounts: true,
	}, nil
}

// TestTheMarkThatCouldNotBeWrittenIsReported says a mark that could not
// be written is said out loud.
//
// 标记写不进去，那个号在库里还占着一个位置：下一趟补号还会挑它，再读一
// 遍（一次读是 2~11 个上游请求），每一趟都是这样，直到它自己好起来。
// 写不进去又没有人知道，就没有人管它。
func TestTheMarkThatCouldNotBeWrittenIsReported(t *testing.T) {
	p := New(10)
	src := &fakeSource{all: []store.Candidate{
		{Account: anAccount("a")},
		{Account: anAccount("b")},
	}}
	reader := &fakeReader{left: map[string]float64{"a": 0, "b": 400}}
	marker := &fakeMarker{err: errors.New("the database did not answer")}
	k := NewKeeper(p, src, reader, marker, KeeperOptions{
		Read: true, Cooldown: time.Minute, ReadBudget: 10,
	})

	n, err := k.Replenish(context.Background(), 2)
	if err == nil {
		t.Fatal("the mark that could not be written was not reported")
	}
	if !strings.Contains(err.Error(), "the database did not answer") {
		t.Fatalf("err = %v, want the failure of the mark", err)
	}
	// 一个号的标记写不进去，不该把整趟填池都放弃：后面的号照旧进池。
	if n != 1 {
		t.Fatalf("replenished = %d, want 1", n)
	}
	if _, ok := p.Get("b"); !ok {
		t.Fatalf("the pool holds %v, want b", p.Accounts())
	}
}

// TestTheMarkIsWrittenEvenWhenThePassIsOver says the mark is written
// even when the pass ran out of time.
//
// 标记是关于这个号的事实：它不能因为这一趟跑超时了就丢掉。丢了这个号还
// 在库里占着位置，下一趟补号还会挑它，一趟读一次（2~11 个上游请求），
// 一趟接一趟，永远。
func TestTheMarkIsWrittenEvenWhenThePassIsOver(t *testing.T) {
	p := New(10)
	src := &fakeSource{all: []store.Candidate{{Account: anAccount("a")}}}
	reader := &spentOnTimeout{}
	marker := &fakeMarker{}
	k := NewKeeper(p, src, reader, marker, KeeperOptions{
		Read: true, Cooldown: time.Minute, ReadBudget: 10, PassTimeout: 100 * time.Millisecond,
	})

	_, err := k.Replenish(context.Background(), 1)
	// 这一趟是被时限掐断的：它报出来的错只能是时限的错。
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the deadline of the pass", err)
	}
	if marker.called != 1 {
		t.Fatalf("the mark was written %d times, want 1", marker.called)
	}
	if marker.ctxErr != nil {
		t.Fatalf("the mark was written with a dead context: %v", marker.ctxErr)
	}
	if len(marker.marks) != 1 || marker.marks[0].AccountID != "a" {
		t.Fatalf("marks = %v, want just a", marker.marks)
	}
}

// TestTheReplenishStopsWhenThePassIsOver says one filling pass does not
// run past its own deadline.
//
// 补号那一趟每 30 秒该来一次（poolLoop）。一趟要是能跑 90 秒（6 个号 ×
// 每个 15 秒），中间那两趟都被"上一趟还在跑"挡掉了，补号的速度就悄悄
// 降下来了 —— 池短着，承诺的并发就少一个。
func TestTheReplenishStopsWhenThePassIsOver(t *testing.T) {
	p := New(10)
	src := &fakeSource{all: []store.Candidate{
		{Account: anAccount("a")},
		{Account: anAccount("b")},
		{Account: anAccount("c")},
		{Account: anAccount("d")},
		{Account: anAccount("e")},
	}}
	reader := &blockingReader{}
	marker := &fakeMarker{}
	k := NewKeeper(p, src, reader, marker, KeeperOptions{
		Read: true, Cooldown: time.Minute, ReadBudget: 10, PassTimeout: 100 * time.Millisecond,
	})
	// 这一趟的时间到了就收手：ctx 是网关自己的命，不是这一趟的。
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	start := time.Now()
	n, err := k.Replenish(ctx, 5)
	elapsed := time.Since(start)
	// 这一趟是被时限掐断的：它报出来的错就是那个时限的错。
	//
	// 以前这里报的是"没有错"：池短着，日志里却什么都没有 —— 看得见的
	// 只有"这一趟没填上"，看不见"这一趟没跑完"。两件事要分开说。
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the deadline of the pass", err)
	}
	if n != 0 {
		t.Fatalf("replenished = %d, want 0", n)
	}
	if elapsed > time.Second {
		t.Fatalf("the pass took %v, want less than a second", elapsed)
	}
	if reader.asked > 1 {
		t.Fatalf("the site was asked %d times, want at most 1", reader.asked)
	}
}

// TestTheSeedFillsThePoolWithoutAskingTheSite says the pool is filled
// at startup without asking the site for anything.
//
// 读额度是 2~11 个上游请求，填满 400 个号要半个多小时，而网关要立刻能
// 用（规格 §6.3）。旧数只是高估 —— 撞空时换号重试会把标记写准，自愈。
func TestTheSeedFillsThePoolWithoutAskingTheSite(t *testing.T) {
	p := New(10)
	src := &fakeSource{all: []store.Candidate{
		{Account: anAccount("a"), Remaining: 400, Known: true},
		{Account: anAccount("b"), Remaining: 400, Known: true},
	}}
	reader := &fakeReader{left: map[string]float64{"a": 400, "b": 400}}
	marker := &fakeMarker{}
	k := NewKeeper(p, src, reader, marker, KeeperOptions{Read: true, Cooldown: time.Minute})

	n, err := k.Seed(context.Background(), 2)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("seeded = %d, want 2", n)
	}
	if p.Len() != 2 {
		t.Fatalf("pool = %d, want 2", p.Len())
	}
	if reader.asked != 0 {
		t.Fatalf("the site was asked %d times, want 0", reader.asked)
	}
}

// TestTheReplenishReadsTheCreditsOfANewAccount says a new account's
// credits are read before it goes into the pool.
//
// 读到的数说它有额度，它才进池：读不出来的不进池（它没有数，挑号也挑不
// 到它）。
func TestTheReplenishReadsTheCreditsOfANewAccount(t *testing.T) {
	p := New(10)
	src := &fakeSource{all: []store.Candidate{{Account: anAccount("a")}}}
	reader := &fakeReader{left: map[string]float64{"a": 250}}
	marker := &fakeMarker{}
	k := NewKeeper(p, src, reader, marker, KeeperOptions{Read: true, Cooldown: time.Minute})

	n, err := k.Replenish(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("replenished = %d, want 1", n)
	}
	if reader.asked != 1 {
		t.Fatalf("the site was asked %d times, want 1", reader.asked)
	}
	e, ok := p.Get("a")
	if !ok {
		t.Fatal("the account did not go into the pool")
	}
	if !e.Known || e.Remaining != 250 {
		t.Fatalf("entry = %+v, want 250 known", e)
	}
}

// TestAnAccountThatReadsAsSpentIsMarkedAndLeftOut says an account that
// reads as spent does not go into the pool, and is marked.
//
// 它留在库里占着一个位置：下次补号不会再挑它（规格 §6.5）。
func TestAnAccountThatReadsAsSpentIsMarkedAndLeftOut(t *testing.T) {
	p := New(10)
	src := &fakeSource{all: []store.Candidate{{Account: anAccount("a")}}}
	reader := &fakeReader{left: map[string]float64{"a": 0}}
	marker := &fakeMarker{}
	k := NewKeeper(p, src, reader, marker, KeeperOptions{Read: true, Cooldown: time.Minute})

	n, err := k.Replenish(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("replenished = %d, want 0", n)
	}
	if p.Len() != 0 {
		t.Fatalf("pool = %d, want 0", p.Len())
	}
	if len(marker.marks) != 1 || marker.marks[0].AccountID != "a" {
		t.Fatalf("marks = %v, want just a", marker.marks)
	}
}

// TestAnAccountThatCannotBeReadDoesNotGoIntoThePool says an account
// whose credits cannot be read does not go into the pool.
//
// 不知道有没有额度，就别占着池里的位置：它的数说不了话，挑号也挑不到它。
func TestAnAccountThatCannotBeReadDoesNotGoIntoThePool(t *testing.T) {
	p := New(10)
	src := &fakeSource{all: []store.Candidate{{Account: anAccount("a")}}}
	reader := &fakeReader{err: errors.New("the site is not answering")}
	marker := &fakeMarker{}
	k := NewKeeper(p, src, reader, marker, KeeperOptions{Read: true, Cooldown: time.Minute})

	n, err := k.Replenish(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("replenished = %d, want 0", n)
	}
	if p.Len() != 0 {
		t.Fatalf("pool = %d, want 0", p.Len())
	}
	if len(marker.marks) != 0 {
		t.Fatalf("marks = %v, want none", marker.marks)
	}
}

// TestTheReplenishDoesNotRunTwiceAtOnce says two replenishments do not
// run at the same time.
//
// 一次只跑一趟：池里的号不会因为跑两趟就多出来，而上游的请求会翻倍。
func TestTheReplenishDoesNotRunTwiceAtOnce(t *testing.T) {
	p := New(10)
	src := &fakeSource{all: []store.Candidate{{Account: anAccount("a")}}}
	reader := &fakeReader{left: map[string]float64{"a": 400}}
	marker := &fakeMarker{}
	k := NewKeeper(p, src, reader, marker, KeeperOptions{Read: true, Cooldown: time.Minute})

	// The first one holds the keeper busy while the second one comes.
	k.mu.Lock()
	k.running = true
	k.mu.Unlock()
	n, err := k.Replenish(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("replenished = %d, want 0", n)
	}
	if reader.asked != 0 {
		t.Fatalf("the site was asked %d times, want 0", reader.asked)
	}
}

// TestTheReplenishStopsWhenThereIsNothingToTake says a pool that cannot
// be filled is not filled with rubbish.
func TestTheReplenishStopsWhenThereIsNothingToTake(t *testing.T) {
	p := New(10)
	src := &fakeSource{}
	reader := &fakeReader{left: map[string]float64{}}
	marker := &fakeMarker{}
	k := NewKeeper(p, src, reader, marker, KeeperOptions{Read: true, Cooldown: time.Minute})

	n, err := k.Replenish(context.Background(), 5)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("replenished = %d, want 0", n)
	}
	if p.Len() != 0 {
		t.Fatalf("pool = %d, want 0", p.Len())
	}
}

// TestTheReplenishReadsNoMoreThanTheBudget says one pass reads no more
// accounts than the budget allows.
//
// 补号读的是上游的钱：一次读是 2~11 个上游请求（规格 §6.4），一趟读几百
// 个号就是几千个请求 —— 那不是填池，是打上游。预算是那一趟的
// （usage.refresh.batch）：用完了这一趟就收手，剩下的号等下一趟。
func TestTheReplenishReadsNoMoreThanTheBudget(t *testing.T) {
	p := New(10)
	src := &fakeSource{all: []store.Candidate{
		{Account: anAccount("a")},
		{Account: anAccount("b")},
		{Account: anAccount("c")},
		{Account: anAccount("d")},
	}}
	reader := &fakeReader{left: map[string]float64{"a": 400, "b": 400, "c": 400, "d": 400}}
	marker := &fakeMarker{}
	k := NewKeeper(p, src, reader, marker, KeeperOptions{
		Read: true, Cooldown: time.Minute, ReadBudget: 2,
	})

	n, err := k.Replenish(context.Background(), 4)
	if err != nil {
		t.Fatal(err)
	}
	if reader.asked != 2 {
		t.Fatalf("the site was asked %d times, want 2", reader.asked)
	}
	if n != 2 {
		t.Fatalf("added = %d, want 2", n)
	}
	if p.Len() != 2 {
		t.Fatalf("pool = %d, want 2", p.Len())
	}
}

// TestTheAccountsThatFailedToBeReadAreNotReadAgain says an account whose
// reading failed is not read again in the same pass.
//
// 它刚才读不到，再读一次也是读不到，而预算是有限的：把预算花在同一个读
// 不到的号上，后面的号就没得读了。
func TestTheAccountsThatFailedToBeReadAreNotReadAgain(t *testing.T) {
	p := New(10)
	src := &fakeSource{all: []store.Candidate{
		{Account: anAccount("a")},
		{Account: anAccount("b")},
	}}
	reader := &fakeReader{
		left: map[string]float64{"b": 400},
		fail: map[string]bool{"a": true},
	}
	marker := &fakeMarker{}
	k := NewKeeper(p, src, reader, marker, KeeperOptions{
		Read: true, Cooldown: time.Minute, ReadBudget: 10,
	})

	n, err := k.Replenish(context.Background(), 2)
	if err != nil {
		t.Fatal(err)
	}
	if reader.asked != 2 {
		t.Fatalf("the site was asked %d times, want 2", reader.asked)
	}
	if n != 1 {
		t.Fatalf("added = %d, want 1", n)
	}
	if p.Len() != 1 {
		t.Fatalf("pool = %d, want 1", p.Len())
	}
}

// TestAnAccountThatReadsAsSpentIsLeftOutAndTheNextOneGoesIn says a
// spent account is marked and left out, and the next candidate goes in.
//
// 补号不能停在一个用完的号上：那个号出池，让位给一个冷的（规格 §4.4）。
func TestAnAccountThatReadsAsSpentIsLeftOutAndTheNextOneGoesIn(t *testing.T) {
	p := New(10)
	src := &fakeSource{all: []store.Candidate{
		{Account: anAccount("a")},
		{Account: anAccount("b")},
	}}
	reader := &fakeReader{left: map[string]float64{"a": 0, "b": 400}}
	marker := &fakeMarker{}
	k := NewKeeper(p, src, reader, marker, KeeperOptions{Read: true, Cooldown: time.Minute})

	n, err := k.Replenish(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("replenished = %d, want 1", n)
	}
	if p.Len() != 1 {
		t.Fatalf("pool = %d, want 1", p.Len())
	}
	entry, ok := p.Get("b")
	if !ok {
		t.Fatalf("the pool holds %v, want b", p.Accounts())
	}
	if entry.Account.ID != "b" {
		t.Fatalf("the pool holds %v, want b", p.Accounts())
	}
	if len(marker.marks) != 1 || marker.marks[0].AccountID != "a" {
		t.Fatalf("marks = %v, want just a", marker.marks)
	}
}
