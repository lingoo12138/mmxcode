package gateway

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"mmxcode/internal/pool"
	"mmxcode/internal/store"
)

// readCountingStore counts the reads of the database.
//
// 它数的是三样：账号、耗尽标记、余额快照 —— 池空的时候适配器问的就是这
// 三样，每一样都是全表读。
type readCountingStore struct {
	*store.Store
	accounts  int
	exhausted int
	snapshots int
	// fail says the database stopped answering.
	fail bool
}

func (c *readCountingStore) ListAccounts(ctx context.Context) ([]store.Account, error) {
	c.accounts++
	if c.fail {
		return nil, errors.New("the database did not answer")
	}
	return c.Store.ListAccounts(ctx)
}

func (c *readCountingStore) ListExhausted(ctx context.Context) ([]store.Exhausted, error) {
	c.exhausted++
	return c.Store.ListExhausted(ctx)
}

func (c *readCountingStore) LatestUsageSnapshots(ctx context.Context) ([]store.UsageSnapshot, error) {
	c.snapshots++
	return c.Store.LatestUsageSnapshots(ctx)
}

// oneAccount makes a store with one signed-in account.
func oneAccount(t *testing.T) (*store.Store, *readCountingStore) {
	t.Helper()
	s := openGatewayStore(t)
	now := time.Now()
	if err := s.CreateAccount(context.Background(), store.Account{
		ID: "acc_01", Name: "a", Status: store.StatusActive, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	return s, &readCountingStore{Store: s}
}

// TestTheEmptyPoolReadsTheDatabaseOnce says the database is read once
// for a batch of questions while the pool is empty.
//
// 池空的时候适配器回到从前的样子，从库里拿账号、标记和余额。那三样都是
// 全表读：一个请求一次就是 2.6MB 的垃圾，几十个并发就是一波几百 MB。
// 缓存一秒，那一波只读一次。
func TestTheEmptyPoolReadsTheDatabaseOnce(t *testing.T) {
	_, cs := oneAccount(t)
	ctx := context.Background()
	ad := poolAdapter{s: cs, fallback: newDBFallback(cs, time.Minute)}

	for i := 0; i < 3; i++ {
		if _, err := ad.Accounts(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := ad.Exhausted(ctx); err != nil {
			t.Fatal(err)
		}
		ad.RemainingAll(ctx, []string{"acc_01"})
	}

	if cs.accounts != 1 {
		t.Fatalf("the accounts were read %d times, want 1", cs.accounts)
	}
	if cs.exhausted != 1 {
		t.Fatalf("the exhausted marks were read %d times, want 1", cs.exhausted)
	}
	if cs.snapshots != 1 {
		t.Fatalf("the snapshots were read %d times, want 1", cs.snapshots)
	}
}

// TestTheEmptyPoolReadsTheDatabaseAgainAfterTheWindow says the cached
// answer does not last forever.
//
// 缓存久了就 stale：刚被标记成「用完」的号还看得出能用。窗口一小，它
// 就重新读一次。
func TestTheEmptyPoolReadsTheDatabaseAgainAfterTheWindow(t *testing.T) {
	_, cs := oneAccount(t)
	ctx := context.Background()
	ad := poolAdapter{s: cs, fallback: newDBFallback(cs, time.Nanosecond)}

	if _, err := ad.Accounts(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	if _, err := ad.Accounts(ctx); err != nil {
		t.Fatal(err)
	}

	if cs.accounts != 2 {
		t.Fatalf("the accounts were read %d times, want 2", cs.accounts)
	}
}

// TestTheFilledPoolDoesNotReadTheDatabase says the pool answers and the
// database is left alone.
//
// 池里有号的时候，那三样都从池里拿 —— 这就是池存在的理由。
func TestTheFilledPoolDoesNotReadTheDatabase(t *testing.T) {
	_, cs := oneAccount(t)
	ctx := context.Background()
	p := pool.New(10)
	p.Admit(pool.Entry{Account: store.Account{ID: "acc_01", Status: store.StatusActive}})
	ad := poolAdapter{s: cs, pool: p, fallback: newDBFallback(cs, time.Minute)}

	accounts, err := ad.Accounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 || accounts[0].ID != "acc_01" {
		t.Fatalf("accounts = %v, want just acc_01", accounts)
	}
	if _, err := ad.Exhausted(ctx); err != nil {
		t.Fatal(err)
	}
	if cs.accounts != 0 || cs.exhausted != 0 || cs.snapshots != 0 {
		t.Fatalf("the database was read: %d accounts, %d marks, %d snapshots",
			cs.accounts, cs.exhausted, cs.snapshots)
	}
}

// TestTheFailingDatabaseIsNotReadAgainBeforeTheWindow says a database
// that does not answer is not asked again before the window is over.
//
// 库从来没答过的时候（手上没有快照），后来的每一个请求都会去读一遍整张
// 表：库在挂的时候，读的次数是跟着来的请求走的，不是跟着那一秒走的。
// 记住上一次的那个错，这一秒里来的请求就都拿那个错走。
func TestTheFailingDatabaseIsNotReadAgainBeforeTheWindow(t *testing.T) {
	_, cs := oneAccount(t)
	cs.fail = true
	f := newDBFallback(cs, time.Minute)
	ctx := context.Background()

	if _, err := f.get(ctx); err == nil {
		t.Fatal("the database that does not answer was not reported")
	}
	if _, err := f.get(ctx); err == nil {
		t.Fatal("the database that does not answer was not reported the second time")
	}
	if cs.accounts != 1 {
		t.Fatalf("the accounts were read %d times, want 1", cs.accounts)
	}
}

// ctxStore answers only while the context is alive.
//
// 它站在读库的那一头：客户端走了的时候，读就该跟着断 —— 库没坏，是那
// 一头不要答案了。
type ctxStore struct {
	*store.Store
	mu sync.Mutex
	// reads counts the reads it was asked for.
	reads int
}

func (c *ctxStore) ListAccounts(ctx context.Context) ([]store.Account, error) {
	c.mu.Lock()
	c.reads++
	c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return c.Store.ListAccounts(ctx)
}

// slowStore holds the first read until the context is done.
//
// 它是那种"读库里那一头慢"的库：第一个人的 ctx 断了，读就断在那儿。
type slowStore struct {
	*store.Store
	mu sync.Mutex
	// reads counts the reads it was asked for.
	reads int
	// started says the first read began.
	started chan struct{}
}

func (s *slowStore) ListAccounts(ctx context.Context) ([]store.Account, error) {
	s.mu.Lock()
	s.reads++
	first := s.reads == 1
	s.mu.Unlock()
	if first {
		close(s.started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.Store.ListAccounts(ctx)
}

// TestTheCancelledCallerDoesNotPoisonTheNextOne says a read that was
// cut short by the caller is not remembered as the database's answer.
//
// 客户端走了的时候，读到一半就断了 —— 那是它的事，不是库的事。这个错
// 不能被记住：记住了一秒，这一秒里来的每一个请求都拿到"context
// canceled"，一个客户端的断开就变成一片 503。下一个请求要自己读一遍。
func TestTheCancelledCallerDoesNotPoisonTheNextOne(t *testing.T) {
	s, _ := oneAccount(t)
	c := &ctxStore{Store: s}
	f := newDBFallback(c, time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.get(ctx); err == nil {
		t.Fatal("the cancelled read was not reported")
	}

	snap, err := f.get(context.Background())
	if err != nil {
		t.Fatalf("the next caller got the cancellation: %v", err)
	}
	if len(snap.accounts) != 1 || snap.accounts[0].ID != "acc_01" {
		t.Fatalf("accounts = %v, want just acc_01", snap.accounts)
	}
	if c.reads != 2 {
		t.Fatalf("the store was read %d times, want 2", c.reads)
	}
}

// TestTheWaiterReadsAgainWhenTheReaderWasCutShort says a waiter does
// not inherit the reader's cancellation.
//
// 一次只读一遍：等的人等的是那个信号。读的那一头被掐断了，等的人醒来
// 的时候手上什么都没有 —— 它得自己读一遍，而不是接着那个取消。
func TestTheWaiterReadsAgainWhenTheReaderWasCutShort(t *testing.T) {
	s, _ := oneAccount(t)
	c := &slowStore{Store: s, started: make(chan struct{})}
	f := newDBFallback(c, time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() {
		_, err := f.get(ctx)
		first <- err
	}()
	<-c.started
	// 读的那一头还在库里等：来第二个请求，它得等。
	second := make(chan error, 1)
	go func() {
		_, err := f.get(context.Background())
		second <- err
	}()
	cancel()

	if err := <-first; err == nil {
		t.Fatal("the cancelled read was not reported")
	}
	if err := <-second; err != nil {
		t.Fatalf("the waiter inherited the cancellation: %v", err)
	}
	if c.reads != 2 {
		t.Fatalf("the store was read %d times, want 2", c.reads)
	}
}

// panickingStore breaks on the first read and answers after that.
//
// 它站在读库的那一头：库崩了的时候，网关不能跟着崩，也不能从此吊死。
type panickingStore struct {
	poolStore
	mu sync.Mutex
	// broken says the store already broke once.
	broken bool
}

func (p *panickingStore) ListAccounts(ctx context.Context) ([]store.Account, error) {
	p.mu.Lock()
	broken := p.broken
	p.broken = true
	p.mu.Unlock()
	if !broken {
		panic("the store broke")
	}
	return p.poolStore.ListAccounts(ctx)
}

// TestTheFallbackSurvivesAPanicInTheRead says a read that panics does
// not wedge the fallback.
//
// 读库的那一头要是炸了，发出去的信号就没人收了：后来的每一个请求都吊
// 在一个不会有人关的通道上，等到自己的 ctx 死了才走 —— 兜底这一辈子就废
// 了（进程还在，net/http 会 recover handler 的 panic）。
func TestTheFallbackSurvivesAPanicInTheRead(t *testing.T) {
	_, cs := oneAccount(t)
	f := newDBFallback(&panickingStore{poolStore: cs}, time.Minute)

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Error("the panic did not come out")
			}
		}()
		_, _ = f.get(context.Background())
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	snap, err := f.get(ctx)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("the fallback did not come back: %v", err)
	}
	if elapsed > time.Second {
		t.Fatalf("the fallback took %v, want less than a second", elapsed)
	}
	if len(snap.accounts) != 1 {
		t.Fatalf("accounts = %v, want just acc_01", snap.accounts)
	}
}

// TestTheOldAnswerIsServedWhenTheDatabaseIsUnhappy says a failing read
// does not take the answer away.
//
// 库不答的时候，手上有旧的一份就拿它：挑出来的号顶多撞一次空，撞空的
// 代价是一个请求 —— 那比一片 503 强。
func TestTheOldAnswerIsServedWhenTheDatabaseIsUnhappy(t *testing.T) {
	_, cs := oneAccount(t)
	ctx := context.Background()
	ad := poolAdapter{s: cs, fallback: newDBFallback(cs, time.Nanosecond)}

	if _, err := ad.Accounts(ctx); err != nil {
		t.Fatal(err)
	}
	cs.fail = true

	accounts, err := ad.Accounts(ctx)
	if err != nil {
		t.Fatalf("the answer was taken away: %v", err)
	}
	if len(accounts) != 1 || accounts[0].ID != "acc_01" {
		t.Fatalf("accounts = %v, want just acc_01", accounts)
	}
}
