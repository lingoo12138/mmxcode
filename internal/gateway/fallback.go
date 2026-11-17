package gateway

import (
	"context"
	"sync"
	"time"

	"mmxcode/internal/rotator"
	"mmxcode/internal/store"
)

// dbFallback is the database's answer for the rotator, taken while
// the pool is empty.
//
// 池空的时候适配器回到从前的样子，从库里拿账号、耗尽标记和余额。那三样
// 都是全表读：1000 个号的账号表 + 标记表 + 全部最新快照（连 windows_json
// 一起），一个请求一次就是 2.6MB 的垃圾，几十个并发就是一波几百 MB。
//
// 所以这里留一份缓存，一秒一份。这一阵子的答案是一个样的 ——
// concurrencyLimit 用的就是同一个道理。缓存让那一波并发只读一次。
//
// 它会过期一秒。这一秒里刚被标记成「用完」的号还看得出能用：请求会挑
// 中它，撞一次空，换号重试。撞一次空的代价是一个请求；不缓存的代价是
// 每一波并发都把整张表读一遍。前者便宜得多。
type dbFallback struct {
	mu   sync.Mutex
	ttl  time.Duration
	s    poolStore
	at   time.Time
	snap *dbAnswer
}

// dbAnswer is one answer from the database.
type dbAnswer struct {
	accounts  []store.Account
	exhausted []store.Exhausted
	remaining map[string]rotator.Remaining
}

// newDBFallback makes the cache.
func newDBFallback(s poolStore, ttl time.Duration) *dbFallback {
	if ttl <= 0 {
		ttl = time.Second
	}
	return &dbFallback{s: s, ttl: ttl}
}

// get gives the snapshot, taking a new one when the old is stale.
//
// 它一次只让一个取：等的时候拿着锁（活儿很便宜，几毫秒）。
func (f *dbFallback) get(ctx context.Context) (*dbAnswer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.snap != nil && time.Since(f.at) < f.ttl {
		return f.snap, nil
	}
	accounts, err := f.s.ListAccounts(ctx)
	if err != nil {
		return nil, err
	}
	exhausted, err := f.s.ListExhausted(ctx)
	if err != nil {
		return nil, err
	}
	snaps, err := f.s.LatestUsageSnapshots(ctx)
	if err != nil {
		return nil, err
	}
	remaining := make(map[string]rotator.Remaining, len(snaps))
	for _, s := range snaps {
		if !s.HasAmounts {
			// 没读到余额的账号不算有余额：留空，让 rotator 把它排在后面
			//（规格不变量 2）。
			continue
		}
		remaining[s.AccountID] = rotator.Remaining{Left: s.Remaining, Known: true}
	}
	f.snap = &dbAnswer{accounts: accounts, exhausted: exhausted, remaining: remaining}
	f.at = time.Now()
	return f.snap, nil
}

// accounts gives the accounts of the snapshot.
func (f *dbFallback) accounts(ctx context.Context) ([]store.Account, error) {
	snap, err := f.get(ctx)
	if err != nil {
		return nil, err
	}
	return snap.accounts, nil
}

// exhausted gives the exhaustion marks of the snapshot.
func (f *dbFallback) exhausted(ctx context.Context) ([]store.Exhausted, error) {
	snap, err := f.get(ctx)
	if err != nil {
		return nil, err
	}
	return snap.exhausted, nil
}

// remaining gives the credits of the snapshot, for the accounts asked for.
//
// 留空的是「没读到余额」的账号：rotator 把它们排在有数的号后面（规格
// 不变量 2）。
func (f *dbFallback) remaining(ctx context.Context, ids []string) map[string]rotator.Remaining {
	snap, err := f.get(ctx)
	if err != nil {
		return map[string]rotator.Remaining{}
	}
	out := make(map[string]rotator.Remaining, len(ids))
	for _, id := range ids {
		if rem, ok := snap.remaining[id]; ok {
			out[id] = rem
		}
	}
	return out
}
