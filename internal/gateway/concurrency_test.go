package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/lingoo12138/mmxcode/internal/config"
	"github.com/lingoo12138/mmxcode/internal/pool"
	"github.com/lingoo12138/mmxcode/internal/store"
)

// manyAccounts makes a store with n signed-in accounts.
//
// 号的名字是数出来的：acc_a0、acc_b0 …… acc_a1 —— 头一个永远是 acc_a0，
// 测试好往池里放它。
func manyAccounts(t *testing.T, n int) *store.Store {
	t.Helper()
	s := openGatewayStore(t)
	now := time.Now()
	for i := 0; i < n; i++ {
		if err := s.CreateAccount(context.Background(), store.Account{
			ID:     "acc_" + string(rune('a'+i%26)) + string(rune('0'+i/26)),
			Status: store.StatusActive, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

// TestTheConcurrencyIsNotMoreThanThePool says the gateway does not
// promise more requests than the pool can carry.
//
// 承诺了接不住就是一片 429（规格不变量 4）。
func TestTheConcurrencyIsNotMoreThanThePool(t *testing.T) {
	s := manyAccounts(t, 100)
	ctx := context.Background()
	cfg := config.Default()
	g := &Server{Store: s, Cfg: cfg, Pool: pool.New(10)}

	// 库里有很多能用的号，池里只有一个。
	g.Pool.Admit(pool.Entry{Account: store.Account{ID: "acc_a0", Status: store.StatusActive}})

	if got := g.totalConcurrency(ctx); got != 1 {
		t.Fatalf("concurrency = %d, want 1", got)
	}
}

// TestTheConcurrencyFallsBackToTheDatabaseWhenThePoolIsEmpty says a
// gateway whose pool is empty still promises what it used to.
func TestTheConcurrencyFallsBackToTheDatabaseWhenThePoolIsEmpty(t *testing.T) {
	s := manyAccounts(t, 10)
	ctx := context.Background()
	cfg := config.Default()
	g := &Server{Store: s, Cfg: cfg, Pool: pool.New(10)}

	if got := g.totalConcurrency(ctx); got != 3 {
		t.Fatalf("concurrency = %d, want 3", got)
	}
}

// TestTheConcurrencyDoesNotGoOverTheCeiling says the promised
// concurrency never goes over the ceiling.
//
// 天花板是「并发可控」的那根杆。池空的时候承诺从库里数：库里 100 个号就
// 承诺 20 个 —— 那一下能从池子撑的数抬到库里的数。天花板把它按在运营定
// 的那个数上。
func TestTheConcurrencyDoesNotGoOverTheCeiling(t *testing.T) {
	s := manyAccounts(t, 100)
	ctx := context.Background()
	cfg := config.Default()
	cfg.Concurrency.TotalMax = 5
	g := &Server{Store: s, Cfg: cfg, Pool: pool.New(10)}

	if got := g.totalConcurrency(ctx); got != 5 {
		t.Fatalf("concurrency = %d, want 5", got)
	}
}

// TestTheCeilingDoesNotRaiseThePromise says the ceiling only lowers it.
//
// 池里只有两个号，承诺五个是接不住的：承诺了接不住就是一片 429（规格
// 不变量 4）。
func TestTheCeilingDoesNotRaiseThePromise(t *testing.T) {
	s := manyAccounts(t, 100)
	ctx := context.Background()
	cfg := config.Default()
	cfg.Concurrency.TotalMax = 5
	g := &Server{Store: s, Cfg: cfg, Pool: pool.New(10)}
	g.Pool.Admit(pool.Entry{Account: store.Account{ID: "acc_a0", Status: store.StatusActive}})
	g.Pool.Admit(pool.Entry{Account: store.Account{ID: "acc_b0", Status: store.StatusActive}})

	if got := g.totalConcurrency(ctx); got != 2 {
		t.Fatalf("concurrency = %d, want 2", got)
	}
}

// TestWithoutACeilingThePromiseIsTheOldOne says nothing changes when no
// ceiling is set.
//
// 0 = 不限：跟从前一样，池空的时候从库里数。
func TestWithoutACeilingThePromiseIsTheOldOne(t *testing.T) {
	s := manyAccounts(t, 100)
	ctx := context.Background()
	cfg := config.Default()
	g := &Server{Store: s, Cfg: cfg, Pool: pool.New(10)}

	if got := g.totalConcurrency(ctx); got != 20 {
		t.Fatalf("concurrency = %d, want 20", got)
	}
}

// TestTheCeilingMakesThePoolSmaller says the pool is sized after the
// promise, and the promise is capped.
//
// 池多大 = max(总并发, 活跃会话数) × 倍数（规格 §6.3）。总并发被天花板
// 按住了，池就跟着小 —— 「n×2」那个说法才有着落（规格 §6.9）。
func TestTheCeilingMakesThePoolSmaller(t *testing.T) {
	s := manyAccounts(t, 100)
	ctx := context.Background()
	cfg := config.Default()
	cfg.Concurrency.TotalMax = 5
	g := &Server{Store: s, Cfg: cfg, Pool: pool.New(0)}

	// 天花板 5，倍数 2 → 池装 10 个号；没有天花板的时候是 20 × 2 = 40。
	if got := g.poolTarget(ctx); got != 10 {
		t.Fatalf("pool target = %d, want 10", got)
	}
}
