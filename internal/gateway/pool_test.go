package gateway

import (
	"context"
	"testing"
	"time"

	"mmxcode/internal/pool"
	"mmxcode/internal/store"
)

// openGatewayStore opens a store for a test.
func openGatewayStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(t.TempDir() + "/pool.db")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// listingStore counts the times the whole table was read.
//
// 它是包在真库外面的：里面的方法都是真的，只是多了两个计数器。
type listingStore struct {
	*store.Store
	// listed is how many times the whole table was read.
	listed int
}

func (c *listingStore) ListAccounts(ctx context.Context) ([]store.Account, error) {
	c.listed++
	return c.Store.ListAccounts(ctx)
}

func (c *listingStore) ListExhausted(ctx context.Context) ([]store.Exhausted, error) {
	c.listed++
	return c.Store.ListExhausted(ctx)
}

// TestThePickerReadsThePoolAndNotTheWholeTable says picking an account
// reads the pool, not the whole table.
//
// 现在每个聊天请求都把账号表和耗尽标记表读一遍（规格 §6.2）：1000 个
// 号就是 1000 行 × 每个请求。池把它变成一次内存里的查表。
func TestThePickerReadsThePoolAndNotTheWholeTable(t *testing.T) {
	s := openGatewayStore(t)
	ctx := context.Background()
	p := pool.New(10)
	p.Admit(pool.Entry{
		Account:   store.Account{ID: "acc_01", Status: store.StatusActive},
		Remaining: 400,
		Known:     true,
	})
	cs := &listingStore{Store: s}
	ad := poolAdapter{s: cs, pool: p}

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
	if cs.listed != 0 {
		t.Fatalf("the whole table was read %d times, want 0", cs.listed)
	}
}

// TestAnEmptyPoolFallsBackToTheDatabase says a gateway whose pool is
// empty still works.
//
// 池是空的（还没填上，或者填池失败）的时候，网关不能一个请求都答不了。
func TestAnEmptyPoolFallsBackToTheDatabase(t *testing.T) {
	s := openGatewayStore(t)
	ctx := context.Background()
	now := time.Now()
	if err := s.CreateAccount(ctx, store.Account{
		ID: "acc_01", Name: "a", Status: store.StatusActive, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	cs := &listingStore{Store: s}
	ad := poolAdapter{s: cs, pool: pool.New(10)}

	accounts, err := ad.Accounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) == 0 {
		t.Fatal("the database was not read")
	}
	if cs.listed == 0 {
		t.Fatal("the database was not read")
	}
}

// TestASpentAccountLeavesThePool says an account whose window is spent
// goes out of the pool.
//
// 它接不了活了，让位给一个冷的（规格 §4.4）。标记照旧写进库里。
func TestASpentAccountLeavesThePool(t *testing.T) {
	s := openGatewayStore(t)
	ctx := context.Background()
	now := time.Now()
	if err := s.CreateAccount(ctx, store.Account{
		ID: "acc_01", Name: "a", Status: store.StatusActive, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	p := pool.New(10)
	p.Admit(pool.Entry{Account: store.Account{ID: "acc_01", Status: store.StatusActive}})
	ad := poolAdapter{s: s, pool: p}

	until := time.Now().Add(time.Hour)
	if err := ad.MarkExhausted(ctx, "acc_01", "general", until, "quota"); err != nil {
		t.Fatal(err)
	}
	if p.Len() != 0 {
		t.Fatalf("pool = %d, want 0", p.Len())
	}
	marks, err := s.ListExhausted(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(marks) != 1 || marks[0].AccountID != "acc_01" {
		t.Fatalf("marks = %v, want just acc_01", marks)
	}
}
