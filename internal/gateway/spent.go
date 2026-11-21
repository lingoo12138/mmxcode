package gateway

import (
	"context"
	"encoding/json"
	"log"
	"strings"
	"time"

	"github.com/lingoo12138/mmxcode/internal/account"
	"github.com/lingoo12138/mmxcode/internal/store"
)

// The timeouts of the background work.
//
// 读和写各有一份：读的那一份用完了，不该把写也一起拖掉。
const (
	// spentReadTimeout is how long the background reading may take.
	spentReadTimeout = 30 * time.Second
	// spentWriteTimeout is how long the background writing may take.
	spentWriteTimeout = 5 * time.Second
)

// resetAfter gives the moment a window comes back, when it is in the
// future.
//
// 过去的时刻当没有：拿一个已经过去的时间去说"它三点回来"，说的是一
// 个已经过去的时刻，而 isExhausted 看到过去的时刻就当没耗尽 —— 于是
// 这个账号立刻又回到轮转里，客户端下一次还是撞上它。没有就是没有，
// 让它走冷却时间。
func resetAfter(at time.Time) time.Time {
	if at.IsZero() || !at.After(time.Now()) {
		return time.Time{}
	}
	return at
}

// windowReset gives the moment a spent window comes back.
//
// The key is the plan group, not one window: the windows "5 hours" and
// "7 days" belong to a group, and the group comes back as a whole.
//
// A time that has passed is no time: the caller falls back to the
// cooldown.
func (s *Server) windowReset(ctx context.Context, accountID, groupKey string) time.Time {
	ws, err := s.Mgr.LatestWindows(ctx, accountID)
	if err != nil {
		return time.Time{}
	}
	return resetOf(ws, groupKey)
}

// resetOf gives the group's reset out of a list of windows.
//
// 一个组是好几个窗口（"5 hours"、"7 days"），组是整个回来的：它最后
// 那个窗口回来了，组才回来。所以组的时刻取它那些窗口里最晚的一个，
// 而且只算将来的 —— 已经过去的那个不算：它不能盖住一个还在将来的窗
// 口。
//
// 组的键比的时候不看大小写：站上写的是 "General"，这边说的是
// "general"，说的是同一个组。
func resetOf(ws []account.Window, groupKey string) time.Time {
	var latest time.Time
	for _, w := range ws {
		if !strings.EqualFold(w.ModelName, groupKey) {
			continue
		}
		at := resetAfter(w.ResetsAt)
		if at.After(latest) {
			latest = at
		}
	}
	return latest
}

// resetOfSnapshot gives the group's reset out of a reading.
//
// 它是后台那一次读用的：读到的数才是真的，标记要照着它改。
func resetOfSnapshot(snap store.UsageSnapshot, groupKey string) time.Time {
	if snap.WindowsJSON == "" {
		return time.Time{}
	}
	var ws []account.Window
	if err := json.Unmarshal([]byte(snap.WindowsJSON), &ws); err != nil {
		return time.Time{}
	}
	return resetOf(ws, groupKey)
}

// markSpent marks an account's group as spent, and reads it once more
// in the background.
//
// ctx 是客户端那个请求的 context：它多半已经被取消了（客户端走了，或
// 者上游的超时到了）。标记的写不用它 —— 标记是网关自己的活：写在客
// 户端那个 context 上，它就跟着客户端一起没了，而库里没有这一行，账
// 号就继续留在轮转里，下一个客户端照样撞上"用完"（规格 §4.1）。
//
// 读（问库里存着的窗口时刻）先用客户端那个 context。它死了就用网关自
// 己的命再问一次：那一刻定下来的"什么时候回来"不该因为客户端走了就
// 短成冷却时间。
//
// 标记的那一刻就把"什么时候回来"定下来：读到的窗口时刻在将来就用
// 它，取不到就用冷却时间。后台那一次读是来改标记的，而且只往后改：
// 读到的时刻比现在的标记晚，标记就挪到那儿；读不到，或者读到的东西
// 里没有这个组，标记就原样留着。
//
// 提前放一个用完的账号回轮转，正是这个标记要防的事。而一个滚动的窗
// 口，它的时刻本来也只是越来越晚：所以"只往后改"平常的时候就是"照
// 着真的时刻改"。
func (s *Server) markSpent(ctx context.Context, accountID, groupKey string) {
	until := s.windowReset(ctx, accountID, groupKey)
	if until.IsZero() && ctx.Err() != nil {
		// 客户端走了：这一次读没有读到。网关自己的命还在，用它再问
		// 一次 —— 读到的时刻不该因为客户端走了就短成冷却时间。
		rctx, rcancel := context.WithTimeout(s.lifeCtx, spentReadTimeout)
		until = s.windowReset(rctx, accountID, groupKey)
		rcancel()
	}
	// 写用网关自己的命：客户端断开、上游超时，都不该让这一行丢掉。
	wctx, wcancel := context.WithTimeout(s.lifeCtx, spentWriteTimeout)
	defer wcancel()
	if err := s.Rot.MarkWindowExhausted(wctx, accountID, groupKey, until, "quota"); err != nil {
		// 写不进去要说得出来：不然这一行没了，谁也不知道，下一个客
		// 户端照样撞上"用完"。
		log.Printf("the spent mark of %s (%s) could not be written: %v", accountID, groupKey, err)
	}
	// 后台的活儿有上限：到顶了这一次就不读，标记照着上面写的那份留
	// 着（冷却时间），下一次再说。
	s.startBackground(func() { s.correctMark(accountID, groupKey) })
	// 池里少了一个号：补一个进去。补号是后台的活，不挡客户端（规格
	// §4.4）。
	s.startBackground(func() { s.TopUpPool(s.lifeCtx) })
}

// correctMark reads the account and puts the mark's time right.
//
// 它不挡客户端：后台的活。
//
// 读到的时刻比现在的标记晚，标记就挪到那儿；不然标记原样留着。读到的
// 东西里没有这个组（或者读失败了），那就没有时刻：没有时刻就不动。
//
// 库里没有标记的时候它也不动：标记是被清掉的（签到给了积分），这一趟
// 读是清掉它之前起的，它不许把标记放回来。
//
// 提前放一个用完的账号回轮转，正是这个标记要防的事。
func (s *Server) correctMark(accountID, groupKey string) {
	ctx, cancel := context.WithTimeout(s.lifeCtx, spentReadTimeout)
	defer cancel()
	snap, err := s.Reader.RefreshUsage(ctx, accountID)
	if err != nil {
		// 读不到：标记原样留着。
		return
	}
	until := resetOfSnapshot(snap, groupKey)
	if until.IsZero() {
		// 这一次读到的东西里没有这个组：没有就是没有。写一个零进去，
		// 冷却时间就会把标记缩短。
		return
	}
	// 写的时限另起一份：读的那一份用完了，写也该写得下。
	wctx, wcancel := context.WithTimeout(s.lifeCtx, spentWriteTimeout)
	defer wcancel()
	cur, found := s.markedUntil(wctx, accountID, groupKey)
	if !found {
		// 库里没有这个标记：它是被清掉的（签到给了积分，读到的数说有额
		// 度，规格 §5.2）。后台这一趟读是清掉它之前起的，它不许把标记
		// 放回来 —— 一个有积分的账号被关在外面，最多关七天。
		//
		// 读不到标记也是这一句：不知道就不动。
		return
	}
	if cur.After(until) {
		// 现在的标记比读到的时刻还晚：标记不动。标记只往后挪。
		return
	}
	_ = s.Rot.MarkWindowExhausted(wctx, accountID, groupKey, until, "quota")
}

// markedUntil gives the moment the account's group is marked spent
// until, and whether there is such a mark at all.
//
// 库里那一行说了算：标记现在到什么时候，是写下去的那一份。读不到就说
// "没有"：不知道就不动。
//
// 「没有标记」和「标记到零时刻」是两件事：前者是被清掉的（签到给了积
// 分），后台那一趟读不许把它放回来；后者是标记在那儿，只是没有时刻。
func (s *Server) markedUntil(ctx context.Context, accountID, groupKey string) (time.Time, bool) {
	rows, err := s.Store.ListExhausted(ctx)
	if err != nil {
		return time.Time{}, false
	}
	var until time.Time
	found := false
	for _, e := range rows {
		if e.AccountID != accountID || e.WindowKey != groupKey {
			continue
		}
		if !found || e.Until.After(until) {
			until = e.Until
			found = true
		}
	}
	return until, found
}
