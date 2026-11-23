package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/lingoo12138/mmxcode/internal/store"
)

// atDay puts the store's clock on a day.
//
// 日子是站点的日子（大陆时间），所以这里用的是中午：不管测试机在哪个时
// 区，那一天都是同一天。
func atDay(s *store.Store, day time.Time) {
	s.Clock = func() time.Time { return day }
}

// TestTheDailyUsageIsWhatGrewOutOfTheLedger says the day's row is the
// growth of the ledger's cumulative number.
//
// 账本给的是累计的数：今天比昨天大，长出来的那一截就是今天用掉的。
func TestTheDailyUsageIsWhatGrewOutOfTheLedger(t *testing.T) {
	s := newStore(t)
	atDay(s, time.Date(2026, 11, 23, 4, 0, 0, 0, time.UTC)) // noon in China
	ctx := context.Background()

	// The first reading of the day is the base: nothing is known about
	// when those credits were used, so they are not counted as today's.
	if err := s.SaveUsageDaily(ctx, "acc_1", 100, true); err != nil {
		t.Fatalf("save the day: %v", err)
	}
	// The ledger grew by 40: that is what the day used.
	if err := s.SaveUsageDaily(ctx, "acc_1", 140, true); err != nil {
		t.Fatalf("save the day: %v", err)
	}
	// The batch ran out and a new one took its place: the ledger went
	// back to zero. That is not consumption.
	if err := s.SaveUsageDaily(ctx, "acc_1", 0, true); err != nil {
		t.Fatalf("save the day: %v", err)
	}
	// The new batch is being used: 25 more.
	if err := s.SaveUsageDaily(ctx, "acc_1", 25, true); err != nil {
		t.Fatalf("save the day: %v", err)
	}
	// A ledger that could not be read is not zero: it changes nothing.
	if err := s.SaveUsageDaily(ctx, "acc_1", 999, false); err != nil {
		t.Fatalf("save the day: %v", err)
	}

	rows, err := s.UsageDailyAccounts(ctx, "2026-11-23")
	if err != nil {
		t.Fatalf("the day's accounts: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("the day's accounts = %d, want one", len(rows))
	}
	if rows[0].Used != 65 {
		t.Errorf("the day used %v, want 65 (40 before the reset, 25 after it)", rows[0].Used)
	}
	if rows[0].Consumed != 25 {
		t.Errorf("the day's base = %v, want the last number read (25)", rows[0].Consumed)
	}
	if rows[0].Readings != 4 {
		t.Errorf("the day has %d readings, want 4 (the one that failed is not a reading)",
			rows[0].Readings)
	}
}

// TestTheFirstReadingOfADayIsComparedWithYesterday says the first reading
// of a day is compared with the last reading of the day before.
//
// 夜里用掉的积分算今天：昨天最后一次读到 100，今天第一次读到 140，那
// 40 分是今天凌晨用掉的。
func TestTheFirstReadingOfADayIsComparedWithYesterday(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	atDay(s, time.Date(2026, 11, 22, 4, 0, 0, 0, time.UTC))
	if err := s.SaveUsageDaily(ctx, "acc_1", 100, true); err != nil {
		t.Fatalf("save the day: %v", err)
	}
	atDay(s, time.Date(2026, 11, 23, 4, 0, 0, 0, time.UTC))
	if err := s.SaveUsageDaily(ctx, "acc_1", 140, true); err != nil {
		t.Fatalf("save the day: %v", err)
	}

	totals, err := s.UsageDailyTotals(ctx, "2026-11-22", "2026-11-23")
	if err != nil {
		t.Fatalf("the totals: %v", err)
	}
	if len(totals) != 2 {
		t.Fatalf("the totals = %d days, want two", len(totals))
	}
	if totals[0].Day != "2026-11-22" || totals[0].Used != 0 {
		t.Errorf("the first day = %+v, want no use (the base is not a use)", totals[0])
	}
	if totals[1].Day != "2026-11-23" || totals[1].Used != 40 {
		t.Errorf("the second day = %+v, want 40 used", totals[1])
	}
	if totals[1].Accounts != 1 {
		t.Errorf("the second day has %d accounts, want one", totals[1].Accounts)
	}
}

// TestTheDailyTotalsAddUpTheAccounts says the day's total is the sum of the
// accounts' days.
func TestTheDailyTotalsAddUpTheAccounts(t *testing.T) {
	s := newStore(t)
	atDay(s, time.Date(2026, 11, 23, 4, 0, 0, 0, time.UTC))
	ctx := context.Background()

	if err := s.SaveUsageDaily(ctx, "acc_1", 10, true); err != nil {
		t.Fatalf("save the day: %v", err)
	}
	if err := s.SaveUsageDaily(ctx, "acc_1", 30, true); err != nil {
		t.Fatalf("save the day: %v", err)
	}
	if err := s.SaveUsageDaily(ctx, "acc_2", 50, true); err != nil {
		t.Fatalf("save the day: %v", err)
	}
	if err := s.SaveUsageDaily(ctx, "acc_2", 70, true); err != nil {
		t.Fatalf("save the day: %v", err)
	}

	totals, err := s.UsageDailyTotals(ctx, "2026-11-23", "2026-11-23")
	if err != nil {
		t.Fatalf("the totals: %v", err)
	}
	if len(totals) != 1 {
		t.Fatalf("the totals = %d days, want one", len(totals))
	}
	if totals[0].Used != 40 {
		t.Errorf("the day used %v, want 40 (20 + 20)", totals[0].Used)
	}
	if totals[0].Accounts != 2 {
		t.Errorf("the day has %d accounts, want two", totals[0].Accounts)
	}
}

// TestTheOldDaysAreThrownAway says the days that are too old are gone.
//
// 折线图看三十天，账本也留三十天：更早的那些谁也不看。
func TestTheOldDaysAreThrownAway(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	atDay(s, time.Date(2026, 10, 1, 4, 0, 0, 0, time.UTC))
	if err := s.SaveUsageDaily(ctx, "acc_1", 10, true); err != nil {
		t.Fatalf("save the day: %v", err)
	}
	atDay(s, time.Date(2026, 11, 23, 4, 0, 0, 0, time.UTC))
	if err := s.SaveUsageDaily(ctx, "acc_1", 20, true); err != nil {
		t.Fatalf("save the day: %v", err)
	}

	n, err := s.PruneUsageDaily(ctx, "2026-11-01")
	if err != nil {
		t.Fatalf("prune the days: %v", err)
	}
	if n != 1 {
		t.Errorf("the prune took %d days, want one", n)
	}
	totals, err := s.UsageDailyTotals(ctx, "2026-10-01", "2026-11-23")
	if err != nil {
		t.Fatalf("the totals: %v", err)
	}
	if len(totals) != 1 || totals[0].Day != "2026-11-23" {
		t.Errorf("the totals = %+v, want the day that is left", totals)
	}
}

// TestTheDayGoesWithTheAccount says an account's day is taken with it.
//
// 账号没了，它的那几行账也不该留着：那是它自己的账。
func TestTheDayGoesWithTheAccount(t *testing.T) {
	s := newStore(t)
	atDay(s, time.Date(2026, 11, 23, 4, 0, 0, 0, time.UTC))
	ctx := context.Background()

	if err := s.SaveUsageDaily(ctx, "acc_1", 10, true); err != nil {
		t.Fatalf("save the day: %v", err)
	}
	if err := s.DeleteAccount(ctx, "acc_1"); err != nil {
		t.Fatalf("delete the account: %v", err)
	}
	rows, err := s.UsageDailyAccounts(ctx, "2026-11-23")
	if err != nil {
		t.Fatalf("the day's accounts: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("the day's accounts = %+v, want none", rows)
	}
}
