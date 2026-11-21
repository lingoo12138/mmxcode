// Package pool holds the accounts the gateway keeps in memory.
//
// 池是缓存，不是真相：账号、耗尽标记、会话绑定的真相都在库里。池丢了，
// 重启后从库里重建就行（规格不变量 1）。
//
// 池存在的理由有两个：
//
//   - 选号不该每次都把整张账号表读一遍。1000 个号就是 1000 行 × 每个
//     请求，池把它变成一次内存里的查表 —— 外加一份名单的拷贝（池多大
//     就拷多大：比全表读小几十倍，但不是零）。
//   - 池有上界。库里的号再多，池里也只有「并发 + 会话」那么多个。
//
// 池里放的是选号要用的两样：账号行、剩余积分。计划窗口（windows_json）
// 不放 —— 选号用不上它，放进去每个账号从 400 字节变成 2.2 KB，内存账翻
// 五倍。
//
// 耗尽标记不放：额度用完的号直接出池了（规格 §4.4），池里没有用完的号，
// 也就没有标记要记。标记的真相在库里。
package pool

import (
	"sync"

	"github.com/lingoo12138/mmxcode/internal/store"
)

// BytesPerAccount is what one account in the pool takes, in bytes.
//
// 它是量出来的：账号行（id/name/user_id/email/status 五个字符串）加上
// 剩余积分和 map 的索引，一行 395 字节。这里取 512 —— 宁可高一点，内存
// 预算算得保守些。
//
// 池里不留第二份账号行：名单是每次现给的那一份（见 Accounts）。
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
}

// Pool is the set of accounts the gateway keeps in memory.
//
// 它是并发安全的：聊天那条路读它，后台补号那条路写它。
//
// 池要用 New 造：零值的池没有那张表，往里放东西会 panic。
//
// 号按进池的顺序排：轮转从队首走到队尾，冷号（先进来的）先被挑。
type Pool struct {
	mu   sync.RWMutex
	byID map[string]*Entry
	cap  int
	// order is the accounts in the order they went in.
	//
	// 顺序就是进池的顺序：轮转的时候从队首走到队尾，冷号（先进来的）
	// 先被挑。一个号出池再进池，它排到队尾 —— 它刚用过。
	//
	// 它是给「轮转」用的：map 的顺序是随机的，随机的顺序让轮转不是轮转。
	order []string
}

// New makes a pool that may hold capacity accounts.
//
// A capacity of zero means no limit.
//
// 负数按 0 算 —— 也是「不限」。
func New(capacity int) *Pool {
	if capacity < 0 {
		capacity = 0
	}
	return &Pool{byID: map[string]*Entry{}, cap: capacity}
}

// Capacity is how many accounts the pool may hold.
//
// 0 means no limit.
//
// 它可以比 Len() 小：缩容不请走任何号（见 SetCapacity），池里 40 个、cap
// 10 的时候就是这样 —— 进不去新的了，要等号一个个地被用完。
func (p *Pool) Capacity() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.cap
}

// SetCapacity sets how many accounts the pool may hold.
//
// 0 means no limit. It never takes accounts out: the pool shrinks by
// accounts being spent, not by being told to.
//
// 所以缩容不会请走任何号：池里已经有 40 个的时候调 SetCapacity(10)，池里
// 还是 40 个 —— 但也进不去新的了（满了），要等号一个个地被用完。请出去
// 的理由只有两个：额度用完了（规格 §4.4），还有它签不上了。
//
// 0 = 不限，不管池里现在有几个。
//
// 负数按 0 算 —— 也是「不限」。
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
	p.order = append(p.order, e.Account.ID)
	return true
}

// Remove takes an account out of the pool.
//
// It says whether the account was in the pool.
//
// 一个号出池有两个理由：额度用完了（规格 §4.4），还有它签不上了
// （signed_out，网关那边把它踢出池，见 dropDeadAccount）。别的理由都不出池 ——
// 「一阵子没用」不是理由：会话钉在号上，把它换出去，缓存命中就没了。
func (p *Pool) Remove(accountID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.byID[accountID]; !ok {
		return false
	}
	delete(p.byID, accountID)
	p.dropFromOrder(accountID)
	return true
}

// dropFromOrder takes an account out of the order.
//
// 顺序是进池的顺序：出池的号从顺序里去掉，它再进来的时候排到队尾。
func (p *Pool) dropFromOrder(accountID string) {
	for i, id := range p.order {
		if id != accountID {
			continue
		}
		p.order = append(p.order[:i], p.order[i+1:]...)
		return
	}
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

// Entries gives every entry of the pool, in the order they went in.
//
// 给的是一份新的：拿着它的人可以随便改 —— 池的那一份不会被改坏。
func (p *Pool) Entries() []Entry {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]Entry, 0, len(p.order))
	for _, id := range p.order {
		e, ok := p.byID[id]
		if !ok {
			continue
		}
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
// 能答 = 签入中（status 是 active）。用完的号不在池里：额度用完就出池
// （规格 §4.4）。
func (p *Pool) Usable() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	n := 0
	for _, e := range p.byID {
		if e.Account.Status != store.StatusActive {
			continue
		}
		n++
	}
	return n
}

// Accounts gives the accounts in the pool, in the order they went in.
//
// 给的是一份新的：拿着它的人可以随便改 —— 池的那一份不会被改坏。
//
// **顺序是轮转靠的东西**：rotator 拿着这份名单从队首走到队尾，冷号先被
// 挑。谁改了顺序，轮转就不是轮转了 —— 所以池给出去的每一份都是它自己的
// 副本，改了也不影响池。
func (p *Pool) Accounts() []store.Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]store.Account, 0, len(p.order))
	for _, id := range p.order {
		e, ok := p.byID[id]
		if !ok {
			continue
		}
		out = append(out, e.Account)
	}
	return out
}
