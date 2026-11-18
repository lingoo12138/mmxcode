package scheduler

import (
	"bytes"
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"mmxcode/internal/account"
	"mmxcode/internal/config"
	"mmxcode/internal/egress"
	"mmxcode/internal/manager"
	"mmxcode/internal/oauth"
	"mmxcode/internal/secret"
	"mmxcode/internal/store"
	"mmxcode/internal/upstream"

	_ "modernc.org/sqlite"
)

func TestCheckinDelayIsJittered(t *testing.T) {
	seen := map[string]time.Duration{}
	for i := 0; i < 40; i++ {
		d := CheckinDelay(0, 30*time.Minute)
		seen[d.String()] = d
	}
	if len(seen) < 5 {
		t.Fatalf("the delay did not vary: %v", seen)
	}
	for d := range seen {
		if d == "" {
			t.Fatal("empty delay")
		}
	}
}

func TestCheckinDelayFits(t *testing.T) {
	for i := 0; i < 100; i++ {
		d := CheckinDelay(0, 30*time.Minute)
		if d < 0 || d > 30*time.Minute {
			t.Fatalf("delay = %v", d)
		}
	}
}

// TestNextCheckin says the daily check-in runs at the site's time.
//
// 站点按大陆时间算一天，签到要落在那一天里（规格 §5.1）。
func TestNextCheckin(t *testing.T) {
	at, err := time.Parse("15:04", "09:00")
	if err != nil {
		t.Fatal(err)
	}
	// 大陆时间 2026-11-13 08:00（= UTC 2026-11-13 00:00）：今天 09:00
	// 还没到，是今天。
	now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	got := NextCheckin(now, at, 0)
	want := time.Date(2026, 11, 13, 1, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("next = %v, want %v", got, want)
	}
	// 大陆时间 10:00（= UTC 02:00）：今天 09:00 过了，是明天。
	now = time.Date(2026, 11, 13, 2, 0, 0, 0, time.UTC)
	got = NextCheckin(now, at, 0)
	want = time.Date(2026, 11, 14, 1, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("next = %v, want %v", got, want)
	}
}

// TestTheNextCheckinDoesNotDependOnTheMachinesZone says the daily
// check-in is at the site's midnight, whatever the machine's timezone is.
//
// 一台 UTC 的机器上「00:00」是大陆时间 08:00 —— 差 8 个钟头；一台东京
// 的机器上「00:00」是大陆时间前一天 23:00，签到落在站点的**前一天**，
// 那天的积分拿不到（规格 §5.1）。
func TestTheNextCheckinDoesNotDependOnTheMachinesZone(t *testing.T) {
	at, err := time.Parse("15:04", "00:00")
	if err != nil {
		t.Fatal(err)
	}
	// UTC 的 2026-11-18 20:00 是大陆时间的 11-19 04:00：下一个大陆时
	// 间的 00:00 是 11-20 的，也就是 UTC 的 11-19 16:00。
	now := time.Date(2026, 11, 18, 20, 0, 0, 0, time.UTC)
	got := NextCheckin(now, at, 0)
	want := time.Date(2026, 11, 19, 16, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("next = %v, want %v", got, want)
	}
}

func TestUsageRefresh(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/minimax-cloud/api/v1/credit/details":
			w.Write([]byte(`{"total_count":1,"details":[{"credit_type":1,"granted_amount":"10","consumed_amount":"0","remaining_amount":"10","granted_at_ms":1763000000000,"expire_at_ms":1900000000000}]}`))
		default:
			w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	ctx := context.Background()
	s, err := store.Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	s.Migrate(ctx)
	defer s.Close()
	now := time.Now()
	s.CreateAccount(ctx, store.Account{
		ID: "acc_01", Name: "a", Status: store.StatusActive, UserID: "1",
		CreatedAt: now, UpdatedAt: now,
	})
	s.SaveToken(ctx, store.Token{
		AccountID: "acc_01", Access: "tok", Refresh: "ref",
		ExpiresAt: now.Add(time.Hour), UpdatedAt: now,
	})
	up := upstream.New(upstream.Options{Timeout: 5 * time.Second})
	acc := account.New(up)
	// The account API is pointed at the test server. Without this the
	// test asks MiniMax for the numbers and waits for an answer that
	// never comes.
	acc.SetBase(srv.URL, srv.URL)
	mgr := manager.New(s, up, acc, egress.New(nil, ""), oauth.NewRenewer())

	sc := New(mgr, config.Default())
	sc.RefreshUsage(ctx)
	snaps, err := s.LatestUsageSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 1 || snaps[0].Granted != 10 {
		t.Fatalf("snaps = %+v", snaps)
	}
}

// TestTheScanEveryCanBeZero says the new key may be zero, and the
// interval goes back to the old key then.
//
// 新键写成 0 的意思是"间隔交回 schedule.usage_refresh"：那一趟按老的
// 间隔跑。写 0 以前配置文件都读不了，这一趟就根本没有"写 0"这回事 ——
// 文档许了，代码不许。
func TestTheScanEveryCanBeZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/minimax-cloud/api/v1/credit/details":
			w.Write([]byte(`{"total_count":1,"details":[{"credit_type":1,"granted_amount":"10","consumed_amount":"0","remaining_amount":"10","granted_at_ms":1763000000000,"expire_at_ms":1900000000000}]}`))
		default:
			w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	ctx := context.Background()
	s, err := store.Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	s.Migrate(ctx)
	defer s.Close()
	now := time.Now()
	s.CreateAccount(ctx, store.Account{
		ID: "acc_01", Name: "a", Status: store.StatusActive, UserID: "1",
		CreatedAt: now, UpdatedAt: now,
	})
	s.SaveToken(ctx, store.Token{
		AccountID: "acc_01", Access: "tok", Refresh: "ref",
		ExpiresAt: now.Add(time.Hour), UpdatedAt: now,
	})
	up := upstream.New(upstream.Options{Timeout: 5 * time.Second})
	acc := account.New(up)
	acc.SetBase(srv.URL, srv.URL)
	mgr := manager.New(s, up, acc, egress.New(nil, ""), oauth.NewRenewer())

	// 新键是 0：间隔交回 schedule.usage_refresh。
	cfg := config.Default()
	cfg.Usage.Refresh.ScanEvery = config.OptionalDuration(0)
	cfg.Schedule.UsageRefresh = config.Duration(20 * time.Millisecond)
	sc := New(mgr, cfg)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go sc.Run(runCtx)

	// 那一趟按老的间隔跑过了：快照出来了。
	deadline := time.Now().Add(5 * time.Second)
	for {
		snaps, err := s.LatestUsageSnapshots(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(snaps) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the round did not run: the interval did not go back to schedule.usage_refresh")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestTheScanEveryKeepsItsOwnInterval says the new key wins when both
// are written.
//
// 两个都写的时候新键赢：schedule.usage_refresh 只是新键写 0 时的回退。
func TestTheScanEveryKeepsItsOwnInterval(t *testing.T) {
	cfg := config.Default()
	cfg.Usage.Refresh.ScanEvery = config.OptionalDuration(30 * time.Second)
	cfg.Schedule.UsageRefresh = config.Duration(20 * time.Millisecond)
	sc := New(nil, cfg)
	if got := sc.usageEvery(); got != 30*time.Second {
		t.Fatalf("the interval = %s, want 30s: the new key wins", got)
	}
}

// TestTheUsageRoundReadsTheAccountsThatNeedIt says the round reads the
// accounts that need a reading first, and stops at the batch.
//
// 账号多起来以后，冷账号的额度没有必要读：读了也是旧的。批次满了就
// 停，剩下的下一趟接着读——排在前面的是该读的。
func TestTheUsageRoundReadsTheAccountsThatNeedIt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/minimax-cloud/api/v1/credit/details":
			w.Write([]byte(`{"total_count":1,"details":[{"credit_type":1,"granted_amount":"10","consumed_amount":"0","remaining_amount":"10","granted_at_ms":1763000000000,"expire_at_ms":1900000000000}]}`))
		default:
			w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	ctx := context.Background()
	s, err := store.Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	// acc_never 一次也没读过：它得读一次，不然卡上没有它。
	// acc_cold 很久以前读过：它排在后面，这一趟轮不到它。
	for _, id := range []string{"acc_never", "acc_cold"} {
		if err := s.CreateAccount(ctx, store.Account{
			ID: id, Name: id, Status: store.StatusActive, UserID: "1",
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
		if err := s.SaveToken(ctx, store.Token{
			AccountID: id, Access: "tok", Refresh: "ref",
			ExpiresAt: now.Add(time.Hour), UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SaveUsageSnapshot(ctx, store.UsageSnapshot{
		AccountID: "acc_cold", Granted: 100, Consumed: 10, Remaining: 90,
		HasAmounts: true, CreatedAt: now.Add(-2 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	up := upstream.New(upstream.Options{Timeout: 5 * time.Second})
	acc := account.New(up)
	// The account API is pointed at the test server. Without this the
	// test asks MiniMax for the numbers and waits for an answer that
	// never comes.
	acc.SetBase(srv.URL, srv.URL)
	mgr := manager.New(s, up, acc, egress.New(nil, ""), oauth.NewRenewer())

	cfg := config.Default()
	// 一趟只读一个：读谁由排在前面的说了算。
	cfg.Usage.Refresh.Batch = 1
	sc := New(mgr, cfg)
	sc.RefreshUsage(ctx)

	snaps, err := s.LatestUsageSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// 只有那个从没读过的被读了。
	fresh := []string{}
	for _, snap := range snaps {
		if snap.CreatedAt.After(now) {
			fresh = append(fresh, snap.AccountID)
		}
	}
	if len(fresh) != 1 || fresh[0] != "acc_never" {
		t.Fatalf("the accounts read = %v, want only acc_never: %+v", fresh, snaps)
	}
}

func TestCheckinAll(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/minimax-cloud/api/v1/signin/status":
			w.Write([]byte(`{"data":{"days":[
				{"day_no":1,"status":3,"is_today":false,"points":25},
				{"day_no":2,"status":3,"is_today":false,"points":25},
				{"day_no":3,"status":3,"is_today":false,"points":25},
				{"day_no":4,"status":2,"is_today":true,"points":25},
				{"day_no":5,"status":1,"is_today":false,"points":25},
				{"day_no":6,"status":1,"is_today":false,"points":25},
				{"day_no":7,"status":1,"is_today":false,"points":25}]}}`))
		case "/minimax-cloud/api/v1/signin/claim":
			w.Write([]byte(`{"data":{"claim_result":1,"points":25}}`))
		default:
			w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	ctx := context.Background()
	s, err := store.Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	s.Migrate(ctx)
	defer s.Close()
	now := time.Now()
	s.CreateAccount(ctx, store.Account{
		ID: "acc_01", Name: "a", Status: store.StatusActive, UserID: "1",
		CreatedAt: now, UpdatedAt: now,
	})
	s.SaveToken(ctx, store.Token{
		AccountID: "acc_01", Access: "tok", Refresh: "ref",
		ExpiresAt: now.Add(time.Hour), UpdatedAt: now,
	})
	up := upstream.New(upstream.Options{Timeout: 5 * time.Second})
	acc := account.New(up)
	// The account API is pointed at the test server. Without this the
	// test asks MiniMax for the numbers and waits for an answer that
	// never comes.
	acc.SetBase(srv.URL, srv.URL)
	mgr := manager.New(s, up, acc, egress.New(nil, ""), oauth.NewRenewer())

	sc := New(mgr, config.Default())
	results := sc.CheckinAll(ctx)
	if len(results) != 1 {
		t.Fatalf("results = %+v", results)
	}
	if results[0].Outcome != "claimed" {
		t.Fatalf("outcome = %q", results[0].Outcome)
	}
}

// TestCheckEgressWritesTheHealth says the result of the outlet check
// reaches the database.
//
// 出口的健康检查写不进数据库，WebUI 的出口页就永远是"没查过"：
// 控制台说两个出口都是好的，表里一行也没有。
func TestCheckEgressWritesTheHealth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`ok`))
	}))
	defer srv.Close()

	ctx := context.Background()
	s, err := store.Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	s.Migrate(ctx)
	defer s.Close()

	up := upstream.New(upstream.Options{Timeout: 5 * time.Second})
	acc := account.New(up)
	mgr := manager.New(s, up, acc, egress.New(nil, ""), oauth.NewRenewer())

	cfg := config.Default()
	// "direct" 不走代理，请求直接到那台假服务器上。
	cfg.Proxy.Groups = map[string]string{"local": "direct"}
	sc := New(mgr, cfg)
	sc.target = srv.URL

	sc.CheckEgress(ctx)

	rows, err := s.EgressHealth(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("the health table has %d rows, want 1: %+v", len(rows), rows)
	}
	if rows[0].URL != "direct" {
		t.Errorf("the row is for %q, want %q", rows[0].URL, "direct")
	}
	if !rows[0].OK {
		t.Errorf("the row says the outlet is not usable: %+v", rows[0])
	}
	if rows[0].Error != "" {
		t.Errorf("the row carries an error: %q", rows[0].Error)
	}
}

// TestCheckEgressWritesAFailedHealth says a broken outlet is written
// down as broken.
//
// 出口不通的时候，表里要有那一行，而且要写清楚为什么不通。
func TestCheckEgressWritesAFailedHealth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	ctx := context.Background()
	s, err := store.Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	s.Migrate(ctx)
	defer s.Close()

	up := upstream.New(upstream.Options{Timeout: 5 * time.Second})
	acc := account.New(up)
	mgr := manager.New(s, up, acc, egress.New(nil, ""), oauth.NewRenewer())

	cfg := config.Default()
	cfg.Proxy.Groups = map[string]string{"local": "direct"}
	sc := New(mgr, cfg)
	sc.target = srv.URL

	sc.CheckEgress(ctx)

	rows, err := s.EgressHealth(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("the health table has %d rows, want 1: %+v", len(rows), rows)
	}
	if rows[0].OK {
		t.Errorf("the row says the outlet is usable: %+v", rows[0])
	}
	if rows[0].Error == "" {
		t.Error("the row does not say why the outlet is broken")
	}
}

func TestTheReloginJobSignsTheSignedOutAccountsInAgain(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := s.CreateAccount(ctx, store.Account{
		ID: "acc_01", Name: "a", Status: store.StatusSignedOut,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	key := secret.Key(bytes.Repeat([]byte{7}, secret.KeySize))
	cipher, err := key.Seal("hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetPassword(ctx, "acc_01", cipher, now); err != nil {
		t.Fatal(err)
	}
	up := upstream.New(upstream.Options{Timeout: 5 * time.Second})
	acc := account.New(up)
	mgr := manager.New(s, up, acc, egress.New(nil, ""), oauth.NewRenewer())
	mgr.Key = key
	mgr.ReloginFn = func(ctx context.Context, a store.Account, password string) error {
		return s.SaveToken(ctx, store.Token{
			AccountID: a.ID, Access: "a", Refresh: "r",
			ExpiresAt: now.Add(time.Hour), UpdatedAt: now,
		})
	}

	sc := New(mgr, config.Default())
	sc.Relogin(ctx)

	got, err := s.GetAccount(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.StatusActive {
		t.Fatalf("status = %q", got.Status)
	}
}

// countReadings says how many usage readings the database holds.
//
// 调度器的那个包外面数不到行数：库的读法只有「每个账号最新的一条」。这
// 个测试要数的是全部的行，所以自己开一条连接去数。
func countReadings(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM usage_snapshots").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestTheUsageRoundThrowsAwayTheOldReadings says the usage round throws
// away the readings that are not needed.
//
// 调度器每 scan_every 扫一趟，每读一个账号就写一行读数：不收拾的话一天
// 下来就是几千行，而除了每个账号最新的一条，剩下的谁也不看。
func TestTheUsageRoundThrowsAwayTheOldReadings(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/minimax-cloud/api/v1/credit/details":
			w.Write([]byte(`{"total_count":1,"details":[{"credit_type":1,"granted_amount":"10","consumed_amount":"0","remaining_amount":"10","granted_at_ms":1763000000000,"expire_at_ms":1900000000000}]}`))
		default:
			w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	ctx := context.Background()
	path := t.TempDir() + "/t.db"
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	if err := s.CreateAccount(ctx, store.Account{
		ID: "acc_01", Name: "a", Status: store.StatusActive, UserID: "1",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveToken(ctx, store.Token{
		AccountID: "acc_01", Access: "tok", Refresh: "ref",
		ExpiresAt: now.Add(time.Hour), UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	// 三次很早以前的读。
	for i := 0; i < 3; i++ {
		if err := s.SaveUsageSnapshot(ctx, store.UsageSnapshot{
			AccountID: "acc_01", HasAmounts: true,
			Granted: 10, Remaining: 10,
			CreatedAt: now.Add(-2 * time.Hour).Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
	}
	up := upstream.New(upstream.Options{Timeout: 5 * time.Second})
	acc := account.New(up)
	acc.SetBase(srv.URL, srv.URL)
	mgr := manager.New(s, up, acc, egress.New(nil, ""), oauth.NewRenewer())
	// 它刚被用过：它是热的，这一趟要读它。
	mgr.MarkUsed("acc_01")

	sc := New(mgr, config.Default())
	sc.RefreshUsage(ctx)

	// 刚才读的那一次留下，三次很早以前的都扔掉。
	if n := countReadings(t, path); n != 1 {
		t.Fatalf("the readings = %d, want only the newest one", n)
	}
}

// TestTheSchedulerCanBeWaitedFor says the scheduler's work can be waited
// for.
//
// 进程要关的时候，调度器的活儿也得停，而且要等它停下来：库是接着关的，
// 它不该往一个正在关着的库上写（那是「database is closed」）。
func TestTheSchedulerCanBeWaitedFor(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	up := upstream.New(upstream.Options{Timeout: 5 * time.Second})
	acc := account.New(up)
	mgr := manager.New(s, up, acc, egress.New(nil, ""), oauth.NewRenewer())
	sc := New(mgr, config.Default())

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc.Run(runCtx)
	}()
	// 停它。
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the scheduler did not stop")
	}
	// 等它的活儿停下来。
	waited := make(chan struct{})
	go func() {
		defer close(waited)
		sc.Wait()
	}()
	select {
	case <-waited:
	case <-time.After(10 * time.Second):
		t.Fatal("the scheduler's work was not waited for")
	}
}

// TestTheCheckinSkipsTheAccountsThatSignedInToday says the accounts that
// were checked in today are not checked in again.
//
// 签过的再签一次，站点只会答 done，白花一次调用 —— 而今天没签到的那部
// 分，正是服务器 00:00 没开着时漏掉的那部分（规格 §5.2）。
func TestTheCheckinSkipsTheAccountsThatSignedInToday(t *testing.T) {
	var claims atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/minimax-cloud/api/v1/signin/status":
			w.Write([]byte(`{"data":{"days":[
				{"day_no":1,"status":3,"is_today":false,"points":25},
				{"day_no":2,"status":3,"is_today":false,"points":25},
				{"day_no":3,"status":3,"is_today":false,"points":25},
				{"day_no":4,"status":2,"is_today":true,"points":25},
				{"day_no":5,"status":1,"is_today":false,"points":25},
				{"day_no":6,"status":1,"is_today":false,"points":25},
				{"day_no":7,"status":1,"is_today":false,"points":25}]}}`))
		case "/minimax-cloud/api/v1/signin/claim":
			claims.Add(1)
			w.Write([]byte(`{"data":{"claim_result":1,"points":25}}`))
		default:
			w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	ctx := context.Background()
	s, err := store.Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	s.Migrate(ctx)
	defer s.Close()
	now := time.Now()
	for _, id := range []string{"acc_done", "acc_waiting"} {
		s.CreateAccount(ctx, store.Account{
			ID: id, Name: id, Status: store.StatusActive, UserID: "1",
			CreatedAt: now, UpdatedAt: now,
		})
		s.SaveToken(ctx, store.Token{
			AccountID: id, Access: "tok", Refresh: "ref",
			ExpiresAt: now.Add(time.Hour), UpdatedAt: now,
		})
	}
	// 这个号今天签过了。
	s.SaveCheckin(ctx, store.Checkin{
		AccountID: "acc_done", Day: store.DayKey(now), Outcome: "claimed",
		Credit: 25, CreatedAt: now,
	})

	up := upstream.New(upstream.Options{Timeout: 5 * time.Second})
	acc := account.New(up)
	acc.SetBase(srv.URL, srv.URL)
	mgr := manager.New(s, up, acc, egress.New(nil, ""), oauth.NewRenewer())

	sc := New(mgr, config.Default())
	results := sc.CheckinAll(ctx)
	if len(results) != 1 || results[0].AccountID != "acc_waiting" {
		t.Fatalf("the results = %+v, want just the waiting account", results)
	}
	if n := claims.Load(); n != 1 {
		t.Fatalf("the claims = %d, want 1", n)
	}
}

// TestTheCheckinIsMadeUpOnStartup says the accounts that have not signed
// in today are checked in when the process starts.
//
// 服务器 00:00 没开着的时候，那一天的签到就没人做了 —— 起进程的时候补
// 一趟，签的是今天还没签的号（规格 §5.2）。
func TestTheCheckinIsMadeUpOnStartup(t *testing.T) {
	var claims atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/minimax-cloud/api/v1/signin/status":
			w.Write([]byte(`{"data":{"days":[
				{"day_no":1,"status":3,"is_today":false,"points":25},
				{"day_no":2,"status":3,"is_today":false,"points":25},
				{"day_no":3,"status":3,"is_today":false,"points":25},
				{"day_no":4,"status":2,"is_today":true,"points":25},
				{"day_no":5,"status":1,"is_today":false,"points":25},
				{"day_no":6,"status":1,"is_today":false,"points":25},
				{"day_no":7,"status":1,"is_today":false,"points":25}]}}`))
		case "/minimax-cloud/api/v1/signin/claim":
			claims.Add(1)
			w.Write([]byte(`{"data":{"claim_result":1,"points":25}}`))
		default:
			w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	ctx := context.Background()
	s, err := store.Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	s.Migrate(ctx)
	defer s.Close()
	now := time.Now()
	s.CreateAccount(ctx, store.Account{
		ID: "acc_01", Name: "a", Status: store.StatusActive, UserID: "1",
		CreatedAt: now, UpdatedAt: now,
	})
	s.SaveToken(ctx, store.Token{
		AccountID: "acc_01", Access: "tok", Refresh: "ref",
		ExpiresAt: now.Add(time.Hour), UpdatedAt: now,
	})

	up := upstream.New(upstream.Options{Timeout: 5 * time.Second})
	acc := account.New(up)
	acc.SetBase(srv.URL, srv.URL)
	mgr := manager.New(s, up, acc, egress.New(nil, ""), oauth.NewRenewer())

	sc := New(mgr, config.Default())
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go sc.Run(runCtx)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if claims.Load() > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the check-in was not made up on startup")
}

// TestTheNextCheckinIsJittered says the daily check-in may be held back.
//
// 抖动是配置里的一个键（schedule.checkin_jitter）：几个实例一起跑的时候
// 抖一下就散开了。零是它的默认值（不抖），写死 30 分钟是另一回事 ——
// 那是 §5.1 里点名要去掉的。
func TestTheNextCheckinIsJittered(t *testing.T) {
	at, err := time.Parse("15:04", "00:00")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 18, 20, 0, 0, 0, time.UTC)
	// 不抖的时候是那一刻。
	want := time.Date(2026, 11, 19, 16, 0, 0, 0, time.UTC)
	seen := map[time.Time]bool{}
	for i := 0; i < 40; i++ {
		got := NextCheckin(now, at, 30*time.Minute)
		if got.Before(want) || !got.Before(want.Add(30*time.Minute)) {
			t.Fatalf("next = %v, want between %v and %v", got, want, want.Add(30*time.Minute))
		}
		seen[got] = true
	}
	if len(seen) < 2 {
		t.Fatal("the check-in time does not jitter")
	}
}

// TestTheJitterDoesNotCrossTheSitesMidnight says the jitter cannot push
// the check-in into the next day.
//
// 「23:50」抖 30 分钟就是 00:20 —— 那是站点第二天的积分，那一份拿不到，
// 而且站点只给一天一次（规格 §5.1）。跨了就不抖。
func TestTheJitterDoesNotCrossTheSitesMidnight(t *testing.T) {
	at, err := time.Parse("15:04", "23:50")
	if err != nil {
		t.Fatal(err)
	}
	// UTC 的 11-18 20:00 是大陆时间的 11-19 04:00：今天 23:50 还没到。
	now := time.Date(2026, 11, 18, 20, 0, 0, 0, time.UTC)
	for i := 0; i < 40; i++ {
		got := NextCheckin(now, at, 30*time.Minute)
		if day := store.DayKey(got); day != "2026-11-19" {
			t.Fatalf("the check-in landed on %s, want 2026-11-19", day)
		}
	}
}

// TestTheCheckinPassesDoNotOverlap says two check-in passes do not run at
// the same time.
//
// 起进程的补签和 00:00 那趟会碰上：23:59 起进程，补签还没跑完就到 00:00。
// 叠着跑就是同一个号被站点拒一次 —— 白花一次调用（规格 §5.2）。
func TestTheCheckinPassesDoNotOverlap(t *testing.T) {
	var claims atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/minimax-cloud/api/v1/signin/status":
			w.Write([]byte(`{"data":{"days":[
				{"day_no":1,"status":3,"is_today":false,"points":25},
				{"day_no":2,"status":3,"is_today":false,"points":25},
				{"day_no":3,"status":3,"is_today":false,"points":25},
				{"day_no":4,"status":2,"is_today":true,"points":25},
				{"day_no":5,"status":1,"is_today":false,"points":25},
				{"day_no":6,"status":1,"is_today":false,"points":25},
				{"day_no":7,"status":1,"is_today":false,"points":25}]}}`))
		case "/minimax-cloud/api/v1/signin/claim":
			claims.Add(1)
			w.Write([]byte(`{"data":{"claim_result":1,"points":25}}`))
		default:
			w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	ctx := context.Background()
	s, err := store.Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	s.Migrate(ctx)
	defer s.Close()
	now := time.Now()
	s.CreateAccount(ctx, store.Account{
		ID: "acc_01", Name: "a", Status: store.StatusActive, UserID: "1",
		CreatedAt: now, UpdatedAt: now,
	})
	s.SaveToken(ctx, store.Token{
		AccountID: "acc_01", Access: "tok", Refresh: "ref",
		ExpiresAt: now.Add(time.Hour), UpdatedAt: now,
	})

	up := upstream.New(upstream.Options{Timeout: 5 * time.Second})
	acc := account.New(up)
	acc.SetBase(srv.URL, srv.URL)
	mgr := manager.New(s, up, acc, egress.New(nil, ""), oauth.NewRenewer())

	sc := New(mgr, config.Default())
	// 前一趟还在跑（这里就是拿着锁的那一趟）。
	sc.checkinMu.Lock()
	started := make(chan struct{})
	go func() {
		close(started)
		sc.checkinAll(ctx)
	}()
	<-started
	time.Sleep(200 * time.Millisecond)
	if n := claims.Load(); n != 0 {
		t.Fatalf("the claims = %d while another pass was running, want 0", n)
	}
	sc.checkinMu.Unlock()

	// 前一趟完了，后一趟才跑。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if claims.Load() > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the pass did not run after the other one was done")
}
