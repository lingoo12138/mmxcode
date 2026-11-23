package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// 每天一行：一个账号一天用了多少积分。
//
// 账本给的是一个累计的数：今天比昨天大，长出来的那一截就是今天用掉的。
// 这一张表记的就是那一截。
//
// 它跟读数表是两回事：读数表记「读到的时候账本上是多少」（一刻钟以前的
// 就扔了），这一张表记「一天里用掉了多少」，留三十天。

// DailyUsage is one account's one day of credits.
type DailyUsage struct {
	// AccountID is the account.
	AccountID string `json:"account_id"`
	// Day is the site's day: 2026-11-23.
	Day string `json:"day"`
	// Consumed is the ledger's cumulative number at the last reading of
	// the day. It is the base the next day's number is compared with.
	Consumed float64 `json:"consumed"`
	// Used is how much was used that day.
	Used float64 `json:"used"`
	// Readings is how many readings went into the row.
	Readings int `json:"readings"`
	// UpdatedAt is when the row was last written.
	UpdatedAt time.Time `json:"updated_at"`
}

// DayTotal is one day of the whole gateway's credits.
type DayTotal struct {
	// Day is the site's day.
	Day string `json:"day"`
	// Used is how much was used that day, over all the accounts.
	Used float64 `json:"used"`
	// Accounts is how many accounts had a row that day.
	Accounts int `json:"accounts"`
	// Readings is how many readings went into the day.
	Readings int `json:"readings"`
}

// SaveUsageDaily folds a reading into the account's day.
//
// 账本上的数是累计的，它今天比昨天大，长出来的那一截就是今天用掉的。
// 批次到期会把它清零（换了一批新的），清零的那一下不是「用掉了」，是
// 「换了一批」，所以只算长出来的那一截。
//
// 一天里的头一笔读数跟前一天的最后一次比：中间用掉的都算今天。前一天没
// 有读数（这个号是新的）的时候，头一笔只作底数 —— 不知道那些数是什么时候
// 用掉的，所以不算今天的消耗。
func (s *Store) SaveUsageDaily(ctx context.Context, accountID string, consumed float64, has bool) error {
	if !has {
		// 读不到的账本不是零：它没有数，所以这一天没有长出来的一截。
		return nil
	}
	now := s.now()
	day := DayKey(now)

	var (
		have bool
		last float64
	)
	row := s.db.QueryRowContext(ctx,
		`SELECT consumed FROM usage_daily WHERE account_id=? AND day=?`, accountID, day)
	switch err := row.Scan(&last); {
	case err == nil:
		have = true
	case errors.Is(err, sql.ErrNoRows):
		// 这一天还没有行：头一笔。
	default:
		return fmt.Errorf("usage daily: read the day: %w", err)
	}

	base := last
	if !have {
		// 头一笔：跟前一天的最后一次比。
		prev, ok := s.consumedOn(ctx, accountID, previousDay(day))
		if !ok {
			// 前一天没有数：没有底数就没有差，这一笔不算消耗。
			base = consumed
		} else {
			base = prev
		}
	}
	delta := consumed - base
	if delta < 0 {
		// 批次换了：账本被清零了。清零不是消耗。
		delta = 0
	}

	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO usage_daily (account_id, day, consumed, used, readings, updated_at)
		VALUES (?,?,?,?,1,?)
		ON CONFLICT(account_id, day) DO UPDATE SET
			consumed   = excluded.consumed,
			used       = usage_daily.used + ?,
			readings   = usage_daily.readings + 1,
			updated_at = excluded.updated_at`,
		accountID, day, consumed, delta, ms(now), delta); err != nil {
		return fmt.Errorf("usage daily: write the day: %w", err)
	}
	return nil
}

// consumedOn gives the last cumulative number of an account on a day.
//
// It says false when that day has no row: the account was not read then, or
// it was not there yet.
func (s *Store) consumedOn(ctx context.Context, accountID, day string) (float64, bool) {
	if day == "" {
		return 0, false
	}
	var v sql.NullFloat64
	row := s.db.QueryRowContext(ctx,
		`SELECT consumed FROM usage_daily WHERE account_id=? AND day=?`, accountID, day)
	switch err := row.Scan(&v); {
	case err == nil:
		return v.Float64, v.Valid
	case errors.Is(err, sql.ErrNoRows):
		return 0, false
	default:
		return 0, false
	}
}

// UsageDailyTotals gives the totals of the days between two days.
//
// The days are the site's days, from and to included. A day that is not in
// the table is not in the result: the caller decides what an absent day
// means (nothing was used that day, or nobody was read).
func (s *Store) UsageDailyTotals(ctx context.Context, from, to string) ([]DayTotal, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT day, SUM(used), COUNT(*), SUM(readings)
		FROM usage_daily
		WHERE day >= ? AND day <= ?
		GROUP BY day
		ORDER BY day`, from, to)
	if err != nil {
		return nil, fmt.Errorf("usage daily totals: %w", err)
	}
	defer rows.Close()
	var out []DayTotal
	for rows.Next() {
		var d DayTotal
		if err := rows.Scan(&d.Day, &d.Used, &d.Accounts, &d.Readings); err != nil {
			return nil, fmt.Errorf("usage daily totals: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("usage daily totals: %w", err)
	}
	return out, nil
}

// UsageDailyAccounts gives one day's rows, the ones that used the most
// first.
//
// 它是那一天的明细：折线图上点一天，就该看得见那天是哪些号在用。
func (s *Store) UsageDailyAccounts(ctx context.Context, day string) ([]DailyUsage, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT account_id, day, consumed, used, readings, updated_at
		FROM usage_daily
		WHERE day=?
		ORDER BY used DESC, account_id`, day)
	if err != nil {
		return nil, fmt.Errorf("usage daily accounts: %w", err)
	}
	defer rows.Close()
	var out []DailyUsage
	for rows.Next() {
		var d DailyUsage
		var consumed sql.NullFloat64
		var updatedAt int64
		if err := rows.Scan(&d.AccountID, &d.Day, &consumed, &d.Used, &d.Readings, &updatedAt); err != nil {
			return nil, fmt.Errorf("usage daily accounts: %w", err)
		}
		d.Consumed = consumed.Float64
		d.UpdatedAt = fromMS(updatedAt)
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("usage daily accounts: %w", err)
	}
	return out, nil
}

// PruneUsageDaily throws away the days that are older than a day.
//
// The days are the site's days: the day key is a date, and dates are
// written down in one order, so "older than" is a string comparison.
func (s *Store) PruneUsageDaily(ctx context.Context, olderThan string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM usage_daily WHERE day < ?`, olderThan)
	if err != nil {
		return 0, fmt.Errorf("usage daily prune: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("usage daily prune: %w", err)
	}
	return n, nil
}

// previousDay gives the day before a day key.
//
// 日子是站点的日子，所以是同一个时区里的前一天。减一天用日期算，不是减
// 24 小时：时区里没有夏令时，两种算法一样，但日期算法不会在跨月的时候
// 算错。
func previousDay(day string) string {
	t, err := time.ParseInLocation("2006-01-02", day, checkinZone)
	if err != nil {
		return ""
	}
	return DayKey(t.AddDate(0, 0, -1))
}
