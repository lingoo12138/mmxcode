// Package usage sums the credits of the gateway's accounts.
//
// The numbers come from MiniMax's own credit ledger, batch by batch:
// what each batch gave, what of it was used, and what is left. Only
// batches that have not expired count, and an account whose ledger
// could not be read is left out of the totals rather than counted as
// zero.
package usage

import (
	"sort"
	"time"

	"mmxcode/internal/store"
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
	// UpdatedAt is when the numbers were read.
	UpdatedAt time.Time `json:"updated_at"`
	// AccountsUnavailable counts the accounts whose ledger could not be
	// read, so a short total is not mistaken for a full one.
	AccountsUnavailable int `json:"accounts_unavailable"`
	// Accounts is each account's own numbers.
	Accounts []AccountUsage `json:"accounts"`
}

// Aggregate sums the accounts' snapshots.
//
// now decides which batches count: one that has run out is out.
func Aggregate(snaps []store.UsageSnapshot, now time.Time) Summary {
	s := Summary{UpdatedAt: now}
	for _, sn := range snaps {
		au := AccountUsage{ID: sn.AccountID}
		if !sn.HasAmounts {
			s.AccountsUnavailable++
			s.Accounts = append(s.Accounts, au)
			continue
		}
		if sn.ExpiresAt.IsZero() || sn.ExpiresAt.After(now) {
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
		s.Accounts = append(s.Accounts, au)
	}
	sort.Slice(s.Accounts, func(i, j int) bool { return s.Accounts[i].ID < s.Accounts[j].ID })
	return s
}
