package manager

import (
	"sort"
	"sync"
	"time"
)

// maxHotAccounts is how many accounts the counter keeps.
//
// 两百个账号的用量已经够看了：再多就是记着没人看的账。超过的时候，
// 最久没用的那个被丢掉。
const maxHotAccounts = 200

// hotUse is one account's use in the window.
type hotUse struct {
	// count is how many times it was used without a gap of a whole
	// window.
	//
	// 它是"用得多不多"的粗略数：中间隔了一个整窗口就从头数。热账号
	// 名单要的是"刚用过"，先后的粗略先后就够，不必是精确的滑动窗口。
	count int
	// last is when it was used last.
	last time.Time
}

// hotCounter counts the accounts that were used lately.
//
// 它记的是"刚刚"的账，不是账本：窗口一过，用量就算数，所以它不落库。
type hotCounter struct {
	mu sync.Mutex
	// at holds the accounts that were used.
	at map[string]*hotUse
}

// newHotCounter makes a counter.
func newHotCounter() *hotCounter {
	return &hotCounter{at: map[string]*hotUse{}}
}

// mark says an account was used.
func (c *hotCounter) mark(accountID string, window time.Duration, now time.Time) {
	if accountID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.at[accountID]
	if e == nil {
		e = &hotUse{}
		c.at[accountID] = e
	}
	if !e.last.IsZero() && !e.last.Add(window).After(now) {
		// 上一回太久了：这一回从头数。
		e.count = 0
	}
	e.count++
	e.last = now
	c.prune(window, now)
	c.trim()
}

// prune throws away the accounts whose use is out of the window.
//
// 调用的人拿着锁。
func (c *hotCounter) prune(window time.Duration, now time.Time) {
	for id, e := range c.at {
		if !e.last.Add(window).After(now) {
			delete(c.at, id)
		}
	}
}

// trim keeps the counter from growing without end.
//
// 调用的人拿着锁。
func (c *hotCounter) trim() {
	for len(c.at) > maxHotAccounts {
		oldest := ""
		var oldestAt time.Time
		for id, e := range c.at {
			if oldest == "" || e.last.Before(oldestAt) {
				oldest, oldestAt = id, e.last
			}
		}
		if oldest == "" {
			return
		}
		delete(c.at, oldest)
	}
}

// list gives the accounts that were used in the window, the most used
// first.
func (c *hotCounter) list(window time.Duration, now time.Time) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.prune(window, now)
	out := make([]string, 0, len(c.at))
	for id := range c.at {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := c.at[out[i]], c.at[out[j]]
		if a.count != b.count {
			return a.count > b.count
		}
		if !a.last.Equal(b.last) {
			return a.last.After(b.last)
		}
		// 同样的次数、同样的时候：按 id 分出来，结果才每次一样。
		return out[i] < out[j]
	})
	return out
}
