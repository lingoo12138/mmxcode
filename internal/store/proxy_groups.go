package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// GlobalGroup is the name of the group that is the global outlet.
//
// 全局出口也是组：它的名字是保留的 default，删不掉（这个名字被占了就没
// 法再建一个全局组）。
const GlobalGroup = "default"

// ProxyGroup is one outlet group: a name and the addresses it holds.
//
// 组的名字是它的身份，地址是一个有序的列表 —— 顺序就是故障转移的顺序。
type ProxyGroup struct {
	// Name is the group's name.
	Name string
	// URLs are the addresses, in the order they are tried.
	URLs []string
	// CreatedAt is when the group was made.
	CreatedAt time.Time
	// UpdatedAt is when the group was last changed.
	UpdatedAt time.Time
}

// ListProxyGroups gives the groups, by name.
func (s *Store) ListProxyGroups(ctx context.Context) ([]ProxyGroup, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT name, urls, created_at, updated_at FROM proxy_groups ORDER BY name")
	if err != nil {
		return nil, fmt.Errorf("read the outlet groups: %w", err)
	}
	defer rows.Close()
	var out []ProxyGroup
	for rows.Next() {
		g, err := scanProxyGroup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the outlet groups: %w", err)
	}
	return out, nil
}

// GetProxyGroup gives one group.
//
// 存储层说的是「没有这一行」，人说的是「没有这个组」。
func (s *Store) GetProxyGroup(ctx context.Context, name string) (ProxyGroup, error) {
	name = strings.TrimSpace(name)
	row := s.db.QueryRowContext(ctx,
		"SELECT name, urls, created_at, updated_at FROM proxy_groups WHERE name=?", name)
	g, err := scanProxyGroup(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ProxyGroup{}, fmt.Errorf("the outlet group %s: %w", name, ErrNotFound)
		}
		return ProxyGroup{}, fmt.Errorf("read the outlet group %s: %w", name, err)
	}
	return g, nil
}

// SaveProxyGroup makes a group or changes it.
//
// 名字是组的身份：这个名字已经有了，就是改这个组（地址换掉），不是建一个
// 新的组。
func (s *Store) SaveProxyGroup(ctx context.Context, g ProxyGroup) error {
	name := strings.TrimSpace(g.Name)
	if name == "" {
		return fmt.Errorf("the outlet group has no name")
	}
	urls, err := encodeGroupURLs(g.URLs)
	if err != nil {
		return fmt.Errorf("save the outlet group %s: %w", name, err)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO proxy_groups (name, urls, created_at, updated_at) VALUES (?,?,?,?)
		ON CONFLICT(name) DO UPDATE SET urls=excluded.urls, updated_at=excluded.updated_at`,
		name, urls, ms(s.stamp(g.CreatedAt)), ms(s.stamp(g.UpdatedAt)))
	if err != nil {
		return fmt.Errorf("save the outlet group %s: %w", name, err)
	}
	return nil
}

// DeleteProxyGroup takes a group away.
//
// 指着这个组的账号回到全局组去 —— 组名清掉，账号自己的地址不动。回来的
// 账号有几个，函数说。
func (s *Store) DeleteProxyGroup(ctx context.Context, name string) (int, error) {
	name = strings.TrimSpace(name)
	if name == GlobalGroup {
		return 0, fmt.Errorf("the global group cannot be taken away")
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("take away the outlet group %s: %w", name, err)
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, "DELETE FROM proxy_groups WHERE name=?", name)
	if err != nil {
		return 0, fmt.Errorf("take away the outlet group %s: %w", name, err)
	}
	gone, _ := res.RowsAffected()
	if gone == 0 {
		return 0, fmt.Errorf("the outlet group %s: %w", name, ErrNotFound)
	}
	res, err = tx.ExecContext(ctx,
		"UPDATE accounts SET proxy_group='', updated_at=? WHERE proxy_group=?",
		ms(s.now()), name)
	if err != nil {
		return 0, fmt.Errorf("take away the outlet group %s: %w", name, err)
	}
	back, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("take away the outlet group %s: %w", name, err)
	}
	return int(back), nil
}

// AccountGroups gives the names the accounts point at, and how many
// accounts point at each.
func (s *Store) AccountGroups(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT proxy_group, COUNT(*) FROM accounts
		WHERE proxy_group IS NOT NULL AND proxy_group <> ''
		GROUP BY proxy_group`)
	if err != nil {
		return nil, fmt.Errorf("read the outlet groups of the accounts: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var (
			name string
			n    int
		)
		if err := rows.Scan(&name, &n); err != nil {
			return nil, fmt.Errorf("read the outlet groups of the accounts: %w", err)
		}
		out[name] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the outlet groups of the accounts: %w", err)
	}
	return out, nil
}

// SeedResult says what the seeding did.
type SeedResult struct {
	// GlobalSeeded says the global group was made from the configuration.
	GlobalSeeded bool
	// GlobalShadowed says the configuration's proxy.default does not
	// count any more: the global group is in the database.
	GlobalShadowed bool
	// Created are the groups that were made from the configuration.
	Created []string
	// Shadowed are the groups that are in the configuration and in the
	// database: the one in the database counts.
	Shadowed []string
	// Unknown are the names the accounts point at that are not in the
	// database.
	Unknown []string
	// UnknownAccounts is how many accounts point at those names.
	UnknownAccounts int
}

// SeedProxyGroups makes the groups the configuration names, if the
// database does not have them.
//
// 库是组的老家：配置里的 proxy.groups 只是一个种子 —— 库里没有的组用配
// 置里的地址建出来，库里已经有了的就是库里的算数。老库里那些指着组名的
// 号，靠这一步还指着原来的那条路。
//
// 全局组（default）总是有的：没有就建一个，配置里的 proxy.default 是它
// 的种子。
func (s *Store) SeedProxyGroups(ctx context.Context, def string, cfg map[string]string) (SeedResult, error) {
	var res SeedResult
	now := s.now()
	have := map[string]bool{}
	groups, err := s.ListProxyGroups(ctx)
	if err != nil {
		return res, err
	}
	for _, g := range groups {
		have[g.Name] = true
	}
	// 全局组：库里没有就建一个。配置里的那个地址是它的种子。
	if !have[GlobalGroup] {
		var urls []string
		if u := strings.TrimSpace(def); u != "" {
			urls = append(urls, u)
		}
		if err := s.SaveProxyGroup(ctx, ProxyGroup{
			Name: GlobalGroup, URLs: urls, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			return res, err
		}
		have[GlobalGroup] = true
		res.GlobalSeeded = len(urls) > 0
	} else if strings.TrimSpace(def) != "" {
		// 库里已经有全局组了：配置里那个不再管事。
		res.GlobalShadowed = true
	}
	// 配置里的组：库里没有的建出来。
	names := make([]string, 0, len(cfg))
	for n := range cfg {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if have[n] {
			res.Shadowed = append(res.Shadowed, n)
			continue
		}
		u := strings.TrimSpace(cfg[n])
		if u == "" {
			continue
		}
		if err := s.SaveProxyGroup(ctx, ProxyGroup{
			Name: n, URLs: []string{u}, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			return res, err
		}
		have[n] = true
		res.Created = append(res.Created, n)
	}
	// 账号指着库里的组：那些组不在的，账号跟着全局组走（组名留着，人把
	// 组建出来它就自己回去了）。
	counts, err := s.AccountGroups(ctx)
	if err != nil {
		return res, err
	}
	for n, c := range counts {
		if have[n] {
			continue
		}
		res.Unknown = append(res.Unknown, n)
		res.UnknownAccounts += c
	}
	sort.Strings(res.Unknown)
	return res, nil
}

// scanProxyGroup reads one row of the groups.
func scanProxyGroup(row scanner) (ProxyGroup, error) {
	var (
		g                ProxyGroup
		raw              string
		createdAt, updAt int64
	)
	if err := row.Scan(&g.Name, &raw, &createdAt, &updAt); err != nil {
		return ProxyGroup{}, err
	}
	urls, err := decodeGroupURLs(raw)
	if err != nil {
		return ProxyGroup{}, fmt.Errorf("read the outlet group %s: %w", g.Name, err)
	}
	g.URLs = urls
	g.CreatedAt = fromMS(createdAt)
	g.UpdatedAt = fromMS(updAt)
	return g, nil
}

// encodeGroupURLs writes the addresses of a group down.
//
// 地址是一个 JSON 数组：顺序在里面，页面上一个文本框就是它。
func encodeGroupURLs(urls []string) (string, error) {
	out := make([]string, 0, len(urls))
	for _, u := range urls {
		if u = strings.TrimSpace(u); u != "" {
			out = append(out, u)
		}
	}
	if len(out) == 0 {
		return "[]", nil
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// decodeGroupURLs reads the addresses of a group.
//
// 读不出来就报错：一份读不出地址的组，谁也不知道它该走哪条路。
func decodeGroupURLs(raw string) ([]string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, fmt.Errorf("the addresses are not a list: %w", err)
	}
	return out, nil
}
