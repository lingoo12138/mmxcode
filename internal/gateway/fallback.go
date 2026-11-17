package gateway

import (
	"context"
	"errors"
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
	mu    sync.Mutex
	ttl   time.Duration
	s     poolStore
	tried time.Time
	snap  *dbAnswer
	// wait is the signal of the refresh in the air.
	//
	// 一次只读一遍：等的人等的是这个信号，不是那把锁 —— 锁里不做读库的
	// 活（读库要几毫秒到几十毫秒，锁里读库就是把所有人吊在库上）。
	wait chan struct{}
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
// 它一次只读一遍：读的时候别人等的是那个信号。手上有旧的一份就拿旧的
// 先走 —— 过期也比等着强，客户端自己会超时，而网关不该把它吊在读库上。
//
// 读失败了还是给答案：手上有一份旧的（可能过期）就拿它，没有才报错。
// 一个请求失败比一片 429 强：旧的那份挑出来的号顶多撞一次空，撞空的
// 代价是一个请求。
func (f *dbFallback) get(ctx context.Context) (*dbAnswer, error) {
	f.mu.Lock()
	if f.snap != nil && time.Since(f.tried) < f.ttl {
		snap := f.snap
		f.mu.Unlock()
		return snap, nil
	}
	if f.wait != nil {
		// 有人在读库。
		ch := f.wait
		stale := f.snap
		f.mu.Unlock()
		if stale != nil {
			return stale, nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		f.mu.Lock()
		snap := f.snap
		f.mu.Unlock()
		if snap == nil {
			return nil, errors.New("the database did not answer")
		}
		return snap, nil
	}
	ch := make(chan struct{})
	f.wait = ch
	f.mu.Unlock()

	answer, err := f.read(ctx)

	f.mu.Lock()
	f.wait = nil
	f.tried = time.Now()
	if err == nil {
		f.snap = answer
	}
	snap := f.snap
	f.mu.Unlock()
	close(ch)

	if err != nil && snap == nil {
		return nil, err
	}
	return snap, nil
}

// read asks the database the three things the adapter needs.
//
// 读库的活儿在这里，不在锁里：账号、耗尽标记、余额快照。
func (f *dbFallback) read(ctx context.Context) (*dbAnswer, error) {
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
	return &dbAnswer{accounts: accounts, exhausted: exhausted, remaining: remaining}, nil
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
