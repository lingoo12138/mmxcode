package usage

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/lingoo12138/mmxcode/internal/config"
	"github.com/lingoo12138/mmxcode/internal/manager"
	"github.com/lingoo12138/mmxcode/internal/store"

	_ "modernc.org/sqlite"
)

// TestTheAccountsToReadAreTheOnesThatNeedIt says the read set is the
// accounts that need a reading, in the order they should be read.
//
// 该读的 = 从没读过的 ∪ 热的 ∪ 签到待补的 ∪ 标记到期的。没读过的排
// 在热的前面：它是一次性的活儿，排在后面就永远排在后面。冷的账号不
// 在名单里：它的数是旧的，读它也是把旧的数再读一遍。
func TestTheAccountsToReadAreTheOnesThatNeedIt(t *testing.T) {
	s := newStore(t)
	now := time.Now()
	mk(t, s, "acc_cold", now.Add(-time.Minute))
	mk(t, s, "acc_never", time.Time{})
	mk(t, s, "acc_hot", now.Add(-time.Hour))
	mk(t, s, "acc_pending", now.Add(-time.Hour))
	mk(t, s, "acc_due", now.Add(-time.Hour))

	// 签到待补：签到比快照新，卡上还是签到前的旧数。
	if err := s.SaveCheckin(context.Background(), store.Checkin{
		AccountID: "acc_pending", Day: "2026-11-15", Outcome: "claimed",
		Credit: 10, CreatedAt: now,
	}); err != nil {
		t.Fatalf("save checkin: %v", err)
	}
	// 标记到期：标记的时刻到了，账号回到轮转里，但没人读过它。
	if err := s.SetExhausted(context.Background(), "acc_due", "general",
		now.Add(-time.Minute), "quota"); err != nil {
		t.Fatalf("set exhausted: %v", err)
	}

	r := NewRound(s, nil, func() []string { return []string{"acc_hot"} }, config.Default())
	got, err := r.Targets(context.Background())
	if err != nil {
		t.Fatalf("targets: %v", err)
	}
	want := []string{"acc_pending", "acc_due", "acc_never", "acc_hot"}
	if len(got) != len(want) {
		t.Fatalf("the accounts to read = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("the accounts to read = %v, want %v", got, want)
		}
	}
}

// TestTheColdAccountsAreLeftAlone says a cold account is not read.
//
// 冷账号的数是旧的，读它也是把旧的数再读一遍。它自己会醒过来：旧的
// 数是高估（积分只会往下走），轮转会挑它，它就热了，热了就该读。
func TestTheColdAccountsAreLeftAlone(t *testing.T) {
	s := newStore(t)
	now := time.Now()
	mk(t, s, "acc_cold", now.Add(-time.Minute))
	r := NewRound(s, nil, func() []string { return nil }, config.Default())
	got, err := r.Targets(context.Background())
	if err != nil {
		t.Fatalf("targets: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("the accounts to read = %v, want none", got)
	}
}

// TestTheDueAccountIsReadOnceAfterItsMarkExpires says a spent mark
// that has expired is read once, and then the account is left alone.
//
// 标记自然过期 → 到期读一次 → 数变新，账号回轮转（规格 §4.5 的恢复
// 点 A）。一次就好：读过的账号不到期了，不然六个到期的账号就把一趟
// 的批次占满，从来没读过的账号永远读不到。
func TestTheDueAccountIsReadOnceAfterItsMarkExpires(t *testing.T) {
	s := newStore(t)
	now := time.Now()
	mk(t, s, "acc_due", now.Add(-time.Hour))
	if err := s.SetExhausted(context.Background(), "acc_due", "general",
		now.Add(-time.Minute), "quota"); err != nil {
		t.Fatalf("set exhausted: %v", err)
	}
	r := NewRound(s, nil, func() []string { return nil }, config.Default())
	got, err := r.Targets(context.Background())
	if err != nil {
		t.Fatalf("targets: %v", err)
	}
	if len(got) != 1 || got[0] != "acc_due" {
		t.Fatalf("the accounts to read = %v, want the due one", got)
	}
	// 读过了：数变新了，它不到期了。
	if err := s.SaveUsageSnapshot(context.Background(), store.UsageSnapshot{
		AccountID: "acc_due", Granted: 100, Consumed: 10, Remaining: 90,
		HasAmounts: true, CreatedAt: now,
	}); err != nil {
		t.Fatalf("save snapshot: %v", err)
	}
	got, err = r.Targets(context.Background())
	if err != nil {
		t.Fatalf("targets: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("the accounts to read = %v, want none", got)
	}
}

// TestTheAccountThatIsStillSpentIsNotDue says an account that is
// still spent for one group is not read.
//
// 它还有一个组欠着额度：读它只是把用完的那一组再读一遍。标记全过期
// 了它才轮到——没过期的那一个说明它还欠着，回不了轮转。
func TestTheAccountThatIsStillSpentIsNotDue(t *testing.T) {
	s := newStore(t)
	now := time.Now()
	mk(t, s, "acc_spent", now.Add(-time.Hour))
	mk(t, s, "acc_waiting", now.Add(-time.Hour))
	// 一个过期了，另一个还没有。
	if err := s.SetExhausted(context.Background(), "acc_spent", "general",
		now.Add(-time.Minute), "quota"); err != nil {
		t.Fatalf("set exhausted: %v", err)
	}
	if err := s.SetExhausted(context.Background(), "acc_spent", "other",
		now.Add(time.Hour), "quota"); err != nil {
		t.Fatalf("set exhausted: %v", err)
	}
	// 标记还没到：它还欠着，不该读。
	if err := s.SetExhausted(context.Background(), "acc_waiting", "general",
		now.Add(time.Hour), "quota"); err != nil {
		t.Fatalf("set exhausted: %v", err)
	}
	r := NewRound(s, nil, func() []string { return nil }, config.Default())
	got, err := r.Targets(context.Background())
	if err != nil {
		t.Fatalf("targets: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("the accounts to read = %v, want none", got)
	}
}

// TestTheFailedReadingIsNotRetriedForever says a reading that failed is
// tried again for a while, and then the account is left alone.
//
// 读失败的账号要再读（签到给的积分还在账本里等着），可是不能一直重
// 试：一个永远读不回来的账号就占着待补那一位，一趟的批次全被它拿
// 走，从来没读过的账号再也读不到——而每个账号至少读一次是硬要求。
func TestTheFailedReadingIsNotRetriedForever(t *testing.T) {
	s := newStore(t)
	now := time.Now()
	mk(t, s, "acc_failed", now.Add(-time.Hour))
	// 那次读失败了：没有数。
	if err := s.SaveUsageSnapshot(context.Background(), store.UsageSnapshot{
		AccountID: "acc_failed", HasAmounts: false, CreatedAt: now,
	}); err != nil {
		t.Fatalf("save snapshot: %v", err)
	}
	// 签到不久之前：还再读一遍。
	if err := s.SaveCheckin(context.Background(), store.Checkin{
		AccountID: "acc_failed", Day: "2026-11-15", Outcome: "claimed",
		Credit: 10, CreatedAt: now.Add(-time.Minute),
	}); err != nil {
		t.Fatalf("save checkin: %v", err)
	}
	r := NewRound(s, nil, func() []string { return nil }, config.Default())
	got, err := r.Targets(context.Background())
	if err != nil {
		t.Fatalf("targets: %v", err)
	}
	if len(got) != 1 || got[0] != "acc_failed" {
		t.Fatalf("the accounts to read = %v, want the failed one", got)
	}
	// 十一分钟以后：不再重读了，把它放下。
	if err := s.SaveCheckin(context.Background(), store.Checkin{
		AccountID: "acc_failed", Day: "2026-11-15", Outcome: "claimed",
		Credit: 10, CreatedAt: now.Add(-11 * time.Minute),
	}); err != nil {
		t.Fatalf("save checkin: %v", err)
	}
	got, err = r.Targets(context.Background())
	if err != nil {
		t.Fatalf("targets: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("the accounts to read = %v, want none", got)
	}
}

// TestTheCheckInThatIsNotOnTheCardIsStillPending says a check-in that
// is newer than the reading is pending, whatever the retry window says.
//
// 签到给的积分卡上还没有，那是要补的：规格 §5.3 的那条规则跟重试的
// 期限是两回事。
func TestTheCheckInThatIsNotOnTheCardIsStillPending(t *testing.T) {
	s := newStore(t)
	now := time.Now()
	mk(t, s, "acc_checked", now.Add(-time.Hour))
	if err := s.SaveCheckin(context.Background(), store.Checkin{
		AccountID: "acc_checked", Day: "2026-11-15", Outcome: "claimed",
		Credit: 10, CreatedAt: now.Add(-11 * time.Minute),
	}); err != nil {
		t.Fatalf("save checkin: %v", err)
	}
	r := NewRound(s, nil, func() []string { return nil }, config.Default())
	got, err := r.Targets(context.Background())
	if err != nil {
		t.Fatalf("targets: %v", err)
	}
	if len(got) != 1 || got[0] != "acc_checked" {
		t.Fatalf("the accounts to read = %v, want the pending one", got)
	}
}

// TestTheAccountsToReadSkipTheSignedOut says a signed-out account is
// not read.
//
// 它读不到：读了也是白读。
func TestTheAccountsToReadSkipTheSignedOut(t *testing.T) {
	s := newStore(t)
	now := time.Now()
	mk(t, s, "acc_out", time.Time{})
	if err := s.UpdateAccountStatus(context.Background(), "acc_out",
		store.StatusSignedOut, "the site refused the token", now); err != nil {
		t.Fatalf("sign out: %v", err)
	}
	r := NewRound(s, nil, func() []string { return nil }, config.Default())
	got, err := r.Targets(context.Background())
	if err != nil {
		t.Fatalf("targets: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("the accounts to read = %v, want none", got)
	}
}

// newStore gives a store with the schema in it.
func newStore(t *testing.T) *store.Store {
	t.Helper()
	s, _ := newStoreWithPath(t)
	return s
}

// newStoreWithPath gives a store with the schema in it, and says where
// its database is.
//
// 一个想知道「那笔写到底在不在盘上」的测试要自己打开那些文件：库文件和
// 它旁边的 WAL 一起拷走 —— 那两个文件加起来就是盘上的东西。
func newStoreWithPath(t *testing.T) (*store.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mmxcode.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return s, path
}

// mk makes an account, with a reading of it or without one.
func mk(t *testing.T, s *store.Store, id string, readAt time.Time) {
	t.Helper()
	now := time.Now()
	if err := s.CreateAccount(context.Background(), store.Account{
		ID: id, Name: id, Status: store.StatusActive, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create account: %v", err)
	}
	if readAt.IsZero() {
		return
	}
	if err := s.SaveUsageSnapshot(context.Background(), store.UsageSnapshot{
		AccountID: id, Granted: 100, Consumed: 10, Remaining: 90, HasAmounts: true,
		CreatedAt: readAt,
	}); err != nil {
		t.Fatalf("save snapshot: %v", err)
	}
}

// fakeReader reads an account's credits without a network.
type fakeReader struct {
	// read are the accounts that were read.
	read []string
	// fail are the accounts whose reading fails.
	fail map[string]bool
	// wait is how long a reading takes.
	wait time.Duration
	// at is when the readings happened.
	at time.Time
	// mu guards read.
	mu sync.Mutex
}

// RefreshUsage reads an account's credits.
func (f *fakeReader) RefreshUsage(ctx context.Context, accountID string) (store.UsageSnapshot, error) {
	if f.wait > 0 {
		select {
		case <-time.After(f.wait):
		case <-ctx.Done():
			return store.UsageSnapshot{}, ctx.Err()
		}
	}
	f.mu.Lock()
	f.read = append(f.read, accountID)
	f.mu.Unlock()
	if f.fail[accountID] {
		return store.UsageSnapshot{}, errors.New("the ledger could not be read")
	}
	return store.UsageSnapshot{
		AccountID: accountID, Granted: 100, Consumed: 10, Remaining: 90,
		HasAmounts: true, CreatedAt: f.at,
	}, nil
}

// TestOneRoundReadsEachAccountOnce says the same account is not read
// twice in one round.
//
// 签到那一趟和热的那一趟可能撞上同一个账号。
func TestOneRoundReadsEachAccountOnce(t *testing.T) {
	r := &Round{Reader: &fakeReader{fail: map[string]bool{}}}
	res, err := r.Run(context.Background(), []string{"acc_1", "acc_1", "acc_2"},
		Options{Concurrency: 2, Timeout: time.Second, Budget: time.Second, Batch: 6})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Refreshed != 2 {
		t.Fatalf("the readings = %d, want two", res.Refreshed)
	}
}

// TestOneRoundKeepsToItsBatch says a round reads at most so many.
//
// 剩下的下一趟接着读：不然一个账号多的网关一轮就把上游问死了。
func TestOneRoundKeepsToItsBatch(t *testing.T) {
	r := &Round{Reader: &fakeReader{fail: map[string]bool{}}}
	res, err := r.Run(context.Background(), []string{"acc_1", "acc_2", "acc_3"},
		Options{Concurrency: 2, Timeout: time.Second, Budget: time.Second, Batch: 2})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Refreshed != 2 {
		t.Fatalf("the readings = %d, want two", res.Refreshed)
	}
	if res.Skipped != 1 {
		t.Fatalf("the skipped = %d, want one", res.Skipped)
	}
}

// TestABatchOfZeroMeansOne says a batch of zero reads one account.
//
// 配 0 的时候按 1 算：0 个一批是一趟也读不了，那不是关掉，是坏了。
func TestABatchOfZeroMeansOne(t *testing.T) {
	r := &Round{Reader: &fakeReader{fail: map[string]bool{}}}
	res, err := r.Run(context.Background(), []string{"acc_1", "acc_2"},
		Options{Concurrency: 1, Timeout: time.Second, Budget: time.Second, Batch: 0})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Refreshed != 1 {
		t.Fatalf("the readings = %d, want one", res.Refreshed)
	}
	if res.Skipped != 1 {
		t.Fatalf("the skipped = %d, want the one the batch left out", res.Skipped)
	}
}

// TestTheBatchOfZeroIsOneInTheControls says the two rounds' controls
// say one account when the config says zero.
//
// 卡片那一趟也是：card: 0 不该是"全都读"，该是"读一个"。
func TestTheBatchOfZeroIsOneInTheControls(t *testing.T) {
	cfg := config.Default()
	cfg.Usage.Refresh.Batch = 0
	cfg.Usage.Refresh.Card = 0
	r := NewRound(nil, nil, nil, cfg)
	if got := r.Options().Batch; got != 1 {
		t.Errorf("the periodic batch = %d, want one", got)
	}
	if got := r.CardOptions().Batch; got != 1 {
		t.Errorf("the card's batch = %d, want one", got)
	}
}

// TestOneRoundKeepsToItsConcurrency says a round reads so many at once.
//
// 并发是读得快，不是读得多：上游不是只给我们用。
func TestOneRoundKeepsToItsConcurrency(t *testing.T) {
	r := &Round{Reader: &fakeReader{fail: map[string]bool{}, wait: 50 * time.Millisecond}}
	res, err := r.Run(context.Background(), []string{"acc_1", "acc_2", "acc_3", "acc_4"},
		Options{Concurrency: 2, Timeout: time.Second, Budget: 5 * time.Second, Batch: 6})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Refreshed != 4 {
		t.Fatalf("the readings = %d, want four", res.Refreshed)
	}
}

// TestOneRoundStopsWhenTheBudgetIsOut says the budget stops the round.
//
// 预算用完就停，剩下的下一趟接着读。
func TestOneRoundStopsWhenTheBudgetIsOut(t *testing.T) {
	r := &Round{Reader: &fakeReader{fail: map[string]bool{}, wait: time.Second}}
	res, err := r.Run(context.Background(), []string{"acc_1", "acc_2"},
		Options{Concurrency: 1, Timeout: time.Second, Budget: 100 * time.Millisecond, Batch: 6})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Refreshed != 0 {
		t.Fatalf("the readings = %d, want none", res.Refreshed)
	}
	if res.Skipped != 1 {
		t.Fatalf("the skipped = %d, want the one the budget left out", res.Skipped)
	}
}

// TestAFailedReadingDoesNotStopTheOthers says one account that cannot
// be read does not stop the rest.
//
// 一个账号读不到不影响别的——但要说出来是哪一个，人才知道去看哪一行。
func TestAFailedReadingDoesNotStopTheOthers(t *testing.T) {
	r := &Round{Reader: &fakeReader{fail: map[string]bool{"acc_1": true}}}
	res, err := r.Run(context.Background(), []string{"acc_1", "acc_2"},
		Options{Concurrency: 2, Timeout: time.Second, Budget: time.Second, Batch: 6})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Refreshed != 1 {
		t.Fatalf("the readings = %d, want one", res.Refreshed)
	}
	if res.Failed != 1 {
		t.Fatalf("the failed = %d, want one", res.Failed)
	}
	if len(res.Reasons) != 1 {
		t.Fatalf("the reasons = %v, want one", res.Reasons)
	}
}

// TestTheReadingSaysWhetherTheAccountHasRoom says a reading says
// whether the account has something to spend.
//
// 账本还有余额，或者计划窗口还有余量，都算还有。
func TestTheReadingSaysWhetherTheAccountHasRoom(t *testing.T) {
	if HasRoom(store.UsageSnapshot{HasAmounts: true, Remaining: 10}) != true {
		t.Error("an account with credits left has no room")
	}
	if HasRoom(store.UsageSnapshot{HasAmounts: true, Remaining: 0}) {
		t.Error("an account without credits has room")
	}
	if HasRoom(store.UsageSnapshot{HasAmounts: false}) {
		t.Error("an account that could not be read has room")
	}
	ws := `[{"name":"5 hours","modelName":"general","amount":10,"limit":100}]`
	if HasRoom(store.UsageSnapshot{WindowsJSON: ws}) != true {
		t.Error("a window with room left has no room")
	}
	full := `[{"name":"5 hours","modelName":"general","amount":100,"limit":100}]`
	if HasRoom(store.UsageSnapshot{WindowsJSON: full}) {
		t.Error("a window that is used up has room")
	}
}

// TestTheReadingClearsTheSpentMark says a reading that says there is
// something to spend takes the "used up" mark off.
//
// 签到给的积分能救活一个用完的账号。不处理的话它会一直待在外面的
// 轮转之外，直到窗口回来（最长七天）—— 白扔一个账号。
func TestTheReadingClearsTheSpentMark(t *testing.T) {
	s := newStore(t)
	now := time.Now()
	mk(t, s, "acc_1", now)
	if err := s.SetExhausted(context.Background(), "acc_1", "general", now.Add(time.Hour), "quota"); err != nil {
		t.Fatalf("set exhausted: %v", err)
	}
	r := NewRound(s, &fakeReader{fail: map[string]bool{}}, nil, config.Default())
	if _, err := r.Settle(context.Background(), "acc_1"); err != nil {
		t.Fatalf("settle: %v", err)
	}
	left, err := s.ListExhausted(context.Background())
	if err != nil {
		t.Fatalf("list exhausted: %v", err)
	}
	if len(left) != 0 {
		t.Fatalf("the spent marks = %v, want none", left)
	}
}

// TestTheClearedSpentMarkIsMadeDurable says the "used up" mark that is
// taken off an account is not in the database when it is taken off.
//
// 签到给的积分能救活一个用完的号，而「用完了」的那个标记是被清掉的。清掉
// 的那笔要落盘：断了电丢了它，这个号会被关在外面最多七天（签到的积分有效
// 期）。
//
// 「真的在盘上」没法在测试里造一次断电来验，但**盘上有什么是能验的**：把
// 库文件和它旁边的 WAL 一起拷一份（那两个文件加起来就是盘上的东西），那个
// 标记得不在里面了。
//
// WAL 也要跟着走 —— 那笔清除是走一条会 fsync 的连接写的：提交把 WAL
// fsync 掉，页没有抄回主库，所以主库里还是清除之前的样子，账在 WAL 上。
// 两个文件一起拷才是盘上的全部。
func TestTheClearedSpentMarkIsMadeDurable(t *testing.T) {
	ctx := context.Background()
	s, path := newStoreWithPath(t)
	now := time.Now()
	mk(t, s, "acc_1", now)
	if err := s.SetExhausted(ctx, "acc_1", "general", now.Add(time.Hour), "quota"); err != nil {
		t.Fatalf("set exhausted: %v", err)
	}
	r := NewRound(s, &fakeReader{fail: map[string]bool{}}, nil, config.Default())
	if _, err := r.Settle(ctx, "acc_1"); err != nil {
		t.Fatalf("settle: %v", err)
	}

	// 库文件和 WAL 一起拷一份。
	copyDir := t.TempDir()
	copyPath := filepath.Join(copyDir, "copy.db")
	for _, suffix := range []string{"", "-wal"} {
		data, err := os.ReadFile(path + suffix)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			t.Fatal(err)
		}
		if err := os.WriteFile(copyPath+suffix, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	db, err := sql.Open("sqlite", copyPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM exhausted_windows").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("the mark is still on the disk: %d rows in the copy", n)
	}
}

// TestAFailedReadingKeepsTheSpentMark says a reading that failed does
// not take the "used up" mark off.
//
// 宁可少用一个账号，也不要把一个用完的账号放回去让客户端撞它。
func TestAFailedReadingKeepsTheSpentMark(t *testing.T) {
	s := newStore(t)
	now := time.Now()
	mk(t, s, "acc_1", now)
	if err := s.SetExhausted(context.Background(), "acc_1", "general", now.Add(time.Hour), "quota"); err != nil {
		t.Fatalf("set exhausted: %v", err)
	}
	r := NewRound(s, &fakeReader{fail: map[string]bool{"acc_1": true}}, nil, config.Default())
	if _, err := r.Settle(context.Background(), "acc_1"); err == nil {
		t.Fatal("settle said nothing about the failed reading")
	}
	left, err := s.ListExhausted(context.Background())
	if err != nil {
		t.Fatalf("list exhausted: %v", err)
	}
	if len(left) != 1 {
		t.Fatalf("the spent marks = %v, want the one", left)
	}
}

// heldReader reads the credits and can be held in the air.
//
// 它能把一次读吊在半空：另一趟就是在这时候撞上来的。它拿的是管理器
// 的那把锁——这一节看的就是管理器跟那一趟的配合。
type heldReader struct {
	// mgr keeps the rounds from reading the same account at once.
	mgr *manager.Manager
	// started says which account a reading is about.
	started chan string
	// release lets the held reading finish.
	release chan struct{}
	// mu guards read.
	mu sync.Mutex
	// read are the accounts that were read.
	read []string
}

// RefreshUsage reads the credits of an account.
func (h *heldReader) RefreshUsage(ctx context.Context, accountID string) (store.UsageSnapshot, error) {
	h.mu.Lock()
	h.read = append(h.read, accountID)
	h.mu.Unlock()
	if h.started != nil {
		h.started <- accountID
	}
	if h.release != nil {
		select {
		case <-h.release:
		case <-ctx.Done():
			return store.UsageSnapshot{}, ctx.Err()
		}
	}
	return store.UsageSnapshot{
		AccountID: accountID, Granted: 100, Consumed: 10, Remaining: 90,
		HasAmounts: true, CreatedAt: time.Now(),
	}, nil
}

// BeginReading keeps two rounds from reading the same account at once.
func (h *heldReader) BeginReading(accountID string) bool {
	return h.mgr.BeginReading(accountID)
}

// EndReading says the reading of an account is done.
func (h *heldReader) EndReading(accountID string) {
	h.mgr.EndReading(accountID)
}

// count says how many readings were asked for.
func (h *heldReader) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.read)
}

// TestTwoRoundsDoNotReadTheSameAccountAtTheSameTime says two rounds
// over the same accounts read one of them once between them.
//
// 签到那一趟和热的那一趟可能撞上同一个账号：同一个账号同一时刻只读
// 一次（规格 §3.3）。撞上的那个账号不是读失败，也不是没轮上：另一趟
// 正在读它。
func TestTwoRoundsDoNotReadTheSameAccountAtTheSameTime(t *testing.T) {
	mgr := manager.New(nil, nil, nil, nil, nil)
	reader := &heldReader{
		mgr:     mgr,
		started: make(chan string, 4),
		release: make(chan struct{}),
	}
	r := &Round{Reader: reader}
	opts := Options{Concurrency: 1, Timeout: 5 * time.Second, Budget: 10 * time.Second, Batch: 6}

	first := make(chan Result, 1)
	go func() {
		res, err := r.Run(context.Background(), []string{"acc_1"}, opts)
		if err != nil {
			t.Errorf("run: %v", err)
		}
		first <- res
	}()
	// 第一趟的那一次读已经在半空了。
	if got := <-reader.started; got != "acc_1" {
		t.Fatalf("the reading is about %q, want the one account", got)
	}

	// 第二趟撞上来了：这个账号正在读，它不读。
	res, err := r.Run(context.Background(), []string{"acc_1"}, opts)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Refreshed != 0 || res.Failed != 0 || res.Skipped != 0 {
		t.Fatalf("the second round = %+v, want nothing read, failed or skipped", res)
	}
	if len(res.Reasons) != 0 {
		t.Fatalf("the second round said %v, want nothing: it is normal", res.Reasons)
	}

	close(reader.release)
	<-first
	if n := reader.count(); n != 1 {
		t.Fatalf("the account was read %d times, want once", n)
	}
}

// TestAReaderWithoutTheGuardIsUnaffected says a reader that cannot keep
// the rounds apart is read as it was.
//
// 没有那把锁的读者：每一趟都读它名单上的账号，规则没有变。
func TestAReaderWithoutTheGuardIsUnaffected(t *testing.T) {
	r := &Round{Reader: &fakeReader{fail: map[string]bool{}}}
	res, err := r.Run(context.Background(), []string{"acc_1"},
		Options{Concurrency: 1, Timeout: time.Second, Budget: time.Second, Batch: 6})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Refreshed != 1 {
		t.Fatalf("the readings = %d, want one", res.Refreshed)
	}
}

// TestTheCheckInFromLongAgoIsNotPending says a check-in from more than a
// week ago does not make an account pending.
//
// 签到给的积分七天就过期了：七天以前的那一次签到，补读它读回来的也是过
// 期数，而卡上摆的旧数跟一个过期的积分没关系。所以这一档只问最近七天
// 的签到（规格 §5.3）。
//
// 这一条是这次改动的行为变化：从前问的是「全部签到」，那样一个八天前
// 签到、之后没读过的号会被一直当成「待补」（规格 §5.3 的规矩）。
func TestTheCheckInFromLongAgoIsNotPending(t *testing.T) {
	s := newStore(t)
	now := time.Now()
	mk(t, s, "acc_old", now.AddDate(0, 0, -9))
	if err := s.SaveCheckin(context.Background(), store.Checkin{
		AccountID: "acc_old", Day: "2026-11-10", Outcome: "claimed",
		Credit: 10, CreatedAt: now.AddDate(0, 0, -8),
	}); err != nil {
		t.Fatalf("save checkin: %v", err)
	}
	r := NewRound(s, nil, func() []string { return nil }, config.Default())
	got, err := r.Targets(context.Background())
	if err != nil {
		t.Fatalf("targets: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("the accounts to read = %v, want none", got)
	}
}
