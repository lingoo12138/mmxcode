package usage

import (
	"context"
	"sort"
	"time"

	"mmxcode/internal/config"
	"mmxcode/internal/store"
)

// Reader reads one account's credits.
//
// 它是一个接口，manager.Manager 正好是这个接口：这一趟读额度的活不
// 需要知道 manager 的别的本事。
type Reader interface {
	// RefreshUsage reads an account's credits and windows and saves them.
	RefreshUsage(ctx context.Context, accountID string) (store.UsageSnapshot, error)
}

// Round reads the credits of the accounts that should be read.
//
// 一趟读额度有四道控：并发、每个账号的时限、一趟的预算、一趟最多读几
// 个。卡住的一个账号不能拖住整趟，一趟也不能读上一百个账号。
type Round struct {
	// Store is the database.
	Store *store.Store
	// Reader reads one account.
	Reader Reader
	// Hot gives the accounts that were used lately, the most used first.
	Hot func() []string
	// Cfg is the configuration.
	Cfg config.Config
}

// NewRound makes a round.
func NewRound(s *store.Store, r Reader, hot func() []string, cfg config.Config) *Round {
	return &Round{Store: s, Reader: r, Hot: hot, Cfg: cfg}
}

// The ranks the accounts are read in.
//
// 排在前面的先读：签到待补的卡上现在是错的，到期的马上要回轮转，从
// 没读过的排在热的前面（它是一次性的活儿，排在后面就永远排在后
// 面），热的刚变过，晚一轮没关系，其余的最久没读的先。
const (
	rankPending = iota
	rankDue
	rankNever
	rankHot
	rankCold
)

// target is one account to read, and where it stands in the order.
type target struct {
	// id is the account.
	id string
	// rank is the group it belongs to.
	rank int
	// order is what breaks a tie inside the rank.
	order int
}

// Targets gives the accounts to read now, in the order they should be
// read.
//
// 该读的 = 从没读过的 ∪ 热的 ∪ 签到待补的 ∪ 标记到期的。
func (r *Round) Targets(ctx context.Context) ([]string, error) {
	accounts, err := r.Store.ListAccounts(ctx)
	if err != nil {
		return nil, err
	}
	snaps, err := r.Store.LatestUsageSnapshots(ctx)
	if err != nil {
		return nil, err
	}
	checkins, err := r.Store.LatestCheckins(ctx)
	if err != nil {
		return nil, err
	}
	exhausted, err := r.Store.ListExhausted(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()

	snapAt := map[string]time.Time{}
	snapHas := map[string]bool{}
	for _, s := range snaps {
		snapAt[s.AccountID] = s.CreatedAt
		snapHas[s.AccountID] = s.HasAmounts
	}
	checkAt := map[string]time.Time{}
	for _, c := range checkins {
		checkAt[c.AccountID] = c.CreatedAt
	}
	// 标记到期：标记的时刻到了，账号回到轮转里，但没有人读过它。
	due := map[string]bool{}
	for _, e := range exhausted {
		if e.Until.After(now) {
			continue
		}
		due[e.AccountID] = true
	}
	hotAt := map[string]int{}
	if r.Hot != nil {
		for i, id := range r.Hot() {
			hotAt[id] = i
		}
	}

	ts := make([]target, 0, len(accounts))
	for _, a := range accounts {
		if a.Status != store.StatusActive {
			// 签出的账号读不到：读了也是白读。
			continue
		}
		read, hasSnap := snapAt[a.ID]
		checked := checkAt[a.ID]
		// 签到待补：签到过了，账本还没读到。
		//
		// 签到是把它放进这个名单的那一件事：签到给了积分，可账本还没
		// 读到（没读到，或者读到的那次失败了），卡上就还是旧的。
		pending := !checked.IsZero() &&
			(!hasSnap || checked.After(read) || !snapHas[a.ID])
		switch {
		case pending:
			ts = append(ts, target{id: a.ID, rank: rankPending})
		case due[a.ID]:
			ts = append(ts, target{id: a.ID, rank: rankDue})
		case !hasSnap:
			// 从没读过的：卡上没有它，而且它永远排在最后就永远读不到。
			ts = append(ts, target{id: a.ID, rank: rankNever})
		default:
			if at, ok := hotAt[a.ID]; ok {
				ts = append(ts, target{id: a.ID, rank: rankHot, order: at})
				continue
			}
			ts = append(ts, target{id: a.ID, rank: rankCold})
		}
	}

	// 其余的最久没读的先：越旧越可能过期。
	cold := []string{}
	for _, t := range ts {
		if t.rank == rankCold {
			cold = append(cold, t.id)
		}
	}
	sort.SliceStable(cold, func(i, j int) bool {
		return snapAt[cold[i]].Before(snapAt[cold[j]])
	})
	coldAt := map[string]int{}
	for i, id := range cold {
		coldAt[id] = i
	}
	for i := range ts {
		if ts[i].rank == rankCold {
			ts[i].order = coldAt[ts[i].id]
		}
	}
	sort.SliceStable(ts, func(i, j int) bool {
		if ts[i].rank != ts[j].rank {
			return ts[i].rank < ts[j].rank
		}
		return ts[i].order < ts[j].order
	})

	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.id)
	}
	return out, nil
}
