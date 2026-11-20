package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestTheCheckinsOfTheDayAreTheOnesThatGotCredits says the day's
// check-ins are given, with the credits they gave.
//
// 它是签到的积分进池的那条路（规格 §5.3）：只给给了积分的 —— 没有积分
// 的签到没有东西要告诉池子。
func TestTheCheckinsOfTheDayAreTheOnesThatGotCredits(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	makeAccount(t, s, "paid")
	makeAccount(t, s, "free")
	day := "2026-11-18"
	if err := s.SaveCheckin(ctx, Checkin{
		AccountID: "paid", Day: day, Outcome: "claimed", Credit: 25,
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	// 没有积分的签到：没有东西要告诉池子。
	if err := s.SaveCheckin(ctx, Checkin{
		AccountID: "free", Day: day, Outcome: "ineligible",
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	// 别的日子签的，不算今天的。
	if err := s.SaveCheckin(ctx, Checkin{
		AccountID: "free", Day: "2026-11-17", Outcome: "claimed", Credit: 25,
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	got, err := s.DayCheckins(ctx, day)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].AccountID != "paid" || got[0].Credit != 25 {
		t.Fatalf("the check-ins of the day = %+v, want just the paid one", got)
	}
}

// TestTheAccountsWithoutACheckinAreTheOnesThatHaveNotSignedInToday
// says the day's list is the accounts that were not checked in.
//
// 只签今天还没签的号（规格 §5.2）：签过的再签一次，站点只会答 done，白
// 花一次调用 —— 而今天没签到的那部分，正是服务器 00:00 没开着时漏掉的
// 那部分。
func TestTheAccountsWithoutACheckinAreTheOnesThatHaveNotSignedInToday(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	makeAccount(t, s, "signed")
	makeAccount(t, s, "waiting")
	day := "2026-11-18"
	if err := s.SaveCheckin(ctx, Checkin{
		AccountID: "signed", Day: day, Outcome: "claimed", Credit: 25,
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	// 签不上的号不在名单里：要的是重登，不是签到。
	if err := s.CreateAccount(ctx, Account{
		ID: "gone", Name: "gone", Status: StatusSignedOut,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	got, err := s.AccountsWithoutCheckin(ctx, day)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "waiting" {
		t.Fatalf("the accounts = %+v, want just the waiting one", got)
	}
}

// TestTheCheckinIndexReachesAnOldFile says a file that is already at the
// old version gets the index on the day.
//
// 老库停在版本 6，走不到 migrateConform 里那几条索引（那一步只把版本 1
// 的库带到版本 2）。索引是新的那一步建的：没有它，「今天谁签到了」就是
// 全表扫 —— 一天一个号一行，1000 个号一年 36 万行（规格 §5.3）。
func TestTheCheckinIndexReachesAnOldFile(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	// 把它退回到老版本：索引没有。
	if _, err := s.db.ExecContext(ctx, "DROP INDEX IF EXISTS idx_checkins_day_account"); err != nil {
		t.Fatal(err)
	}
	// 退回到 6：那是索引那一步（版本 7）之前的版本 —— 从那里再走一遍，
	// 索引那一步就会跑。
	if _, err := s.db.ExecContext(ctx, "PRAGMA user_version = 6"); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='index' AND tbl_name='checkins'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		if name == "idx_checkins_day_account" {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("the old file did not get the index on the day")
	}
}

// TestTheCheckinZoneIsTheSitesZone says the check-in days are counted in
// the site's zone.
//
// 站点按大陆时间算一天：签到的时刻和「今天是哪一天」都得按那个时区算
// （规格 §5.1）。
func TestTheCheckinZoneIsTheSitesZone(t *testing.T) {
	// UTC 的 11-18 23:30 是大陆时间的 11-19 07:30。
	at := time.Date(2026, 11, 18, 23, 30, 0, 0, time.UTC)
	if got := at.In(CheckinZone()).Hour(); got != 7 {
		t.Errorf("the hour in the site's zone = %d, want 7", got)
	}
	if got := DayKey(at); got != "2026-11-19" {
		t.Errorf("the day = %s, want 2026-11-19", got)
	}
}

// TestTheCheckinsHaveAnIndexOnTheDay says the day's check-ins can be
// found without reading the whole table.
//
// 签到一天一行，一个号一天一行：1000 个号一年就是 36 万行。查「今天谁
// 签到」要的是索引，不是全表扫（规格 §5.3）。
//
// 那条索引盖住的不止日子：号、时刻、积分都在里面 —— 「今天签到的号和它
// 们给的积分」那一条查询因此不用回表（1000 次回表就是 1000 次随机读）。
func TestTheCheckinsHaveAnIndexOnTheDay(t *testing.T) {
	s := openTest(t)
	rows, err := s.db.QueryContext(context.Background(),
		`SELECT name FROM sqlite_master WHERE type='index' AND tbl_name='checkins'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		if name == "idx_checkins_day_account" {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("the check-ins have no index on the day")
	}
}

// TestTheAccountsWithoutACheckinAskTheDayAtOnce says the day's list is
// asked for in one go.
//
// 一个号一次问一次签到没有，1000 个号就是 1000 次对签到表的随机探测
// （签到的表越大越贵：一年 36 万行的库上是 4.05 ms）。同一天签到的那些号
// 是可以一次拿全的：签到表按日子排，那一天就是连着的一千行。
//
// 断言抓的是**我们自己的**名字（索引名），不是 SQLite 的措辞 ——
// 「LIST SUBQUERY」那些词是 SQLite 的话，升级一下就可能换一句，而它要防
// 的那件事（那一天的签到一次拿全）没变。
func TestTheAccountsWithoutACheckinAskTheDayAtOnce(t *testing.T) {
	s := openTest(t)
	makeAccount(t, s, "waiting")

	plan := queryPlan(t, s, accountsWithoutCheckinSQL, StatusActive, "2026-11-18")
	if !strings.Contains(plan, "idx_checkins_day_account") {
		t.Fatalf("the plan does not use the index on the day: %s", plan)
	}
}
