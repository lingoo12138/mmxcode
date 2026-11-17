package gateway

import (
	"context"
	"testing"
	"time"

	"mmxcode/internal/pool"
	"mmxcode/internal/store"
)

// readCountingStore counts the reads of the database.
//
// 它数的是三样：账号、耗尽标记、余额快照 —— 池空的时候适配器问的就是这
// 三样，每一样都是全表读。
//
// 它数的是三样：账号、耗尽标记、余额快照 —— 池空的时候适配器问的就是这
// 三样，每一样都是全表读。
type readCountingStore struct {
	*store.Store
	accounts  int
	exhausted int
	snapshots int
}

func (c *readCountingStore) ListAccounts(ctx context.Context) ([]store.Account, error) {
	c.accounts++
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
