package store

import (
	"context"
	"fmt"
)

// LegacyProxy is one account that still carries an outlet group.
//
// 出口组退役了：组名还在那些老账号的行上。开机的时候收拾一次 —— 组名变
// 成账号自己的地址，这张表就空了。
type LegacyProxy struct {
	// ID is the account's id.
	ID string
	// Group is the outlet group the account is pointed at.
	Group string
	// URL is the address the account carries itself. It is empty when
	// the account has none.
	URL string
}

// LegacyProxyGroups gives the accounts that still carry an outlet group.
//
// 这一列不删：删列要重建整张表，那张表上有触发器、有一百多个号在用，为
// 一个已经空了的列去重建它不值。代码从此不再写它、也不再读它，只有这一
// 处读 —— 开机的时候收拾一次。
func (s *Store) LegacyProxyGroups(ctx context.Context) ([]LegacyProxy, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, proxy_group, proxy_url FROM accounts
		WHERE proxy_group IS NOT NULL AND proxy_group <> ''`)
	if err != nil {
		return nil, fmt.Errorf("read the outlet groups of the accounts: %w", err)
	}
	defer rows.Close()
	var out []LegacyProxy
	for rows.Next() {
		var lp LegacyProxy
		if err := rows.Scan(&lp.ID, &lp.Group, &lp.URL); err != nil {
			return nil, fmt.Errorf("read the outlet groups of the accounts: %w", err)
		}
		out = append(out, lp)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the outlet groups of the accounts: %w", err)
	}
	return out, nil
}
