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

	"mmxcode/internal/store"
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
	// Accounts is each account's own numbers.
	Accounts []AccountUsage `json:"accounts"`
}

// Aggregate sums the accounts' snapshots.
//
// One account is one row. When two readings of one account are given,
// the newer one is the account's reading and the older one is left
// out: summing both would count the same credits twice.
//
// now decides which batches count: one that has run out is out. It is
// also what an expired reading is compared with, so that the same
// reading does not change its state from one call to the next.
func Aggregate(snaps []store.UsageSnapshot, now time.Time) Summary {
	s := Summary{UpdatedAt: now}
	var newest time.Time
	for _, sn := range newestPerAccount(snaps) {
		au := AccountUsage{ID: sn.AccountID}
		switch {
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
		if sn.CreatedAt.After(newest) {
			newest = sn.CreatedAt
		}
		s.Accounts = append(s.Accounts, au)
	}
	if !newest.IsZero() {
		s.UpdatedAt = newest
	}
	sort.Slice(s.Accounts, func(i, j int) bool { return s.Accounts[i].ID < s.Accounts[j].ID })
	return s
}

// newestPerAccount gives each account's newest snapshot.
//
// The newest is the one that was read last: the later reading wins,
// and a reading that shares its millisecond with another is told from
// it by the id, which grows with every row written.
func newestPerAccount(snaps []store.UsageSnapshot) []store.UsageSnapshot {
	byAccount := make(map[string]store.UsageSnapshot, len(snaps))
	at := make(map[string]int, len(snaps))
	order := make([]string, 0, len(snaps))
	for i, sn := range snaps {
		cur, ok := byAccount[sn.AccountID]
		if !ok {
			byAccount[sn.AccountID] = sn
			at[sn.AccountID] = i
			order = append(order, sn.AccountID)
			continue
		}
		// Two readings that cannot be told apart are the later one's:
		// they are the same reading, so either will do.
		if newer(sn, cur) || (sn.ID == cur.ID && i > at[sn.AccountID]) {
			byAccount[sn.AccountID] = sn
			at[sn.AccountID] = i
		}
	}
	out := make([]store.UsageSnapshot, 0, len(order))
	for _, id := range order {
		out = append(out, byAccount[id])
	}
	return out
}

// newer says a reading is newer than another.
func newer(a, b store.UsageSnapshot) bool {
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return a.CreatedAt.After(b.CreatedAt)
	}
	return a.ID > b.ID
}
