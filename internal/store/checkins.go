package store

import (
	"context"
	"fmt"
	"time"
)

// checkinZone is the timezone a check-in day is counted in.
//
// The site counts its days in China, so a day here is that zone's day.
// The zone is loaded by name, so the name is never ambiguous: "CST" is
// China Standard Time here and US Central somewhere else. A machine
// without the timezone database gets the same offset under a name that
// says what it is.
var checkinZone = loadCheckinZone()

// loadCheckinZone gives the timezone a check-in day is counted in.
func loadCheckinZone() *time.Location {
	if loc, err := time.LoadLocation("Asia/Shanghai"); err == nil {
		return loc
	}
	return time.FixedZone("UTC+8", 8*3600)
}

// CheckinZone gives the timezone the check-in days are counted in.
//
// 它导出出来，是为了让调度器算「下一趟签到是几点」的时候跟库里算「今
// 天是哪一天」用的是同一个时区：站点按大陆时间算一天，签到就得落在那
// 一天里（规格 §5.1）。
func CheckinZone() *time.Location {
	return checkinZone
}

// DayKey is the day a check-in is counted for.
//
// The day is the site's day, so that the day rolls over at the same
// hour for every account, whatever the machine's own timezone is.
func DayKey(t time.Time) string {
	return t.In(checkinZone).Format("2006-01-02")
}

// Today is the site's day, by the store's clock.
//
// 签到这一件事只有一个"今天"：签到那一趟的名单（今天谁还没签）、签到的
// 积分进池（今天谁签了）、候选的排序（今天谁签过）—— 它们得说的是同
// 一个日子。库里带着自己的钟（Store.Clock），测试拿它把日子钉住。
func (s *Store) Today() string {
	return DayKey(s.now())
}

// CheckinCredit is what one account's check-in gave.
type CheckinCredit struct {
	// AccountID is the account.
	AccountID string
	// Credit is how many credits the check-in gave.
	Credit float64
}

// DayCheckins gives the accounts that were checked in on a day, with the
// credits the check-in gave.
//
// 它是签到的积分进池的那条路：签到给了积分，池里那一份是进池那一刻的，
// 不知道这件事（规格 §5.3）。
//
// 只给给了积分的：没有积分的签到没有东西要告诉池子。
func (s *Store) DayCheckins(ctx context.Context, day string) ([]CheckinCredit, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT account_id, credit FROM checkins
		WHERE day = ? AND credit > 0
		ORDER BY account_id`, day)
	if err != nil {
		return nil, fmt.Errorf("the check-ins of the day: %w", err)
	}
	defer rows.Close()
	var out []CheckinCredit
	for rows.Next() {
		var c CheckinCredit
		if err := rows.Scan(&c.AccountID, &c.Credit); err != nil {
			return nil, fmt.Errorf("the check-ins of the day: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("the check-ins of the day: %w", err)
	}
	return out, nil
}

// accountsWithoutCheckinSQL is the query that gives the accounts that
// were not checked in on a day.
//
// 它是签到那一趟的名单：只签今天还没签的号（规格 §5.2）。
//
// 问法是「今天签到过的号有哪些」一次拿全，再去掉它们（NOT IN），不是一
// 个号一次问一次签到没有：后者是 1000 次对签到表的随机探测，签到的表越
// 大越贵（一年 36 万行的库上：4.05 ms 对 1.52 ms）。
//
// NOT IN 遇到 NULL 会一个都不返回。签到的 account_id 是 NOT NULL 的
// （schema.go），迁移的时候老行补的也是空串不是 NULL，所以不会。
const accountsWithoutCheckinSQL = `
		SELECT a.id, a.name, a.user_id, a.email, a.status, a.proxy_url,
			a.last_error, a.created_at, a.updated_at
		FROM accounts a
		WHERE a.status = ?
		  AND a.id NOT IN (SELECT c.account_id FROM checkins c WHERE c.day = ?)
		ORDER BY a.created_at, a.id`

// AccountsWithoutCheckin gives the accounts that were not checked in on a
// day.
//
// 只有 active 的号：签不上的号签不了（signed_out 是 token 掉了，要的是
// 重登，不是签到）。
func (s *Store) AccountsWithoutCheckin(ctx context.Context, day string) ([]Account, error) {
	rows, err := s.db.QueryContext(ctx, accountsWithoutCheckinSQL, StatusActive, day)
	if err != nil {
		return nil, fmt.Errorf("the accounts without a check-in: %w", err)
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, fmt.Errorf("the accounts without a check-in: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("the accounts without a check-in: %w", err)
	}
	return out, nil
}
