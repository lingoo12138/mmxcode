package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// touchBatch is how many sessions one statement touches.
//
// SQLite has a limit on how many placeholders a statement may have (999
// is the old default). A batch bigger than that would not run at all.
const touchBatch = 500

// Candidate is an account that could be admitted to the pool.
//
// 它是「还没被挑进池」的那一类账号：签入中、现在没有被标记用完，而且
// 带着它最后读到的那份积分。
type Candidate struct {
	// Account is the account itself.
	Account Account
	// Remaining is how many credits are left. It says something only
	// when Known is true.
	Remaining float64
	// Known says the credits are known.
	//
	// 它是 false 的时候 Remaining 说不了话：这个号的额度不知道是多少。
	// 不是 0 —— 0 是「读到了，没有了」。
	//
	// 它和 LastReadAt 是两回事：读过一次但那次没有数（HasAmounts 是
	// false）的号，是「读过了，数额不知道」：Known 是 false，LastReadAt
	// 不是零。
	Known bool
	// LastReadAt is when the credits were read last. It is the zero
	// time when they never were.
	LastReadAt time.Time
}

// PoolCandidates gives the accounts that could be admitted to the pool.
//
// 谁先进池（排队的规矩）：
//
//  1. **最近活跃的对话绑着的号** —— 它们不能丢：会话钉在号上，钉住的号
//     不在池里就会被改绑，缓存命中就没了（规格不变量 3）。
//  2. **今天签到过的号** —— 签到刚给了积分（七天有效），它一定还有额度；
//     而从没读过的号有没有额度是**不知道的**，填池那一趟要读它，读到 0
//     就白读一次（规格 §5.3）。
//  3. **从没读过的号，最新的导入排前面** —— 新导入的号几乎肯定没被用过
//     （用过就会被读，读过就有数）。导入很久还没数的号多半是被用过的，
//     它的额度早就不知道剩多少了。
//  4. **读过、但读得最早的号** —— 最冷的那些：它们闲着，闲着的号额度
//     还没被消耗光。
//
// 排除掉的：exclude 里有的（已经在池里）、状态不是 active 的、现在被
// 标记用完的（标记的时刻还在将来的才算；过去的标记已经回来了，那个号
// 可以再进池）。
//
// since 是「多久以前算活跃」：只有这段时间里被碰过的会话，它绑着的号
// 才排在最前。
//
// 读多少行：要 limit 个候选，外加 exclude 里那么多行 —— 被排除的那些也
// 占着行号，所以要多取一些才够数。回来的是**至多** limit 个。
func (s *Store) PoolCandidates(ctx context.Context, limit int, exclude map[string]bool, since time.Time) ([]Candidate, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT a.id, a.name, a.user_id, a.email, a.status, a.proxy_group, a.proxy_url,
			a.last_error, a.created_at, a.updated_at,
			u.remaining, u.created_at
		FROM accounts a
		LEFT JOIN usage_snapshots u ON u.id = (
			SELECT u2.id FROM usage_snapshots u2 WHERE u2.account_id = a.id
			ORDER BY u2.created_at DESC, u2.id DESC LIMIT 1)
		WHERE a.status = ?
		  AND NOT EXISTS (
			SELECT 1 FROM exhausted_windows e
			WHERE e.account_id = a.id AND e.until > ?)
		ORDER BY
			(EXISTS (SELECT 1 FROM sessions s
				WHERE s.account_id = a.id AND s.updated_at >= ?)) DESC,
			(EXISTS (SELECT 1 FROM checkins c
				WHERE c.account_id = a.id AND c.day = ?)) DESC,
			(u.created_at IS NULL) DESC,
			u.created_at ASC,
			a.created_at DESC,
			a.id
		LIMIT ?`, StatusActive, ms(s.now()), ms(since), s.Today(), limit+len(exclude))
	if err != nil {
		return nil, fmt.Errorf("pool candidates: %w", err)
	}
	defer rows.Close()
	var out []Candidate
	for rows.Next() {
		var (
			c         Candidate
			remaining sql.NullFloat64
			readAt    sql.NullInt64
		)
		a, err := scanAccount(rows, &remaining, &readAt)
		if err != nil {
			return nil, fmt.Errorf("pool candidates: %w", err)
		}
		c.Account = a
		c.Remaining = remaining.Float64
		c.Known = remaining.Valid
		c.LastReadAt = fromMS(readAt.Int64)
		if exclude[c.Account.ID] {
			continue
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pool candidates: %w", err)
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// CountUsable says how many accounts could answer right now.
//
// 能答 = 签入中（status 是 active）而且现在没有被标记用完。标记的时刻
// 已经过去的那个号算能答 —— 它的窗口回来了（规格 §4.5）。
func (s *Store) CountUsable(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM accounts a
		WHERE a.status = ?
		  AND NOT EXISTS (
			SELECT 1 FROM exhausted_windows e
			WHERE e.account_id = a.id AND e.until > ?)`,
		StatusActive, ms(s.now())).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count usable accounts: %w", err)
	}
	return n, nil
}

// CountRecentSessions says how many sessions were touched since a moment.
//
// 「活跃会话」是这么数的：一段时间里被碰过的会话。绑在它们身上的号是
// 缓存命中的地方 —— 池要装得下它们（规格 §6.3）。
//
// 会话表从来不删（规格 §8），所以数的是「最近碰过的」，不是「一共有的」。
//
// 「碰过」是网关写进去的（TouchSessions）：每次请求真的用了这个会话，
// 网关就把它的 updated_at 刷新一下。刷新是攒着写的 —— 一批一次，不是
// 每个请求写一行。
func (s *Store) CountRecentSessions(ctx context.Context, since time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM sessions WHERE updated_at >= ?", ms(since)).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count recent sessions: %w", err)
	}
	return n, nil
}

// TouchSessions marks sessions as used now.
//
// 「用过」是活跃会话唯一的凭据。updated_at 只在改绑的时候刷新：一段一直
// 用同一个号的对话从来不改绑，它的 updated_at 就停在当初绑定的那一刻，
// 一小时以后看起来就不活跃了 —— 而它明明一直在用。
//
// 一次改一批：每个请求写一行是浪费，锁也白抢。网关那边攒一批再一次写
// 掉，这里也按批写 —— 一条语句的占位符个数有上限。
func (s *Store) TouchSessions(ctx context.Context, sessionKeys []string) error {
	if len(sessionKeys) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(sessionKeys))
	keys := make([]string, 0, len(sessionKeys))
	for _, k := range sessionKeys {
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return nil
	}
	for start := 0; start < len(keys); start += touchBatch {
		end := start + touchBatch
		if end > len(keys) {
			end = len(keys)
		}
		chunk := keys[start:end]
		marks := strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")
		args := make([]any, 0, len(chunk)+1)
		args = append(args, ms(s.now()))
		for _, k := range chunk {
			args = append(args, k)
		}
		q := "UPDATE sessions SET updated_at = ? WHERE session_key IN (" + marks + ")"
		if _, err := s.db.ExecContext(ctx, q, args...); err != nil {
			return fmt.Errorf("touch the sessions: %w", err)
		}
	}
	return nil
}
