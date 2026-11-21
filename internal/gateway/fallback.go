package gateway

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/lingoo12138/mmxcode/internal/rotator"
	"github.com/lingoo12138/mmxcode/internal/store"
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
	// err is what the last read answered with.
	//
	// 它是给"库从来没答过"的时候准备的：手上没有快照，那就只剩这个错可
	// 以给。没有它，这一秒里来的每一个请求都会去读一遍整张表 —— 库在挂
	// 的时候，读的次数是跟着来的请求走的，不是跟着那一秒走的。
	err error
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
//
// 这一秒里只读一遍，不管有没有读到过：手上没有快照的时候，上一次的那个
// 错就是这一秒里每一个请求的答案 —— 库在挂的时候，读的次数是跟着来的
// 请求走的，不是跟着那一秒走的。
//
// 有一件事不记：被调用方掐断的那一次读。那是那一个请求的事，不是库的事
// —— 记下来就成了这一秒里每一个请求的答案，一个客户端的断开就变成一片
// 503。这样的读不算一次读：下一个请求自己读一遍。
func (f *dbFallback) get(ctx context.Context) (*dbAnswer, error) {
	for {
		f.mu.Lock()
		if time.Since(f.tried) < f.ttl {
			snap := f.snap
			err := f.err
			f.mu.Unlock()
			if snap != nil {
				return snap, nil
			}
			if err != nil {
				return nil, err
			}
			return nil, errors.New("the database did not answer")
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
				// 读的那一头完了。再来一遍：它要么读到了答案（那一趟
				// 盖了戳，答案在这一秒里），要么它自己被掐断了（那一趟
				// 不算数，这一趟自己读）。
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		ch := make(chan struct{})
		f.wait = ch
		f.mu.Unlock()

		// 信号一定要发出去：等的人等的是它。
		//
		// 读的那一头要是 panic 了，这个 defer 照样跑 —— 不然后来的每一个请
		// 求都吊在一个不会有人关的通道上，等到自己的 ctx 死了才走，兜底这一
		// 辈子就废了（进程还在，net/http 会 recover handler 的 panic）。
		defer close(ch)
		// 那个"有人在读"的标记也要放开：同样是那个道理，panic 走的是 defer，
		// 不是代码后面的那几行。
		defer func() {
			f.mu.Lock()
			if f.wait == ch {
				f.wait = nil
			}
			f.mu.Unlock()
		}()

		answer, err := f.read(ctx)

		f.mu.Lock()
		f.wait = nil
		if err != nil && ctx.Err() != nil {
			// 读被掐断了：那是调用方的事，不是库的事。这个错不记（记
			// 下来就是这一秒里每一个请求的答案），这一趟也不算一次读
			// （不盖戳：下一个请求自己读一遍）。
			snap := f.snap
			f.mu.Unlock()
			if snap != nil {
				return snap, nil
			}
			return nil, err
		}
		f.tried = time.Now()
		if err == nil {
			f.snap = answer
			f.err = nil
		} else {
			// 读不到：记住这个错。这一秒里来的请求就拿它走，不必再读一遍整
			// 张表。
			f.err = err
		}
		snap := f.snap
		f.mu.Unlock()

		if err != nil && snap == nil {
			return nil, err
		}
		return snap, nil
	}
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
