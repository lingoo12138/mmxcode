// Package usage sums the credits of the gateway's accounts.
//
// The numbers come from MiniMax's own credit ledger, batch by batch:
// what each batch gave, what of it was used, and what is left. Only
// batches that have not expired count, and an account whose ledger
// could not be read is left out of the totals rather than counted as
// zero.
//
// The three states are kept apart: an account whose credits were read,
// an account whose credits have run out, and an account whose credits
// could not be read. A total that is short must be explainable.
package usage

import (
	"sort"
	"time"

	"github.com/lingoo12138/mmxcode/internal/store"
)

// The states an account's credits can be in.
//
// They are told apart, because they mean different things: credits that
// were read are counted, credits that have run out are counted as
// nothing, and credits that could not be read are not counted at all.
const (
	// StateRead means the credits were read and some of them are live.
	StateRead = "read"
	// StateExpired means the credits were read and all of them have
	// run out.
	StateExpired = "expired"
	// StateUnavailable means the credits could not be read.
	StateUnavailable = "unavailable"
	// StateUnread means the account's credits were never read: the
	// account is in the database, but no reading of it has been made
	// yet.
	//
	// 没读过跟读不到是两回事：读不到是这一次失败了，没读过是从头
	// 就没读过。两个都不算数——它们不是零，是不知道——但人要知道
	// 是哪一种：读不到要去看看出了什么事，没读过只是还没轮到。
	StateUnread = "unread"
)

// AccountUsage is one account's credits.
type AccountUsage struct {
	// ID is the account's id.
	ID string `json:"id"`
	// Email is the account's email, when it is known.
	Email string `json:"email,omitempty"`
	// Granted is what the account's live batches gave.
	Granted *float64 `json:"granted"`
	// Consumed is what of it was used.
	Consumed *float64 `json:"consumed"`
	// Remaining is what of it is left.
	Remaining *float64 `json:"remaining"`
	// ExpiresAt is when the soonest of its live batches runs out.
	ExpiresAt *time.Time `json:"expires_at"`
	// State says what the numbers are: StateRead, StateExpired or
	// StateUnavailable.
	//
	// The three are told apart, so that an account whose credits ran
	// out is not the same as an account whose credits were never read.
	State string `json:"state"`
}

// Summary is the gateway's credits as a whole.
type Summary struct {
	// TotalGranted is what all the live batches gave.
	TotalGranted float64 `json:"total_granted"`
	// TotalConsumed is what of it was used.
	TotalConsumed float64 `json:"total_consumed"`
	// TotalRemaining is what of it is left.
	TotalRemaining float64 `json:"total_remaining"`
	// ExpiresAt is when the soonest live batch runs out.
	ExpiresAt *time.Time `json:"expires_at"`
	// UpdatedAt is when the numbers were read: the newest of the
	// readings that went into this summary. It is not the time the
	// summary was made, so a stale number is not made to look fresh.
	UpdatedAt time.Time `json:"updated_at"`
	// AccountsUnavailable counts the accounts whose ledger could not be
	// read, so a short total is not mistaken for a full one.
	AccountsUnavailable int `json:"accounts_unavailable"`
	// AccountsExpired counts the accounts whose credits were read and
	// have all run out, so a short total can say why it is short.
	AccountsExpired int `json:"accounts_expired"`
	// AccountsUnread counts the accounts whose credits were never read.
	AccountsUnread int `json:"accounts_unread"`
	// Readings counts the accounts that have a reading of their
	// credits, whatever the reading said.
	//
	// 没有读数的时候，"读数时间"没有可说：说成汇总的时间，读数就像
	// 是刚刚才读的——其实一个账号都没读过。
	Readings int `json:"readings"`
	// Accounts is each account's own numbers.
	Accounts []AccountUsage `json:"accounts"`
}

// Aggregate sums the accounts' snapshots.
//
// One account is one row. When two readings of one account are given,
// the newer one is the account's reading and the older one is left
// out: summing both would count the same credits twice.
//
// accounts are the accounts in the database. They are there so that an
// account whose credits have never been read is in the summary too:
// without it a brand new account is nowhere to be seen, and "not in
// the summary" reads as "no credits". The accounts given may be nil:
// then only the accounts with a reading are in the summary, and no
// email is known.
//
// now decides which batches count: one that has run out is out. It is
// also what an expired reading is compared with, so that the same
// reading does not change its state from one call to the next.
func Aggregate(snaps []store.UsageSnapshot, accounts []store.Account, now time.Time) Summary {
	s := Summary{UpdatedAt: now}
	// 一个账号一条读数：两个读数撞在一起的时候，后来读到的那一条是它
	// 的读数。时刻说哪一次读是最新的，id 只在时刻一样时分出先后。
	reading := map[string]store.UsageSnapshot{}
	for _, sn := range snaps {
		cur, ok := reading[sn.AccountID]
		if !ok || sn.CreatedAt.After(cur.CreatedAt) ||
			(sn.CreatedAt.Equal(cur.CreatedAt) && sn.ID >= cur.ID) {
			reading[sn.AccountID] = sn
		}
	}
	emails := map[string]string{}
	ids := make(map[string]bool, len(accounts)+len(reading))
	for _, a := range accounts {
		emails[a.ID] = a.Email
		ids[a.ID] = true
	}
	for id := range reading {
		ids[id] = true
	}
	var newest time.Time
	for id := range ids {
		au := AccountUsage{ID: id, Email: emails[id]}
		sn, read := reading[id]
		switch {
		case !read:
			// 没读过：这个账号在库里，只是还没有人读过它的账本。它
			// 不是零——它是不算数，而且人得看得见它。
			au.State = StateUnread
			s.AccountsUnread++
		case !sn.HasAmounts:
			// The credits could not be read: the account is left out
			// of the totals rather than counted as zero.
			au.State = StateUnavailable
			s.AccountsUnavailable++
		case !sn.ExpiresAt.IsZero() && !sn.ExpiresAt.After(now):
			// The credits were read and every one of them has run
			// out. The account has nothing left, which is not the
			// same as an account whose credits are unknown.
			au.State = StateExpired
			s.AccountsExpired++
			g, c, r := sn.Granted, sn.Consumed, sn.Remaining
			au.Granted, au.Consumed, au.Remaining = &g, &c, &r
			t := sn.ExpiresAt
			au.ExpiresAt = &t
		default:
			au.State = StateRead
			g, c, r := sn.Granted, sn.Consumed, sn.Remaining
			au.Granted, au.Consumed, au.Remaining = &g, &c, &r
			s.TotalGranted += g
			s.TotalConsumed += c
			s.TotalRemaining += r
			if !sn.ExpiresAt.IsZero() {
				t := sn.ExpiresAt
				au.ExpiresAt = &t
				if s.ExpiresAt == nil || t.Before(*s.ExpiresAt) {
					s.ExpiresAt = &t
				}
			}
		}
		if read {
			s.Readings++
			if sn.CreatedAt.After(newest) {
				newest = sn.CreatedAt
			}
		}
		s.Accounts = append(s.Accounts, au)
	}
	if !newest.IsZero() {
		s.UpdatedAt = newest
	}
	sort.Slice(s.Accounts, func(i, j int) bool { return s.Accounts[i].ID < s.Accounts[j].ID })
	return s
}
