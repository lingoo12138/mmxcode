package pool

import (
	"context"
	"errors"
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
}

func (f *fakeMarker) SetExhausted(_ context.Context, accountID, windowKey string, until time.Time, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.marks = append(f.marks, store.Exhausted{
		AccountID: accountID, WindowKey: windowKey, Until: until, Reason: reason,
	})
	return nil
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
	k := NewKeeper(p, src, reader, marker, true, time.Minute)

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
	k := NewKeeper(p, src, reader, marker, true, time.Minute)

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
	k := NewKeeper(p, src, reader, marker, true, time.Minute)

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
	k := NewKeeper(p, src, reader, marker, true, time.Minute)

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
	k := NewKeeper(p, src, reader, marker, true, time.Minute)

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
	k := NewKeeper(p, src, reader, marker, true, time.Minute)

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
