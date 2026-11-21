package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/lingoo12138/mmxcode/internal/account"
	"github.com/lingoo12138/mmxcode/internal/store"
)

// fakeReader reads the credits without the network.
//
// 网关读额度只有一个口子：Reader 那个字段。测试把它换成这个，测试就
// 永远不碰 MiniMax 自己。
type fakeReader struct {
	mu sync.Mutex
	// snap is what a reading gives back.
	snap store.UsageSnapshot
	// err is what a reading fails with.
	err error
	// reads counts the readings that were asked for.
	reads int
	// asked are the accounts the readings were asked for.
	asked []string
}

// set gives the reader what it should say.
func (f *fakeReader) set(snap store.UsageSnapshot, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snap = snap
	f.err = err
}

// RefreshUsage reads the credits from the test's own answers.
func (f *fakeReader) RefreshUsage(ctx context.Context, accountID string) (store.UsageSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	f.asked = append(f.asked, accountID)
	if f.err != nil {
		return store.UsageSnapshot{}, f.err
	}
	return f.snap, nil
}

// count says how many readings were asked for.
func (f *fakeReader) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

// askedFor says which accounts the readings were asked for.
//
// 它记下问的是谁：光数次数说不清是哪些账号被问过。
func (f *fakeReader) askedFor() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.asked...)
}

// readingOf makes a reading that says the windows it has.
func readingOf(t *testing.T, ws []account.Window) store.UsageSnapshot {
	t.Helper()
	raw, err := json.Marshal(ws)
	if err != nil {
		t.Fatal(err)
	}
	return store.UsageSnapshot{WindowsJSON: string(raw)}
}

// markUntil gives the moment the account's group is marked spent until.
//
// 库里那一行说了算：标记现在到什么时候，是写下去的那一份。
func markUntil(t *testing.T, s *store.Store, accountID, groupKey string) time.Time {
	t.Helper()
	rows, err := s.ListExhausted(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range rows {
		if e.AccountID == accountID && e.WindowKey == groupKey {
			return e.Until
		}
	}
	return time.Time{}
}

// TestAResetInThePastIsNoReset says a window that came back already is
// not a time to come back to.
//
// 过去的时刻照写不误有个坏处：isExhausted 看到过去的时刻就当"没耗
// 尽"，于是这个账号立刻又回到轮转里，客户端下一次还是撞上它。
func TestAResetInThePastIsNoReset(t *testing.T) {
	now := time.Now()
	if got := resetAfter(time.Now().Add(-time.Hour)); !got.IsZero() {
		t.Errorf("the reset = %s, want none", got)
	}
	if got := resetAfter(now.Add(time.Hour)); !got.After(now) {
		t.Errorf("the reset = %s, want a time in the future", got)
	}
}

// TestTheGroupComesBackWithItsLatestWindow says a group comes back as a
// whole: its moment is the latest of the windows it still has to come
// back from.
//
// 一个组是好几个窗口（"5 hours"、"7 days"），组是整个回来的：它最后
// 那个窗口回来了，组才回来。已经过去的那个窗口不算：它不能盖住一个
// 还在将来的窗口。
func TestTheGroupComesBackWithItsLatestWindow(t *testing.T) {
	now := time.Now()
	// 一个过去的，一个将来的：将来的是那个时刻。
	ws := []account.Window{
		{ModelName: "general", ResetsAt: now.Add(-time.Hour)},
		{ModelName: "general", ResetsAt: now.Add(time.Hour)},
	}
	if got := resetOf(ws, "general"); !got.Equal(now.Add(time.Hour)) {
		t.Errorf("the group's reset = %s, want %s", got, now.Add(time.Hour))
	}
	// 两个将来的：晚的那个是那个时刻。
	ws = []account.Window{
		{ModelName: "general", ResetsAt: now.Add(time.Hour)},
		{ModelName: "general", ResetsAt: now.Add(3 * time.Hour)},
	}
	if got := resetOf(ws, "general"); !got.Equal(now.Add(3 * time.Hour)) {
		t.Errorf("the group's reset = %s, want %s", got, now.Add(3*time.Hour))
	}
	// 组的键比的时候不看大小写：站上写的是 "General"，这边说的是
	// "general"，说的是同一个组。
	ws = []account.Window{{ModelName: "General", ResetsAt: now.Add(2 * time.Hour)}}
	if got := resetOf(ws, "general"); !got.Equal(now.Add(2 * time.Hour)) {
		t.Errorf("the group's reset = %s, want %s", got, now.Add(2*time.Hour))
	}
}

// TestTheMarkIsWrittenWhenTheClientIsGone says the mark is written
// even when the client goes away.
//
// 上游说"额度用完"的那一刻就把标记写下去（规格 §4.1）。那一刻客户端
// 可能刚好断开：标记写在客户端那个请求的 context 上，它就跟着那个
// context 一起没了 —— 库里没有这一行，账号继续留在轮转里，下一个客
// 户端照样撞上它。
func TestTheMarkIsWrittenWhenTheClientIsGone(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	// 客户端走了：请求的 context 被取消了。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.gw.markSpent(ctx, "acc_01", "general")
	ex, err := h.store.ListExhausted(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ex) != 1 || ex[0].AccountID != "acc_01" {
		t.Fatalf("exhausted = %+v, want the mark", ex)
	}
	if ex[0].WindowKey != "general" {
		t.Fatalf("window key = %q, want the group", ex[0].WindowKey)
	}
}

// TestTheBackgroundReadingDoesNotShortenTheMark says a reading that
// does not mention the group leaves the mark as it is.
//
// 标记是三小时以后：读到的数里没有这个组，那就没有。没有就是没有，写
// 一个零进去，冷却时间（五分钟）就会把标记缩短，一个还欠着额度的账号
// 就提前回到轮转里 —— 提前放行正是这个标记要防的事。
func TestTheBackgroundReadingDoesNotShortenTheMark(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()
	until := time.Now().Add(3 * time.Hour)
	if err := h.gw.Rot.MarkWindowExhausted(ctx, "acc_01", "general", until, "quota"); err != nil {
		t.Fatal(err)
	}
	// 这一次读到的东西里没有这个组。
	h.reader.set(readingOf(t, []account.Window{{ModelName: "video", ResetsAt: time.Now().Add(time.Hour)}}), nil)
	h.gw.correctMark("acc_01", "general")
	got := markUntil(t, h.store, "acc_01", "general")
	if got.UnixMilli() != until.UnixMilli() {
		t.Errorf("the mark = %s, want %s", got, until)
	}
}

// TestTheBackgroundReadingDoesNotShortenTheMarkWithAnEarlierMoment says
// a reading that says an earlier moment leaves the mark as it is.
//
// 标记是三小时以后，读到的时刻是一小时以后：标记不动。只往后挪。
func TestTheBackgroundReadingDoesNotShortenTheMarkWithAnEarlierMoment(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()
	until := time.Now().Add(3 * time.Hour)
	if err := h.gw.Rot.MarkWindowExhausted(ctx, "acc_01", "general", until, "quota"); err != nil {
		t.Fatal(err)
	}
	h.reader.set(readingOf(t, []account.Window{{ModelName: "general", ResetsAt: time.Now().Add(time.Hour)}}), nil)
	h.gw.correctMark("acc_01", "general")
	got := markUntil(t, h.store, "acc_01", "general")
	if got.UnixMilli() != until.UnixMilli() {
		t.Errorf("the mark = %s, want %s", got, until)
	}
}

// TestTheBackgroundReadingMovesTheMarkLater says a reading that says a
// later moment moves the mark there.
//
// 标记是三小时以后，读到的时刻是五小时以后：标记挪到五小时以后。一个
// 滚动的窗口它的时刻本来就是越来越晚的，所以平常的时候这就是"照着真
// 的时刻改"。
func TestTheBackgroundReadingMovesTheMarkLater(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()
	if err := h.gw.Rot.MarkWindowExhausted(ctx, "acc_01", "general", time.Now().Add(3*time.Hour), "quota"); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(5 * time.Hour)
	h.reader.set(readingOf(t, []account.Window{{ModelName: "general", ResetsAt: later}}), nil)
	h.gw.correctMark("acc_01", "general")
	got := markUntil(t, h.store, "acc_01", "general")
	if got.UnixMilli() != later.UnixMilli() {
		t.Errorf("the mark = %s, want %s", got, later)
	}
}

// TestTheBackgroundReadingThatFailsLeavesTheMarkAlone says a reading
// that fails leaves the mark as it is.
//
// 读不到就不知道什么时候回来：不知道就不动。动一下就有可能提前放行。
func TestTheBackgroundReadingThatFailsLeavesTheMarkAlone(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()
	until := time.Now().Add(3 * time.Hour)
	if err := h.gw.Rot.MarkWindowExhausted(ctx, "acc_01", "general", until, "quota"); err != nil {
		t.Fatal(err)
	}
	h.reader.set(store.UsageSnapshot{}, errors.New("the site is not there"))
	h.gw.correctMark("acc_01", "general")
	got := markUntil(t, h.store, "acc_01", "general")
	if got.UnixMilli() != until.UnixMilli() {
		t.Errorf("the mark = %s, want %s", got, until)
	}
}

// TestTheBackgroundReadingGoesThroughTheReader says the background
// reading goes through the injected reader and not to the network.
//
// 网关读额度只有一个口子：Reader 那个字段。这个账号没有令牌，所以管
// 理器那条路读不到它 —— 读额度就只可能走那个口子。
func TestTheBackgroundReadingGoesThroughTheReader(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()
	now := time.Now()
	h.store.CreateAccount(ctx, store.Account{
		ID: "acc_02", Name: "b", Status: store.StatusActive, UserID: "0987654321",
		CreatedAt: now, UpdatedAt: now,
	})
	if err := h.gw.Rot.MarkWindowExhausted(ctx, "acc_02", "general", time.Now().Add(3*time.Hour), "quota"); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(5 * time.Hour)
	h.reader.set(readingOf(t, []account.Window{{ModelName: "general", ResetsAt: later}}), nil)
	h.gw.correctMark("acc_02", "general")
	if n := h.reader.count(); n != 1 {
		t.Errorf("the reader was asked %d times, want 1", n)
	}
	got := markUntil(t, h.store, "acc_02", "general")
	if got.UnixMilli() != later.UnixMilli() {
		t.Errorf("the mark = %s, want %s", got, later)
	}
}

// TestTheMarkAndNoMarkAreNotTheSame says a mark that is there and no
// mark at all are told apart.
//
// 「没有标记」和「标记到零时刻」是两件事：前者是被清掉的（签到给了积
// 分），后者是标记在那儿、只是没有时刻。后台那一趟读不许把清掉的标记
// 又写回去。
func TestTheMarkAndNoMarkAreNotTheSame(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()
	if _, found := h.gw.markedUntil(ctx, "acc_01", "general"); found {
		t.Error("there is a mark, want none")
	}
	// 一个到零时刻的标记：它在，只是没有时刻。
	if err := h.store.SetExhausted(ctx, "acc_01", "general", time.Time{}, "quota"); err != nil {
		t.Fatal(err)
	}
	until, found := h.gw.markedUntil(ctx, "acc_01", "general")
	if !found {
		t.Error("the mark is not found, want it")
	}
	if !until.IsZero() {
		t.Errorf("the mark = %s, want the zero time", until)
	}
}

// TestTheBackgroundReadingDoesNotPutBackAClearedMark says a background
// reading does not put a mark back that is not in the database.
//
// 上游拒了它 → 标记 → 后台去读。读的那一趟还没回来，签到给了积分，读到
// 的数说有额度 → 标记被清掉 → 账号回到轮转里。后台那一趟这时候才回来，
// 它把标记又写回去：这个有积分的账号就白扔了 —— 最多扔七天。
func TestTheBackgroundReadingDoesNotPutBackAClearedMark(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	// 后台那一次读：读到的是七天以后回来的时刻。
	h.reader.set(readingOf(t, []account.Window{
		{ModelName: "general", ResetsAt: time.Now().Add(7 * 24 * time.Hour)},
	}), nil)
	// 库里没有标记了：签到给了积分，标记已经被清掉。
	h.gw.correctMark("acc_01", "general")
	got := markUntil(t, h.store, "acc_01", "general")
	if !got.IsZero() {
		t.Errorf("the mark = %s, want none: it was cleared on purpose", got)
	}
}
