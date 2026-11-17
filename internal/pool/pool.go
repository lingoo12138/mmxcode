// Package pool holds the accounts the gateway keeps in memory.
//
// 池是缓存，不是真相：账号、耗尽标记、会话绑定的真相都在库里。池丢了，
// 重启后从库里重建就行（规格不变量 1）。
//
// 池存在的理由有两个：
//
//   - 选号不该每次都把整张账号表读一遍。1000 个号就是 1000 行 × 每个
//     请求，池把它变成一次内存里的查表。
//   - 池有上界。库里的号再多，池里也只有「并发 + 会话」那么多个。
//
// 池里放的是选号要用的三样：账号行、剩余积分、耗尽标记。计划窗口
// （windows_json）不放 —— 选号用不上它，放进去每个账号从 400 字节变成
// 2.2 KB，内存账翻五倍。
package pool

import (
	"sync"
	"time"

	"mmxcode/internal/store"
)

// BytesPerAccount is what one account in the pool takes, in bytes.
//
// 它是量出来的：账号行（id/name/user_id/email/status 五个字符串）加上
// 剩余积分、耗尽标记和 map 的索引，一行 395 字节。这里取 512 —— 宁可
// 高一点，内存预算算得保守些。
const BytesPerAccount = 512

// Entry is one account in the pool.
type Entry struct {
	// Account is the account itself.
	Account store.Account
	// Remaining is how many credits are left. It says something only
	// when Known is true.
	Remaining float64
	// Known says the credits have been read.
	//
	// 它是 false 的时候 Remaining 说不了话：这个号还没被读过。不是 0
	// —— 0 是「读到了，没有了」。
	Known bool
	// Spent says the account's window is spent.
	Spent bool
	// SpentUntil is when the window comes back.
	SpentUntil time.Time
	// SpentGroup is the window group that is spent.
	SpentGroup string
}

// Pool is the set of accounts the gateway keeps in memory.
//
// 它是并发安全的：聊天那条路读它，后台补号那条路写它。
//
// 账号名单和用完的名单各有一份现成的快照：挑号每个请求都要问一次，问
// 的时候不该现凑一份。快照是整份换掉的，不是就地改的 —— 拿着它的人看
// 到的一直是完整的一份。
type Pool struct {
	mu   sync.RWMutex
	byID map[string]*Entry
	cap  int
	// accounts is the snapshot of the accounts in the pool.
	accounts []store.Account
	// spent is the snapshot of the accounts whose window is spent.
	spent []store.Exhausted
}

// New makes a pool that may hold capacity accounts.
//
// A capacity of zero means no limit.
func New(capacity int) *Pool {
	if capacity < 0 {
		capacity = 0
	}
	return &Pool{byID: map[string]*Entry{}, cap: capacity}
}

// Capacity is how many accounts the pool may hold.
//
// 0 means no limit.
func (p *Pool) Capacity() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.cap
}

// SetCapacity sets how many accounts the pool may hold.
//
// 0 means no limit. It never takes accounts out: the pool shrinks by
// accounts being spent, not by being told to.
func (p *Pool) SetCapacity(n int) {
	if n < 0 {
		n = 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cap = n
}

// Len is how many accounts the pool holds.
func (p *Pool) Len() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.byID)
}

// Full says the pool cannot take one more account.
func (p *Pool) Full() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.cap > 0 && len(p.byID) >= p.cap
}

// Admit puts an account into the pool.
//
// It says whether the account went in. It does not go in when the pool
// is full, or when it is already in.
//
// 一个已经在池里的号不会被换掉：池里的号带着它读到的积分，换掉它就是把
// 刚读到的数扔了。
func (p *Pool) Admit(e Entry) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cap > 0 && len(p.byID) >= p.cap {
		return false
	}
	if _, ok := p.byID[e.Account.ID]; ok {
		return false
	}
	entry := e
	p.byID[e.Account.ID] = &entry
	p.rebuild()
	return true
}

// Remove takes an account out of the pool.
//
// It says whether the account was in the pool.
//
// 一个号出池只有一个理由：额度用完了（规格 §4.4）。别的理由都不出池 ——
// 「一阵子没用」不是理由：会话钉在号上，把它换出去，缓存命中就没了。
func (p *Pool) Remove(accountID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.byID[accountID]; !ok {
		return false
	}
	delete(p.byID, accountID)
	p.rebuild()
	return true
}

// Get gives one entry of the pool.
func (p *Pool) Get(accountID string) (Entry, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byID[accountID]
	if !ok {
		return Entry{}, false
	}
	return *e, true
}

// Entries gives every entry of the pool.
func (p *Pool) Entries() []Entry {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]Entry, 0, len(p.byID))
	for _, e := range p.byID {
		out = append(out, *e)
	}
	return out
}

// IDs gives the ids of the accounts in the pool.
func (p *Pool) IDs() map[string]bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make(map[string]bool, len(p.byID))
	for id := range p.byID {
		out[id] = true
	}
	return out
}

// MarkSpent marks an account's window spent.
//
// 标记的是「这个窗口用完了，什么时候回来」：池里的号照旧留着，只是它
// 不再能被挑到 —— 挑号的那一套会看 Spent 和 SpentUntil。
func (p *Pool) MarkSpent(accountID, windowKey string, until time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byID[accountID]
	if !ok {
		return
	}
	e.Spent = true
	e.SpentGroup = windowKey
	e.SpentUntil = until
	p.rebuild()
}

// SetRemaining writes the credits of an account that is in the pool.
//
// 一个不在池里的号没有地方可写：它不是池的成员，它的数跟挑号无关。
func (p *Pool) SetRemaining(accountID string, left float64, known bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byID[accountID]
	if !ok {
		return
	}
	e.Remaining = left
	e.Known = known
}

// Usable is how many accounts in the pool can answer right now.
//
// 能答 = 签入中（status 是 active）而且它的窗口没有用完。窗口的时刻已
// 经过去的标记不算用完 —— 那个号回来了（规格 §4.5）。
func (p *Pool) Usable(now time.Time) int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	n := 0
	for _, e := range p.byID {
		if e.Account.Status != store.StatusActive {
			continue
		}
		if e.Spent && e.SpentUntil.After(now) {
			continue
		}
		n++
	}
	return n
}

// Accounts gives the accounts in the pool.
//
// 给的是现成的那一份：挑号每个请求都要问一次，问的时候不该现凑一份。
// 那一份是整份换掉的，不是就地改的 —— 拿着它的人看到的一直是完整的一
// 份。
func (p *Pool) Accounts() []store.Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.accounts
}

// Spent gives the accounts in the pool whose window is spent.
func (p *Pool) Spent() []store.Exhausted {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.spent
}

// rebuild makes the snapshots again.
//
// 它在写锁底下跑：换的是整份，不是就地改。
func (p *Pool) rebuild() {
	accounts := make([]store.Account, 0, len(p.byID))
	spent := make([]store.Exhausted, 0, len(p.byID))
	for _, e := range p.byID {
		accounts = append(accounts, e.Account)
		if !e.Spent {
			continue
		}
		spent = append(spent, store.Exhausted{
			AccountID: e.Account.ID,
			WindowKey: e.SpentGroup,
			Until:     e.SpentUntil,
			Reason:    "quota",
		})
	}
	p.accounts = accounts
	p.spent = spent
}
