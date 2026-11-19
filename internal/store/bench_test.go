package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The benchmarks measure the queries that run on their own, over a
// database with the size the gateway is meant to carry: a thousand
// accounts and a month of check-ins.
//
// 测的是**查询本身**，不是整个服务：网关每 30 秒补一次池、调度器每 30 秒
// 跑一趟读额度、每个客户端请求问一次号剩多少 —— 这些 SQL 就是它们的
// 全部开销。改了索引或者改了写法，`go test -bench` 一跑就知道。
//
// 夹具的样子是**稳态**：签到的历史留 30 天（sqlite.checkin_retention_days），
// 每个号留最新的一条读数，外加最近一刻钟里读的那些（清理那一趟留的）。
// 一年 36 万行签到的那种库不是稳态 —— 那是没删干净的库。

// fixture is the database the benchmarks run against.
type fixture struct {
	store *Store
	dir   string
}

var (
	sharedFixture *fixture
	fixtureErr    error
)

// theFixture builds the database the benchmarks share. It is built once.
func theFixture(tb testing.TB) *fixture {
	tb.Helper()
	if sharedFixture == nil {
		sharedFixture, fixtureErr = buildFixture()
	}
	if fixtureErr != nil {
		tb.Fatal(fixtureErr)
	}
	return sharedFixture
}

// The shape of the fixture.
const (
	// fixtureAccounts is how many accounts the gateway is meant to carry.
	fixtureAccounts = 1000
	// fixtureDays is how many days of check-ins are kept.
	fixtureDays = 30
	// fixtureRounds is how many rounds of readings are still in the table:
	// one per account plus the ones of the last quarter of an hour.
	fixtureRounds = 15
)

// buildFixture makes a database with a thousand accounts and a month of
// check-ins.
func buildFixture() (*fixture, error) {
	dir, err := os.MkdirTemp("", "mmxbench")
	if err != nil {
		return nil, err
	}
	s, err := Open(filepath.Join(dir, "bench.db"),
		WithCacheMB(8), WithSynchronous("normal"))
	if err != nil {
		return nil, err
	}
	if err := s.Migrate(context.Background()); err != nil {
		return nil, err
	}
	ctx := context.Background()
	now := time.Now()
	for i := 0; i < fixtureAccounts; i++ {
		id := fmt.Sprintf("acc_%04d", i)
		if err := s.CreateAccount(ctx, Account{
			ID: id, Name: "name-" + id, UserID: fmt.Sprintf("user-%d", i),
			Email: id + "@example.com", Status: StatusActive,
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			return nil, err
		}
		if err := s.SaveToken(ctx, Token{
			AccountID: id, Access: "tok", Refresh: "ref",
			ExpiresAt: now.Add(time.Hour), UpdatedAt: now,
		}); err != nil {
			return nil, err
		}
	}
	// 一个月的签到：一天一个号一行。这是库里唯一会长大的东西。
	//
	// 一批一批地插（一条语句插一万行）：夹具是给基准用的，不要把时间
	// 花在造数据上。
	day := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO checkins (account_id,day,outcome,credit,streak,message,created_at)
		 VALUES (?,?,?,?,?,?,?)`)
	if err != nil {
		return nil, err
	}
	for d := 0; d < fixtureDays; d++ {
		key := day.AddDate(0, 0, d).Format("2006-01-02")
		for i := 0; i < fixtureAccounts; i++ {
			if _, err := stmt.ExecContext(ctx,
				fmt.Sprintf("acc_%04d", i), key, "claimed", 25.0, 7, "", now.UnixMilli()); err != nil {
				return nil, err
			}
		}
	}
	if err := stmt.Close(); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	// 读数：每个号最新的一条，外加最近一刻钟里那一轮轮的。
	//
	// 清理那一趟（PruneUsageSnapshots）留的就是这个形状：每个号一条最新
	// 的，加上 15 分钟之内的那几条。
	for r := 0; r < fixtureRounds; r++ {
		at := now.Add(-time.Duration(r) * 30 * time.Second)
		for i := 0; i < fixtureAccounts; i++ {
			if err := s.SaveUsageSnapshot(ctx, UsageSnapshot{
				AccountID: fmt.Sprintf("acc_%04d", i),
				Granted:   100, Consumed: 1, Remaining: 99,
				CreatedAt: at,
			}); err != nil {
				return nil, err
			}
		}
	}
	return &fixture{store: s, dir: dir}, nil
}

// BenchmarkPoolCandidates measures the query that fills the pool.
func BenchmarkPoolCandidates(b *testing.B) {
	f := theFixture(b)
	ctx := context.Background()
	since := time.Now().Add(-time.Hour)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cands, err := f.store.PoolCandidates(ctx, 400, map[string]bool{}, since)
		if err != nil {
			b.Fatal(err)
		}
		if len(cands) == 0 {
			b.Fatal("no candidates")
		}
	}
}

// BenchmarkLatestUsageSnapshots measures the query the card and the
// fallback ask for.
func BenchmarkLatestUsageSnapshots(b *testing.B) {
	f := theFixture(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		snaps, err := f.store.LatestUsageSnapshots(ctx)
		if err != nil {
			b.Fatal(err)
		}
		if len(snaps) != fixtureAccounts {
			b.Fatalf("snaps = %d, want %d", len(snaps), fixtureAccounts)
		}
	}
}

// BenchmarkAccountsWithoutCheckin measures the list the check-in signs.
//
// 用夹具里没有签到记录的那一天：签到那一趟问的就是「今天谁还没签」，
// 答是「一千个」。
func BenchmarkAccountsWithoutCheckin(b *testing.B) {
	f := theFixture(b)
	ctx := context.Background()
	day := "2027-01-01"
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		accs, err := f.store.AccountsWithoutCheckin(ctx, day)
		if err != nil {
			b.Fatal(err)
		}
		if len(accs) != fixtureAccounts {
			b.Fatalf("accounts = %d, want %d", len(accs), fixtureAccounts)
		}
	}
}

// BenchmarkCheckinTimes measures the check-in times the credits round
// asks for.
//
// 读额度那一趟每 30 秒问一次「最近签到的号是什么时候」。
func BenchmarkCheckinTimes(b *testing.B) {
	f := theFixture(b)
	ctx := context.Background()
	since := time.Now().AddDate(0, 0, -7)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		times, err := f.store.CheckinTimes(ctx, since)
		if err != nil {
			b.Fatal(err)
		}
		if len(times) == 0 {
			b.Fatal("no check-in times")
		}
	}
}

// BenchmarkDayCheckins measures the credits the check-ins gave.
func BenchmarkDayCheckins(b *testing.B) {
	f := theFixture(b)
	ctx := context.Background()
	day := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC).Format("2006-01-02")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		credits, err := f.store.DayCheckins(ctx, day)
		if err != nil {
			b.Fatal(err)
		}
		if len(credits) != fixtureAccounts {
			b.Fatalf("credits = %d, want %d", len(credits), fixtureAccounts)
		}
	}
}

// BenchmarkCountUsable measures the count the concurrency promise is
// computed from.
func BenchmarkCountUsable(b *testing.B) {
	f := theFixture(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		n, err := f.store.CountUsable(ctx)
		if err != nil {
			b.Fatal(err)
		}
		if n != fixtureAccounts {
			b.Fatalf("usable = %d, want %d", n, fixtureAccounts)
		}
	}
}

// BenchmarkPruneUsageSnapshots measures the cleanup that runs every round.
func BenchmarkPruneUsageSnapshots(b *testing.B) {
	f := theFixture(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := f.store.PruneUsageSnapshots(ctx, time.Now().Add(-15*time.Minute), 1); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkSaveUsageSnapshot measures one write: what the credits round
// does, six times every thirty seconds.
func BenchmarkSaveUsageSnapshot(b *testing.B) {
	f := theFixture(b)
	ctx := context.Background()
	now := time.Now()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := f.store.SaveUsageSnapshot(ctx, UsageSnapshot{
			AccountID: "acc_0000", Granted: 100, Consumed: 1, Remaining: 99,
			CreatedAt: now,
		}); err != nil {
			b.Fatal(err)
		}
	}
}

// The isolation benchmarks: the old SQL and the new SQL, on the same
// database.
//
// 前面的基准量的是「改之前」和「改之后」两个整体。这几个量的是**改写本身**
// 值多少：两条 SQL 在同一个库上跑，索引是同一套，差的只有写法。
//
// 改写的那两条（`NOT IN` 和从 accounts 出发）在两边都留着：将来谁把写法
// 改回去，基准会立刻说出来。

// isoCount runs a query and counts the rows it gives.
func isoCount(ctx context.Context, s *Store, q string, args ...any) (int, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		n++
	}
	return n, rows.Err()
}

// isoNoCheckinOldSQL is how the list of the accounts to check in used to
// be asked for.
const isoNoCheckinOldSQL = `
	SELECT a.id FROM accounts a
	WHERE a.status = ?
	  AND NOT EXISTS (SELECT 1 FROM checkins c WHERE c.account_id = a.id AND c.day = ?)
	ORDER BY a.created_at, a.id`

// isoNoCheckinNewSQL is how it is asked for now.
const isoNoCheckinNewSQL = `
	SELECT a.id FROM accounts a
	WHERE a.status = ?
	  AND a.id NOT IN (SELECT c.account_id FROM checkins c WHERE c.day = ?)
	ORDER BY a.created_at, a.id`

// isoUsageOldSQL is how the latest reading of every account used to be
// asked for.
var isoUsageOldSQL = `
	SELECT ` + usageSnapshotColumns("s") + `
	FROM usage_snapshots s
	WHERE s.id = (SELECT s2.id FROM usage_snapshots s2
		WHERE s2.account_id = s.account_id
		ORDER BY s2.created_at DESC, s2.id DESC LIMIT 1)
	ORDER BY s.account_id`

// isoUsageNewSQL is how it is asked for now.
var isoUsageNewSQL = `
	SELECT ` + usageSnapshotColumns("u") + `
	FROM accounts a JOIN usage_snapshots u ON u.id = (
		SELECT s2.id FROM usage_snapshots s2
		WHERE s2.account_id = a.id
		ORDER BY s2.created_at DESC, s2.id DESC LIMIT 1)
	ORDER BY a.id`

// BenchmarkIsoNoCheckinOld measures the old way of asking the list.
func BenchmarkIsoNoCheckinOld(b *testing.B) {
	f := theFixture(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := isoCount(ctx, f.store, isoNoCheckinOldSQL, StatusActive, "2027-01-01"); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkIsoNoCheckinNew measures the new way of asking the list.
func BenchmarkIsoNoCheckinNew(b *testing.B) {
	f := theFixture(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := isoCount(ctx, f.store, isoNoCheckinNewSQL, StatusActive, "2027-01-01"); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkIsoUsageOld measures the old way of asking the latest reading.
func BenchmarkIsoUsageOld(b *testing.B) {
	f := theFixture(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := isoCount(ctx, f.store, isoUsageOldSQL); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkIsoUsageNew measures the new way of asking the latest reading.
func BenchmarkIsoUsageNew(b *testing.B) {
	f := theFixture(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := isoCount(ctx, f.store, isoUsageNewSQL); err != nil {
			b.Fatal(err)
		}
	}
}

// TestMain cleans up the fixture the benchmarks share.
func TestMain(m *testing.M) {
	code := m.Run()
	if sharedFixture != nil {
		os.RemoveAll(sharedFixture.dir)
	}
	os.Exit(code)
}
