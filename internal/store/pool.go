package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

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
	// Known says the credits have been read.
	//
	// 它是 false 的时候 Remaining 说不了话：这个号从来没被读过。不是 0
	// —— 0 是「读到了，没有了」。
	Known bool
	// LastReadAt is when the credits were read last. It is the zero
	// time when they never were.
	LastReadAt time.Time
}

// PoolCandidates gives the accounts that could be admitted to the pool.
//
// **最冷的在前**：从来没读过的排最前，读过的按最后读到的时刻从早到晚。
// 冷号先进池 —— 被用熟的号留给会话（规格 §6.5）。
//
// 排除掉的：exclude 里有的（已经在池里）、状态不是 active 的、现在被
// 标记用完的（标记的时刻还在将来的才算；过去的标记已经回来了，那个号
// 可以再进池）。
//
// 读多少行：要 limit 个候选，外加 exclude 里那么多行 —— 被排除的那些也
// 占着行号，所以要多取一些才够数。
func (s *Store) PoolCandidates(ctx context.Context, limit int, exclude map[string]bool) ([]Candidate, error) {
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
		ORDER BY (u.created_at IS NULL) DESC, u.created_at ASC, a.created_at ASC
		LIMIT ?`, StatusActive, ms(s.now()), limit+len(exclude))
	if err != nil {
		return nil, fmt.Errorf("pool candidates: %w", err)
	}
	defer rows.Close()
	var out []Candidate
	for rows.Next() {
		var (
			c         Candidate
			userID    sql.NullString
			email     sql.NullString
			pg        sql.NullString
			pu        sql.NullString
			lastErr   sql.NullString
			createdAt int64
			updatedAt int64
			remaining sql.NullFloat64
			readAt    sql.NullInt64
		)
		if err := rows.Scan(&c.Account.ID, &c.Account.Name, &userID, &email, &c.Account.Status,
			&pg, &pu, &lastErr, &createdAt, &updatedAt,
			&remaining, &readAt); err != nil {
			return nil, fmt.Errorf("pool candidates: %w", err)
		}
		c.Account.UserID = userID.String
		c.Account.Email = email.String
		c.Account.ProxyGroup = pg.String
		c.Account.ProxyURL = pu.String
		c.Account.LastError = lastErr.String
		c.Account.CreatedAt = fromMS(createdAt)
		c.Account.UpdatedAt = fromMS(updatedAt)
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
func (s *Store) CountRecentSessions(ctx context.Context, since time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM sessions WHERE updated_at >= ?", ms(since)).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count recent sessions: %w", err)
	}
	return n, nil
}
