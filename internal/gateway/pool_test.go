package gateway

import (
	"context"
	"errors"
	"testing"
	"time"

	"mmxcode/internal/config"
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

// TestTheReadBudgetIsNeverUnlimited says the keeper's read budget is
// never "unlimited".
//
// keeper 那边的 0 说的是"不限"：补号读的是上游的钱，一次读是 2~11 个上游
// 请求（规格 §6.4），一趟读几百个号就是几千个请求 —— 那不是填池，是打上
// 游。而 usage.refresh.batch 配 0 说的是"一个一批"（usage.batch 的那
// 条规矩），不是"全都读"。所以这一层必须把它夹住。
func TestTheReadBudgetIsNeverUnlimited(t *testing.T) {
	for _, c := range []struct {
		batch int
		want  int
	}{
		{0, 1}, {-1, 1}, {1, 1}, {6, 6},
	} {
		cfg := config.Default()
		cfg.Usage.Refresh.Batch = c.batch
		if got := poolReadBudget(cfg.Usage); got != c.want {
			t.Errorf("the read budget for batch %d = %d, want %d", c.batch, got, c.want)
		}
	}
}

// TestTheKeeperOptionsNeverHaveAnUnlimitedReadBudget says the keeper the
// gateway builds never reads without a budget.
//
// keeper 那边的 0 说的是"不限"：补号读的是上游的钱，一次读是 2~11 个上游
// 请求（规格 §6.4），一趟读几百个号就是几千个请求 —— 那不是填池，是打上
// 游。而 usage.refresh.batch 配 0 说的是"一个一批"（usage.batch 的那
// 条规矩），不是"全都读"。所以这一层必须把它夹住。
//
// 夹的是 keeper 拿到的那份预算：算预算的函数对了，接线的那一行漏了夹子
// 也没用 —— 那个 keeper 拿到的就是"不限"。所以这里看的是 New() 用的那
// 份选项，不是那个函数。
func TestTheKeeperOptionsNeverHaveAnUnlimitedReadBudget(t *testing.T) {
	for _, c := range []struct {
		batch int
		want  int
	}{
		{0, 1}, {-1, 1}, {1, 1}, {6, 6},
	} {
		cfg := config.Default()
		cfg.Usage.Refresh.Batch = c.batch
		opts := keeperOptions(cfg)
		if opts.ReadBudget != c.want {
			t.Errorf("the read budget for batch %d = %d, want %d", c.batch, opts.ReadBudget, c.want)
		}
	}
}

// TestTheKeeperOptionsCarryTheConfiguration says the keeper the gateway
// builds gets the configuration it should.
//
// 别的几项也是配置说了算：读不读额度（pool.read_on_admit）、冷却多久
// （rotation.exhausted_cooldown）、一个号读多久（usage.refresh.timeout）、
// 一趟跑多久（usage.refresh.budget）。
func TestTheKeeperOptionsCarryTheConfiguration(t *testing.T) {
	cfg := config.Default()
	cfg.Usage.Refresh.Batch = 0
	opts := keeperOptions(cfg)

	if opts.Read != cfg.Pool.ReadOnAdmit {
		t.Errorf("read = %v, want %v", opts.Read, cfg.Pool.ReadOnAdmit)
	}
	if opts.Cooldown != cfg.Rotation.ExhaustedCooldown.Duration() {
		t.Errorf("cooldown = %v, want %v", opts.Cooldown, cfg.Rotation.ExhaustedCooldown.Duration())
	}
	if opts.ReadTimeout != cfg.Usage.Refresh.Timeout.Duration() {
		t.Errorf("read timeout = %v, want %v", opts.ReadTimeout, cfg.Usage.Refresh.Timeout.Duration())
	}
	if opts.PassTimeout != cfg.Usage.Refresh.Budget.Duration() {
		t.Errorf("pass timeout = %v, want %v", opts.PassTimeout, cfg.Usage.Refresh.Budget.Duration())
	}
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

// failingMarkStore does not write the marks.
type failingMarkStore struct {
	poolStore
}

func (f *failingMarkStore) SetExhausted(ctx context.Context, accountID, windowKey string, until time.Time, reason string) error {
	return errors.New("the database did not answer")
}

// TestASpentAccountLeavesThePoolEvenWhenTheMarkIsNotWritten says an
// account that is spent leaves the pool even when the mark could not be
// written.
//
// 标记写不进去的时候，那个号在池里留着：它带着一份旧的数（看着还有额
// 度），下一次就被挑中，每一次都是一次白花的请求，直到库缓过来。
//
// 池是缓存：把一个库里还觉得好的号丢出去是安全的（它以后还能再进来），
// 而把一个已经用完的号留在池里，每一次挑它都是一次白花的请求。标记的
// 真相在库里，池只是缓存。
func TestASpentAccountLeavesThePoolEvenWhenTheMarkIsNotWritten(t *testing.T) {
	s := openGatewayStore(t)
	ctx := context.Background()
	p := pool.New(10)
	p.Admit(pool.Entry{Account: store.Account{ID: "acc_01", Status: store.StatusActive}})
	ad := poolAdapter{s: &failingMarkStore{poolStore: s}, pool: p}

	err := ad.MarkExhausted(ctx, "acc_01", "general", time.Now().Add(time.Hour), "quota")
	if err == nil {
		t.Fatal("the mark that could not be written was not reported")
	}
	if p.Len() != 0 {
		t.Fatalf("pool = %d, want 0", p.Len())
	}
}

// TestADeadAccountIsReplacedAtOnce says the pool is filled again right
// after a dead account leaves it.
//
// 签不上的号出池以后，池里就少了一个 —— 而池短着的时候，承诺的并发就少
// 一个（不变量 4）。等下一趟补号（最多三十秒）才补上，这中间那一段，
// 网关白答应着做不到的事。
func TestADeadAccountIsReplacedAtOnce(t *testing.T) {
	s := openGatewayStore(t)
	ctx := context.Background()
	now := time.Now()
	// 一个签出的号，一个好的号。
	if err := s.CreateAccount(ctx, store.Account{
		ID: "acc_01", Name: "a", Status: store.StatusSignedOut, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateAccount(ctx, store.Account{
		ID: "acc_02", Name: "b", Status: store.StatusActive, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	p := pool.New(10)
	p.Admit(pool.Entry{Account: store.Account{ID: "acc_01", Status: store.StatusActive}})
	cfg := config.Default()
	// 补号的时候不读额度：读额度是问上游的，这个测试不碰上游。
	cfg.Pool.ReadOnAdmit = false
	lifeCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	keeper := pool.NewKeeper(p, poolSource{s: s, window: cfg.Pool.SessionWindow.Duration()},
		&fakeReader{}, s, pool.KeeperOptions{
			Read: false, Cooldown: time.Minute, ReadBudget: 1,
			ReadTimeout: time.Second, PassTimeout: 5 * time.Second,
		})
	g := &Server{
		Store:      s,
		Cfg:        cfg,
		Pool:       p,
		Keeper:     keeper,
		lifeCtx:    lifeCtx,
		lifeCancel: cancel,
		bgSem:      make(chan struct{}, 1),
	}

	g.dropDeadAccount(ctx, "acc_01")

	// 补号是后台的活：等它一会儿。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := p.Get("acc_02"); ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the pool holds %v, want acc_02", p.Accounts())
}

// TestASignedOutAccountLeavesThePool says an account that cannot sign
// in goes out of the pool.
//
// 池里留着一个签不上的号有两个坏处：它占着位置（承诺的并发按池夹），而
// 且挑号会挑到它 —— 每挑一次就是一次 401 加一次续签。
func TestASignedOutAccountLeavesThePool(t *testing.T) {
	s := openGatewayStore(t)
	ctx := context.Background()
	now := time.Now()
	// 库里说它签出了。
	if err := s.CreateAccount(ctx, store.Account{
		ID: "acc_01", Name: "a", Status: store.StatusSignedOut, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	p := pool.New(10)
	p.Admit(pool.Entry{Account: store.Account{ID: "acc_01", Status: store.StatusActive}})
	g := &Server{Store: s, Cfg: config.Default(), Pool: p}

	g.dropDeadAccount(ctx, "acc_01")

	if p.Len() != 0 {
		t.Fatalf("pool = %d, want 0", p.Len())
	}
}

// TestAnAccountThatIsStillActiveStaysInThePool says a failed sign-in
// that the database does not call permanent does not empty the pool.
//
// 续签抖了一下不是签出：那个号还能答，把它踢出去只是白白少一个能答的
// 号（承诺的并发按池夹）。
func TestAnAccountThatIsStillActiveStaysInThePool(t *testing.T) {
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
	g := &Server{Store: s, Cfg: config.Default(), Pool: p}

	g.dropDeadAccount(ctx, "acc_01")

	if p.Len() != 1 {
		t.Fatalf("pool = %d, want 1", p.Len())
	}
}
