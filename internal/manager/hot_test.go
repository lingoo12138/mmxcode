package manager

import (
	"testing"
	"time"
)

// TestTheHotCountForgetsTheOldUses says a use that is out of the
// window is not a use any more.
//
// 热账号是"刚刚"的账：窗口一过就算数。它不落库，所以窗口一过就忘了。
func TestTheHotCountForgetsTheOldUses(t *testing.T) {
	now := time.Now()
	c := newHotCounter()
	c.mark("acc_1", time.Minute, now.Add(-2*time.Minute))
	if got := c.list(time.Minute, now); len(got) != 0 {
		t.Fatalf("the account is still hot: %v", got)
	}
	c.mark("acc_1", time.Minute, now)
	if got := c.list(time.Minute, now); len(got) != 1 || got[0] != "acc_1" {
		t.Fatalf("the hot accounts = %v, want the one", got)
	}
}

// TestTheHotCountCountsTheUses says the account used most comes first.
//
// 热的里面也要有先后：刚用得多的先读。
func TestTheHotCountCountsTheUses(t *testing.T) {
	now := time.Now()
	c := newHotCounter()
	c.mark("acc_1", time.Minute, now)
	c.mark("acc_2", time.Minute, now)
	c.mark("acc_2", time.Minute, now)
	c.mark("acc_3", time.Minute, now)
	got := c.list(time.Minute, now)
	if len(got) != 3 {
		t.Fatalf("the hot accounts = %v, want three", got)
	}
	if got[0] != "acc_2" {
		t.Errorf("the first hot account = %q, want the one used twice", got[0])
	}
}

// TestTheHotCountIsNotBig says the counter does not grow without end.
//
// 账号多了也不会把它撑大：过期的自己清掉，清完还有多就把最久没用的
// 那个丢掉。
func TestTheHotCountIsNotBig(t *testing.T) {
	now := time.Now()
	c := newHotCounter()
	for i := 0; i < maxHotAccounts+50; i++ {
		c.mark(string(rune('a'+i%26))+time.Duration(i).String(), time.Minute, now)
	}
	if got := len(c.list(time.Minute, now)); got > maxHotAccounts {
		t.Fatalf("the counter holds %d accounts, want at most %d", got, maxHotAccounts)
	}
}
