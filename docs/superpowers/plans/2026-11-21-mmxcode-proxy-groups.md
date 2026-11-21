# 出口组 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 一个出口组能有多条地址（一条也行），组里的地址按顺序用、坏了往下换；组存在数据库里，页面/API/命令行都能改；全局代理本身也是一个组。

**Architecture:** 组是新表 `proxy_groups`（名字是身份，`urls` 是一个有序的 JSON 数组）。账号行上那个 `proxy_group` 那一列重新有用，存的是组名。出口的算法在 `internal/egress`：候选地址按「账号自己的地址 → 账号指着的组 → 全局组」排，组内按健康排（探通过的、没探过的、探失败的）。网关在网络错误时换下一个候选地址重试。健康表 `egress_health`（按地址记）管着「哪个地址活着」。

**Tech Stack:** Go 1.26、`modernc.org/sqlite`（纯 Go）、`spf13/cobra`、`encoding/json`、服务端渲染的模板 + 一份 `app.js`。

规格：`docs/superpowers/specs/2026-11-21-mmxcode-proxy-groups-design.md`

---

## 路线

出口的链条是四级：

```
账号自己的地址  →  账号指着的组  →  全局组（default）  →  直连
```

每一级都能有多条地址（组），组内按第 3 个任务的排序挑。第一级是账号自己的地址 —— 一条。

任务表：

| 任务 | 做什么 | 落在哪 |
|---|---|---|
| 1 | 组的表和存取 | `internal/store` |
| 2 | 账号行上的组名 | `internal/store` |
| 3 | 出口怎么算（候选地址、健康排序） | `internal/egress` |
| 4 | 开机时种组 | `internal/store`、`cmd/mmxcode/root.go` |
| 5 | 请求失败时换地址 | `internal/manager`、`internal/gateway` |
| 6 | 健康检查查组里的地址 | `internal/scheduler` |
| 7 | 页面和 API | `internal/webui` |
| 8 | 命令行 | `cmd/mmxcode` |
| 9 | 文档和部署 | README、配置样例 |
| 10 | 体检 | 全部 |

---

### Task 1: 组的表

**Files:**
- Modify: `internal/store/schema.go`（新表）
- Modify: `internal/store/store.go`（版本 8、迁移步骤）
- Create: `internal/store/proxy_groups.go`
- Test: `internal/store/proxy_groups_test.go`

- [ ] **Step 1: 写失败的测试**

`internal/store/proxy_groups_test.go` 整个文件：

```go
package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestTheGroupsAreKept says a group is kept as it was written: the name
// is its identity, and the addresses keep their order.
//
// 顺序就是故障转移的顺序：第一个是主路，后面的都是备路。存进去的时候
// 顺序乱了，这个组就废了。
func TestTheGroupsAreKept(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now()
	err := s.SaveProxyGroup(ctx, ProxyGroup{
		Name: "clash", URLs: []string{"http://a:8080", "http://b:8080"},
		CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("save the group: %v", err)
	}
	got, err := s.GetProxyGroup(ctx, "clash")
	if err != nil {
		t.Fatalf("get the group: %v", err)
	}
	if got.Name != "clash" {
		t.Errorf("the group's name is %q", got.Name)
	}
	if len(got.URLs) != 2 || got.URLs[0] != "http://a:8080" || got.URLs[1] != "http://b:8080" {
		t.Errorf("the group's addresses are %v", got.URLs)
	}
}

// TestTheGroupIsChanged says a group that is saved again keeps its name
// and gets the new addresses.
//
// 组的名字是它的身份：改组是改它的地址，不是换一个新组来。
func TestTheGroupIsChanged(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now()
	if err := s.SaveProxyGroup(ctx, ProxyGroup{
		Name: "clash", URLs: []string{"http://a:8080"}, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("save the group: %v", err)
	}
	if err := s.SaveProxyGroup(ctx, ProxyGroup{
		Name: "clash", URLs: []string{"http://b:8080", "http://c:8080"}, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("change the group: %v", err)
	}
	got, err := s.GetProxyGroup(ctx, "clash")
	if err != nil {
		t.Fatalf("get the group: %v", err)
	}
	if len(got.URLs) != 2 || got.URLs[0] != "http://b:8080" {
		t.Errorf("the group's addresses are %v", got.URLs)
	}
}

// TestTheGroupThatIsNotThere says a group that is not there is said so.
func TestTheGroupThatIsNotThere(t *testing.T) {
	s := openTest(t)
	_, err := s.GetProxyGroup(context.Background(), "nope")
	if err == nil {
		t.Fatal("the group that is not there was found")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("the error is %v", err)
	}
}

// TestTheGroupsAreListedByName says the list is by name, so the page
// shows them in the same order every time.
func TestTheGroupsAreListedByName(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now()
	for _, n := range []string{"zh", "clash", "hk"} {
		if err := s.SaveProxyGroup(ctx, ProxyGroup{
			Name: n, URLs: []string{"http://" + n + ":8080"}, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("save the group %s: %v", n, err)
		}
	}
	got, err := s.ListProxyGroups(ctx)
	if err != nil {
		t.Fatalf("list the groups: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("there are %d groups", len(got))
	}
	if got[0].Name != "clash" || got[1].Name != "hk" || got[2].Name != "zh" {
		t.Errorf("the groups are %s, %s, %s", got[0].Name, got[1].Name, got[2].Name)
	}
}

// TestTheAccountPointsAtAGroup says an account can point at a group, and
// that the address and the group are one thing: setting one clears the
// other.
//
// 地址和组是二选一：账号只有一条出口。
func TestTheAccountPointsAtAGroup(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now()
	makeAccount(t, s, "acc_1")
	if err := s.SetAccountOutlet(ctx, "acc_1", "", "clash", now); err != nil {
		t.Fatalf("set the outlet: %v", err)
	}
	a, err := s.GetAccount(ctx, "acc_1")
	if err != nil {
		t.Fatalf("get the account: %v", err)
	}
	if a.ProxyGroup != "clash" {
		t.Errorf("the account's group is %q", a.ProxyGroup)
	}
	if a.ProxyURL != "" {
		t.Errorf("the account has an address of its own: %q", a.ProxyURL)
	}
	// 给了地址，组就该让位。
	if err := s.SetAccountOutlet(ctx, "acc_1", "http://a:8080", "", now); err != nil {
		t.Fatalf("set the outlet: %v", err)
	}
	a, err = s.GetAccount(ctx, "acc_1")
	if err != nil {
		t.Fatalf("get the account: %v", err)
	}
	if a.ProxyURL != "http://a:8080" {
		t.Errorf("the account's address is %q", a.ProxyURL)
	}
	if a.ProxyGroup != "" {
		t.Errorf("the account still points at the group %q", a.ProxyGroup)
	}
}

// TestTheGroupThatIsTakenAway says the accounts that pointed at a group
// go back to the global one when the group is taken away.
//
// 组没了，账号行上的组名就没人认了 —— 清掉，它们跟着全局走。
func TestTheGroupThatIsTakenAway(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now()
	for _, id := range []string{"acc_1", "acc_2"} {
		makeAccount(t, s, id)
		if err := s.SetAccountOutlet(ctx, id, "", "clash", now); err != nil {
			t.Fatalf("set the outlet: %v", err)
		}
	}
	if err := s.SaveProxyGroup(ctx, ProxyGroup{
		Name: "clash", URLs: []string{"http://a:8080"}, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("save the group: %v", err)
	}
	back, err := s.DeleteProxyGroup(ctx, "clash")
	if err != nil {
		t.Fatalf("take away the group: %v", err)
	}
	if back != 2 {
		t.Errorf("%d account(s) went back to the global group", back)
	}
	a, err := s.GetAccount(ctx, "acc_1")
	if err != nil {
		t.Fatalf("get the account: %v", err)
	}
	if a.ProxyGroup != "" {
		t.Errorf("the account still points at the group %q", a.ProxyGroup)
	}
}

// TestTheGlobalGroupCannotBeTakenAway says the global group stays.
//
// 全局组是保留名：它没了，"全局"就不全局了。
func TestTheGlobalGroupCannotBeTakenAway(t *testing.T) {
	s := openTest(t)
	if _, err := s.DeleteProxyGroup(context.Background(), GlobalGroup); err == nil {
		t.Fatal("the global group was taken away")
	}
}

// TestTheGlobalGroupIsMadeFromTheConfiguration says the global group is
// made from proxy.default when the database does not have it.
//
// 全局出口也是组：配置里的那个地址是它的种子。种子用过之后配置里那个就
// 不再管事了（页面上改的是库里那份）。
func TestTheGlobalGroupIsMadeFromTheConfiguration(t *testing.T) {
	s := openTest(t)
	res, err := s.SeedProxyGroups(context.Background(), "http://global:8080", nil)
	if err != nil {
		t.Fatalf("seed the groups: %v", err)
	}
	if !res.GlobalSeeded {
		t.Error("the global group was not made from the configuration")
	}
	g, err := s.GetProxyGroup(context.Background(), GlobalGroup)
	if err != nil {
		t.Fatalf("get the global group: %v", err)
	}
	if len(g.URLs) != 1 || g.URLs[0] != "http://global:8080" {
		t.Errorf("the global group's addresses are %v", g.URLs)
	}
}

// TestTheGroupsInTheConfigurationAreSeeded says a group in the
// configuration is made when the database does not have it.
//
// 老库里有一批账号指着组名（组名指向的地址在配置文件里）。组回到库里，
// 这些号不用动就还指着原来的那条路。
func TestTheGroupsInTheConfigurationAreSeeded(t *testing.T) {
	s := openTest(t)
	res, err := s.SeedProxyGroups(context.Background(), "",
		map[string]string{"clash": "http://clash:8080"})
	if err != nil {
		t.Fatalf("seed the groups: %v", err)
	}
	if len(res.Created) != 1 || res.Created[0] != "clash" {
		t.Errorf("the groups that were made are %v", res.Created)
	}
	g, err := s.GetProxyGroup(context.Background(), "clash")
	if err != nil {
		t.Fatalf("get the group: %v", err)
	}
	if len(g.URLs) != 1 || g.URLs[0] != "http://clash:8080" {
		t.Errorf("the group's addresses are %v", g.URLs)
	}
}

// TestTheGroupInTheDatabaseCounts says the group in the database is not
// touched by the one in the configuration.
//
// 库里是老家：人在页面上改过的组，配置里的那个不算数。
func TestTheGroupInTheDatabaseCounts(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now()
	if err := s.SaveProxyGroup(ctx, ProxyGroup{
		Name: "clash", URLs: []string{"http://mine:8080"}, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("save the group: %v", err)
	}
	res, err := s.SeedProxyGroups(ctx, "", map[string]string{"clash": "http://theirs:8080"})
	if err != nil {
		t.Fatalf("seed the groups: %v", err)
	}
	if len(res.Shadowed) != 1 || res.Shadowed[0] != "clash" {
		t.Errorf("the groups that were shadowed are %v", res.Shadowed)
	}
	g, err := s.GetProxyGroup(ctx, "clash")
	if err != nil {
		t.Fatalf("get the group: %v", err)
	}
	if len(g.URLs) != 1 || g.URLs[0] != "http://mine:8080" {
		t.Errorf("the group's addresses are %v", g.URLs)
	}
}

// TestTheGroupNameThatIsNotThereIsSaid says an account that points at a
// group that is not in the database is named.
//
// 那个账号跟着全局组走（组名留着：人把组建出来，它就自己回去了）。但人
// 该知道有账号指着一条不存在的路。
func TestTheGroupNameThatIsNotThereIsSaid(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now()
	makeAccount(t, s, "acc_1")
	if err := s.SetAccountOutlet(ctx, "acc_1", "", "hk", now); err != nil {
		t.Fatalf("set the outlet: %v", err)
	}
	res, err := s.SeedProxyGroups(ctx, "http://global:8080", nil)
	if err != nil {
		t.Fatalf("seed the groups: %v", err)
	}
	if len(res.Unknown) != 1 || res.Unknown[0] != "hk" {
		t.Errorf("the unknown groups are %v", res.Unknown)
	}
	if res.UnknownAccounts != 1 {
		t.Errorf("%d account(s) point at a group that is not there", res.UnknownAccounts)
	}
}
```

- [ ] **Step 2: 跑测试，确认它失败**

Run: `go test ./internal/store/ -run TestTheGroups -v`
Expected: FAIL —— `undefined: ProxyGroup`、`undefined: openTest`。

- [ ] **Step 3: 看 store 的测试是怎么建库的**

Run: `grep -n "func openTest" -A 20 internal/store/store_test.go`
Expected: 一个开着一个临时库的构造函数。若没有，就照 `internal/store/store_test.go` 里别的测试那样建。

- [ ] **Step 4: 写表**

`internal/store/schema.go` 的 `tables` 里，`egress_health` 后面加这一张：

```go
	{
		// The outlet groups: a name and the addresses it holds.
		//
		// 组的名字是它的身份，地址是一个有序的数组（JSON）—— 顺序就是故
		// 障转移的顺序。
		name: "proxy_groups",
		body: `(
  name       TEXT PRIMARY KEY,
  urls       TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
)`,
		copy: []copyColumn{
			notNull("name", "''"),
			notNull("urls", "'[]'"),
			notNull("created_at", "0"),
			notNull("updated_at", "0"),
		},
	},
```

- [ ] **Step 5: 版本和迁移**

`internal/store/store.go`：

```go
// schemaVersion is the version a file is at when the steps are done.
const schemaVersion = 8

```

`migrations` 的末尾加上 `migrateProxyGroups,`，并在迁移列表后面加上这个步骤：

```go
// migrateProxyGroups gives the file its table of outlet groups.
//
// 这一张表是后来才有的：一个文件在它之前做出来的时候没有它。表不在就
// 造出来，在就不动它 —— 已经有了的组不该为了一个迁移被抄一遍。
func migrateProxyGroups(ctx context.Context, conn *sql.Conn) error {
	return runStep(ctx, conn, 8, func(tx *sql.Tx) error {
		for _, t := range tables {
			if t.name != "proxy_groups" {
				continue
			}
			got, err := storedSQL(ctx, tx, t.name)
			if err != nil {
				return err
			}
			if got != "" {
				return nil
			}
			if _, err := tx.ExecContext(ctx, "CREATE TABLE "+t.name+t.body); err != nil {
				return fmt.Errorf("migrate: make the table %s: %w", t.name, err)
			}
		}
		return nil
	})
}

```

- [ ] **Step 6: 组的存取**

`internal/store/proxy_groups.go` 整个文件：

```go
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
```

- [ ] **Step 7: 跑测试，确认它过了**

Run: `go test ./internal/store/ -run TestTheGroup -v`
Expected: PASS。

若 `isNoRows` 和 `isNotFound` 在这个包里不存在，用包里已有的写法（`grep -n "ErrNotFound" internal/store/store.go` 看它是怎么判的）改掉。

- [ ] **Step 8: 提交**

```bash
git add internal/store/schema.go internal/store/store.go internal/store/proxy_groups.go internal/store/proxy_groups_test.go
git commit -F .git/commit_msg.txt
```

`.git/commit_msg.txt` 写：

```
feat(store): 出口组的表：名字是身份，地址是有序的一串

组回来了：一个名字配一组地址，顺序就是故障转移的顺序。地址存在一个 JSON
数组里（页面上一个文本框就是它），组的名字是主键 —— 改组是改它的地址。

新表 proxy_groups，文件版本 7 到 8。老文件做一次迁移：表不在就造出来。
```

---

### Task 2: 账号行上的组名

**Files:**
- Modify: `internal/store/store.go`（`Account.ProxyGroup`、那些 SELECT、`scanAccount`、`SetAccountOutlet`）
- Modify: `internal/store/proxy_groups.go`（`DeleteProxyGroup` 已经清账号了）
- Test: `internal/store/proxy_groups_test.go`

- [ ] **Step 1: 写失败的测试**

加到 `internal/store/proxy_groups_test.go`：

```go
// TestTheAccountPointsAtAGroup says an account can point at a group, and
// that the address and the group are one thing: setting one clears the
// other.
//
// 地址和组是二选一：账号只有一条出口。
func TestTheAccountPointsAtAGroup(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now()
	makeAccount(t, s, "acc_1")
	if err := s.SetAccountOutlet(ctx, "acc_1", "", "clash", now); err != nil {
		t.Fatalf("set the outlet: %v", err)
	}
	a, err := s.GetAccount(ctx, "acc_1")
	if err != nil {
		t.Fatalf("get the account: %v", err)
	}
	if a.ProxyGroup != "clash" {
		t.Errorf("the account's group is %q", a.ProxyGroup)
	}
	if a.ProxyURL != "" {
		t.Errorf("the account has an address of its own: %q", a.ProxyURL)
	}
	// 给了地址，组就该让位。
	if err := s.SetAccountOutlet(ctx, "acc_1", "http://a:8080", "", now); err != nil {
		t.Fatalf("set the outlet: %v", err)
	}
	a, err = s.GetAccount(ctx, "acc_1")
	if err != nil {
		t.Fatalf("get the account: %v", err)
	}
	if a.ProxyURL != "http://a:8080" {
		t.Errorf("the account's address is %q", a.ProxyURL)
	}
	if a.ProxyGroup != "" {
		t.Errorf("the account still points at the group %q", a.ProxyGroup)
	}
}

// TestTheGroupThatIsTakenAway says the accounts that pointed at a group
// go back to the global one when the group is taken away.
//
// 组没了，账号行上的组名就没人认了 —— 清掉，它们跟着全局走。
func TestTheGroupThatIsTakenAway(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now()
	for _, id := range []string{"acc_1", "acc_2"} {
		makeAccount(t, s, id)
		if err := s.SetAccountOutlet(ctx, id, "", "clash", now); err != nil {
			t.Fatalf("set the outlet: %v", err)
		}
	}
	if err := s.SaveProxyGroup(ctx, ProxyGroup{
		Name: "clash", URLs: []string{"http://a:8080"}, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("save the group: %v", err)
	}
	back, err := s.DeleteProxyGroup(ctx, "clash")
	if err != nil {
		t.Fatalf("take away the group: %v", err)
	}
	if back != 2 {
		t.Errorf("%d account(s) went back to the global group", back)
	}
	a, err := s.GetAccount(ctx, "acc_1")
	if err != nil {
		t.Fatalf("get the account: %v", err)
	}
	if a.ProxyGroup != "" {
		t.Errorf("the account still points at the group %q", a.ProxyGroup)
	}
}

// TestTheGlobalGroupCannotBeTakenAway says the global group stays.
//
// 全局组是保留名：它没了，"全局"就不全局了。
func TestTheGlobalGroupCannotBeTakenAway(t *testing.T) {
	s := openTest(t)
	if _, err := s.DeleteProxyGroup(context.Background(), GlobalGroup); err == nil {
		t.Fatal("the global group was taken away")
	}
}

```

- [ ] **Step 2: 跑测试，确认它失败**

Run: `go test ./internal/store/ -run "TestTheAccountPointsAtAGroup|TestTheGroupThatIsTakenAway" -v`
Expected: FAIL —— `unknown field ProxyGroup`、`undefined: SetAccountOutlet`。

- [ ] **Step 3: 账号行上多一列组名**

`internal/store/store.go`：

`Account` 结构体里，`ProxyURL` 后面加：

```go
	// ProxyGroup is the outlet group the account is pointed at.
	//
	// 组名是空的：这个账号没有指着一个组，它跟着全局组走（或者自己写了
	// 地址，或者直连）。
	ProxyGroup string
```

所有 `SELECT id, name, user_id, email, status, proxy_url, ...` 都要在 `proxy_url` 后面加上 `proxy_group`（`internal/store/store.go` 里 6 处，`internal/store/checkins.go` 1 处，`internal/store/pool.go` 1 处）。

`scanAccount` 改成：

```go
// scanAccount reads one account row.
//
// The extra destinations are read after the account's own columns, so a
// caller that wants more of the row (the credits of a reading, say) does
// not have to spell the account's ten columns out again.
func scanAccount(sc scanner, extra ...any) (Account, error) {
	var (
		a                    Account
		userID, email, pu    sql.NullString
		pg                   sql.NullString
		lastErr              sql.NullString
		createdAt, updatedAt int64
	)
	dest := []any{&a.ID, &a.Name, &userID, &email, &a.Status, &pu, &pg,
		&lastErr, &createdAt, &updatedAt}
	dest = append(dest, extra...)
	if err := sc.Scan(dest...); err != nil {
		return Account{}, err
	}
	a.UserID = userID.String
	a.Email = email.String
	a.ProxyURL = pu.String
	a.ProxyGroup = pg.String
	a.LastError = lastErr.String
	a.CreatedAt = fromMS(createdAt)
	a.UpdatedAt = fromMS(updatedAt)
	return a, nil
}

```

`SetAccountProxy` 改成写两列（地址和组是二选一，两个一起写）：

```go
// SetAccountOutlet writes an account's outlet.
//
// It is narrow like UpdateAccountStatus: the outlet is the only thing
// it writes, so a caller that changes the outlet does not have to read
// the account first.
//
// 地址和组是二选一：账号只有一条出口。给了地址，组就清掉；给了组，地址
// 就清掉。空着的那个就是「没有」—— 账号跟着全局组走。
func (s *Store) SetAccountOutlet(ctx context.Context, id, url, group string, updatedAt time.Time) error {
	return s.setAccountOutlet(ctx, s.db, id, url, group, updatedAt)
}

```

`SetAccountProxy` 删掉（它只被页面和命令行用，两个都要换成 `SetAccountOutlet`）。谁在用：

Run: `grep -rn "SetAccountProxy" --include=*.go .`
Expected: `internal/webui/webui.go`、`cmd/mmxcode/accounts.go`、`internal/provision/chain.go`（导入那一条）。导入那条改成 `SetAccountOutlet(ctx, id, "", "", now)`（导入不碰账号的出口）。

- [ ] **Step 4: 跑测试，确认它过了**

Run: `go test ./internal/store/ -v`
Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/store/store.go internal/store/proxy_groups.go internal/store/proxy_groups_test.go internal/store/checkins.go internal/store/pool.go internal/provision/chain.go
git commit -F .git/commit_msg.txt
```

`.git/commit_msg.txt` 写：

```
feat(store): 账号行上的组名重新有用，地址和组是二选一

accounts.proxy_group 那一列一直在库里（删列要重建整张表，那张表上有触发
器、有一百多个号）。组回来了，这一列就又是组名了。

SetAccountOutlet 一次写两列：给了地址组就清掉，给了组地址就清掉。账号只
有一条出口。

组被删掉的时候，指着它的账号回到全局组 —— 组名清掉，账号自己的地址不动。
```

---

### Task 3: 出口怎么算

**Files:**
- Create: `internal/egress/groups.go`
- Modify: `internal/egress/egress.go`（`Resolver` 的字段、`Resolve`、`Outlets`、`OutletURLs`）
- Test: `internal/egress/groups_test.go`

- [ ] **Step 1: 写失败的测试**

`internal/egress/groups_test.go` 整个文件：

```go
package egress

import (
	"net"
	"testing"

	"github.com/lingoo12138/mmxcode/internal/store"
)

// TestTheCandidatesFollowTheChain says the candidates are the account's
// own address, then its group, then the global group.
//
// 出口是四级的：账号自己的地址 → 账号指着的组 → 全局组 → 直连。
func TestTheCandidatesFollowTheChain(t *testing.T) {
	r := New("http://global:8080")
	r.SetGroups(map[string][]string{
		store.GlobalGroup: {"http://global:8080"},
		"clash":           {"http://clash:8080", "http://clash2:8080"},
	})
	got := r.Candidates(store.Account{ProxyURL: "http://own:8080", ProxyGroup: "clash"})
	want := []string{"http://own:8080", "http://clash:8080", "http://clash2:8080", "http://global:8080"}
	if len(got) != len(want) {
		t.Fatalf("the candidates are %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("candidate %d is %q, not %q", i, got[i], want[i])
		}
	}
}

// TestTheAccountWithoutAGroupFollowsTheGlobalOne says an account with
// nothing of its own follows the global group.
func TestTheAccountWithoutAGroupFollowsTheGlobalOne(t *testing.T) {
	r := New("")
	r.SetGroups(map[string][]string{
		store.GlobalGroup: {"http://global:8080", "http://global2:8080"},
	})
	got := r.Candidates(store.Account{})
	if len(got) != 2 || got[0] != "http://global:8080" {
		t.Errorf("the candidates are %v", got)
	}
}

// TestTheAccountWithNothingAtAllIsDirect says there is no candidate at
// all when nothing is configured.
//
// 直连是最后一级，不是错。
func TestTheAccountWithNothingAtAllIsDirect(t *testing.T) {
	r := New("")
	r.SetGroups(map[string][]string{})
	if got := r.Resolve(store.Account{}); got != Direct {
		t.Errorf("the way out is %q", got)
	}
}

// TestTheAddressThatAnsweredComesFirst says the addresses that answered
// the probe come first, then the ones that were never probed, then the
// ones that did not answer.
//
// 组里的顺序是主备：探通过的先走，没探过的其次（探的结果可能是旧的），
// 探失败的最后。
func TestTheAddressThatAnsweredComesFirst(t *testing.T) {
	r := New("")
	r.SetGroups(map[string][]string{
		store.GlobalGroup: {"http://dead:8080", "http://alive:8080", "http://unknown:8080"},
	})
	r.SetHealth("http://dead:8080", false)
	r.SetHealth("http://alive:8080", true)
	got := r.Candidates(store.Account{})
	want := []string{"http://alive:8080", "http://unknown:8080", "http://dead:8080"}
	if len(got) != len(want) {
		t.Fatalf("the candidates are %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("candidate %d is %q, not %q", i, got[i], want[i])
		}
	}
}

// TestTheGroupThatIsNotThereIsSkipped says a group that is not in the
// database is skipped: the account follows the global one.
//
// 组名留着（人等下把组建出来，这个号就自己回去了），出口往下走。
func TestTheGroupThatIsNotThereIsSkipped(t *testing.T) {
	r := New("")
	r.SetGroups(map[string][]string{
		store.GlobalGroup: {"http://global:8080"},
	})
	got := r.Candidates(store.Account{ProxyGroup: "nope"})
	if len(got) != 1 || got[0] != "http://global:8080" {
		t.Errorf("the candidates are %v", got)
	}
}

// TestTheSameAddressIsOnlyOnce says the same address is not asked for
// twice.
func TestTheSameAddressIsOnlyOnce(t *testing.T) {
	r := New("")
	r.SetGroups(map[string][]string{
		store.GlobalGroup: {"http://global:8080"},
		"clash":           {"http://global:8080", "http://clash:8080"},
	})
	got := r.Candidates(store.Account{ProxyGroup: "clash"})
	if len(got) != 2 {
		t.Errorf("the candidates are %v", got)
	}
}

// TestTheUnreachableIsToldFromTheSlow says only the errors that say the
// proxy was never reached count as the proxy's fault.
//
// 上游慢、TLS 错、拿到了 502 —— 那些不是代理的错。全记的话，一次抖动就
// 把一条好路踢掉了。
func TestTheUnreachableIsToldFromTheSlow(t *testing.T) {
	dial := &net.OpError{Op: "dial", Err: &net.DNSError{Err: "no such host"}}
	if !Unreachable(dial) {
		t.Error("the dial failure is not unreachable")
	}
	proxy := &net.OpError{Op: "proxyconnect", Err: dial}
	if !Unreachable(proxy) {
		t.Error("the proxy failure is not unreachable")
	}
	read := &net.OpError{Op: "read", Err: net.ErrClosed}
	if Unreachable(read) {
		t.Error("the read failure is unreachable")
	}
	if Unreachable(nil) {
		t.Error("no error at all is unreachable")
	}
}
```

- [ ] **Step 2: 跑测试，确认它失败**

Run: `go test ./internal/egress/ -run "TestTheCandidates|TestTheAccount|TestTheAddress|TestTheGroup|TestTheSame|TestTheUnreachable" -v`
Expected: FAIL —— `undefined: SetGroups`、`undefined: Candidates`、`undefined: Unreachable`。

- [ ] **Step 3: 解析器多记两张表**

`internal/egress/egress.go`，`Resolver` 结构体里加两个字段：

```go
	// groups are the outlet groups, by name: the addresses of each, in
	// the order they are tried.
	groups map[string][]string
	// health says what the last probe of an address said.
	health map[string]bool
```

`New` 里把这两张表建起来：

```go
// New makes a resolver over the global default.
func New(def string) *Resolver {
	return &Resolver{
		def:        def,
		groups:     map[string][]string{},
		health:     map[string]bool{},
		clients:    map[string]*http.Client{},
		transports: map[string]http.RoundTripper{},
	}
}

```

`Resolve` 改成用候选地址：

```go
// Resolve gives the proxy URL an account's requests go through.
//
// Nothing configured means direct: the account has no proxy of its
// own, and there is no global default either.
//
// The lock is taken, so that the maps are read the same way the caches
// are.
func (r *Resolver) Resolve(a store.Account) string {
	c := r.Candidates(a)
	if len(c) == 0 {
		return Direct
	}
	return c[0]
}

```

- [ ] **Step 4: 候选地址**

`internal/egress/groups.go` 整个文件：

```go
package egress

import (
	"errors"
	"net"
	"strings"

	"github.com/lingoo12138/mmxcode/internal/store"
)

// SetGroups gives the resolver the groups it does not know yet.
//
// 组是换过的：页面上改了一个组，网关下一次挑地址就该按新的来。这是一份
// 快照 —— 换的时候整份换，不是一组一组地换（换到一半的快照会挑出半个组
// 里的地址）。
func (r *Resolver) SetGroups(groups map[string][]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	next := make(map[string][]string, len(groups))
	for name, urls := range groups {
		next[name] = append([]string(nil), urls...)
	}
	r.groups = next
}

// SetHealth says what the last probe of an address said.
//
// 探活的结果是用来排序的：探通过的先走。
func (r *Resolver) SetHealth(url string, ok bool) {
	url = strings.TrimSpace(url)
	if url == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.health[url] = ok
}

// Candidates gives the addresses an account's requests may go through, in
// the order they are tried.
//
// 出口是四级的：账号自己的地址 → 账号指着的组 → 全局组 → 直连。组里的
// 地址按健康排（探通过的、没探过的、探失败的）。
//
// 一个候选都没有就是直连 —— 直连是最后一级，不是错。
func (r *Resolver) Candidates(a store.Account) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	seen := map[string]bool{}
	add := func(u string) {
		u = strings.TrimSpace(u)
		if u == "" || seen[u] {
			return
		}
		seen[u] = true
		out = append(out, u)
	}
	if u := strings.TrimSpace(a.ProxyURL); u != "" {
		add(u)
	}
	if g := strings.TrimSpace(a.ProxyGroup); g != "" {
		for _, u := range r.order(r.groups[g]) {
			add(u)
		}
	}
	for _, u := range r.order(r.groups[store.GlobalGroup]) {
		add(u)
	}
	// 解析器还没有组（开机的时候还没读库）：用配置里那个全局默认。
	if _, ok := r.groups[store.GlobalGroup]; !ok {
		if v := strings.TrimSpace(r.def); v != "" {
			add(v)
		}
	}
	return out
}

// order puts the addresses that answered first, then the ones that were
// never probed, then the ones that did not answer.
//
// 探失败的最后：探的结果可能是旧的（探完到现在过了一个轮次，代理早就活
// 了）。
func (r *Resolver) order(urls []string) []string {
	if len(urls) < 2 {
		return urls
	}
	var good, unknown, bad []string
	for _, u := range urls {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		switch ok, seen := r.health[u]; {
		case seen && ok:
			good = append(good, u)
		case seen:
			bad = append(bad, u)
		default:
			unknown = append(unknown, u)
		}
	}
	out := make([]string, 0, len(urls))
	out = append(out, good...)
	out = append(out, unknown...)
	out = append(out, bad...)
	return out
}

// Unreachable says whether an error means the proxy was never reached.
//
// 只有「根本没连上」才算代理的错：dial 失败、proxyconnect 失败、代理自己
// 的域名解析不了。上游慢、超时、TLS 握手错、上游回了 502 —— 那些不是代
// 理的错，不该把它记成坏的。
//
// 全记的话，一次抖动就把一条好路踢掉了；全不记的话，一个死掉的代理在下
// 一轮健康检查之前，每个请求都先撞它一次。
func Unreachable(err error) bool {
	if err == nil {
		return false
	}
	var op *net.OpError
	if errors.As(err, &op) {
		switch op.Op {
		case "dial", "proxyconnect":
			return true
		}
	}
	var dns *net.DNSError
	return errors.As(err, &dns)
}

// GroupMap gives the groups as a map from the name to the addresses.
//
// 页面上、健康检查里、网关上都用这一份：一个组一个名字，名字下面是它
// 的地址。
func GroupMap(groups []store.ProxyGroup) map[string][]string {
	out := make(map[string][]string, len(groups))
	for _, g := range groups {
		out[g.Name] = append([]string(nil), g.URLs...)
	}
	return out
}

// GroupsWithDefault puts the configuration's default address into the
// groups when the global group is not there.
//
// 库里还没有全局组的时候（刚开机还没种，或者种失败了），出口走的就是配置
// 里那个全局默认 —— 解析器也是这么挑的。健康检查和页面得查同一个地址：查
// 的是另一个，说的是另一件事。
func GroupsWithDefault(groups map[string][]string, def string) map[string][]string {
	out := make(map[string][]string, len(groups)+1)
	for name, urls := range groups {
		out[name] = append([]string(nil), urls...)
	}
	if _, ok := out[store.GlobalGroup]; !ok {
		if v := strings.TrimSpace(def); v != "" {
			out[store.GlobalGroup] = []string{v}
		}
	}
	return out
}
```

- [ ] **Step 5: 在用的地址**

`internal/egress/egress.go` 的 `Outlets` 和 `OutletURLs` 改成看组：

```go
// Outlets gives the addresses that are in use.
//
// The groups come first — the global one, then the others by name — and
// then the addresses the accounts carry themselves, each of them once.
//
// 在用的出口就这些：全局组、每个组，加上账号自己写的那些。健康检查查的就
// 是它们，页面列的也是它们 —— 列那些没人走的地址，查出来的「可用」什么
// 也不说明。
func Outlets(accounts []store.Account, groups map[string][]string) []Outlet {
	var out []Outlet
	seen := map[string]bool{}
	add := func(name, url string) {
		u := strings.TrimSpace(url)
		if u == "" || seen[u] {
			return
		}
		seen[u] = true
		out = append(out, Outlet{Name: name, URL: u})
	}
	// 全局组先来：它是所有人都跟着的那一个。
	for _, u := range groups[store.GlobalGroup] {
		add(DefaultOutlet, u)
	}
	names := make([]string, 0, len(groups))
	for n := range groups {
		if n == store.GlobalGroup {
			continue
		}
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		for _, u := range groups[n] {
			add(n, u)
		}
	}
	for _, a := range accounts {
		add(OwnOutlet, a.ProxyURL)
	}
	return out
}

// OutletURLs gives the addresses of the outlets, in order.
func OutletURLs(accounts []store.Account, groups map[string][]string) []string {
	outs := Outlets(accounts, groups)
	urls := make([]string, 0, len(outs))
	for _, o := range outs {
		urls = append(urls, o.URL)
	}
	return urls
}

```

`internal/egress/egress.go` 的 import 里加上 `"sort"`。

- [ ] **Step 6: 跑测试，确认它过了**

Run: `go test ./internal/egress/ -v`
Expected: PASS。

- [ ] **Step 7: 提交**

```bash
git add internal/egress/egress.go internal/egress/groups.go internal/egress/groups_test.go
git commit -F .git/commit_msg.txt
```

`.git/commit_msg.txt` 写：

```
feat(egress): 出口是四级的，组里的地址按健康排

候选地址：账号自己的地址 → 账号指着的组 → 全局组 → 直连。组里的顺序是
探通过的、没探过的、探失败的（探的结果可能是旧的）。

Unreachable 只认「根本没连上」那几种错（dial、proxyconnect、代理自己的
域名解析不了）：上游慢、超时、TLS 错不算代理的错 —— 全记的话一次抖动就
把一条好路踢掉了。
```

---

### Task 4: 开机时种组

**Files:**
- Modify: `internal/store/proxy_groups.go`（`SeedProxyGroups`、`AccountGroups`）
- Modify: `internal/config/config.go`（`Proxy.Groups`）
- Modify: `cmd/mmxcode/root.go`（开机种组、说那些话、给解析器灌组）
- Test: `internal/store/proxy_groups_test.go`

- [ ] **Step 1: 写失败的测试**

加到 `internal/store/proxy_groups_test.go`：

```go
// TestTheGlobalGroupIsMadeFromTheConfiguration says the global group is
// made from proxy.default when the database does not have it.
//
// 全局出口也是组：配置里的那个地址是它的种子。种子用过之后配置里那个就
// 不再管事了（页面上改的是库里那份）。
func TestTheGlobalGroupIsMadeFromTheConfiguration(t *testing.T) {
	s := openTest(t)
	res, err := s.SeedProxyGroups(context.Background(), "http://global:8080", nil)
	if err != nil {
		t.Fatalf("seed the groups: %v", err)
	}
	if !res.GlobalSeeded {
		t.Error("the global group was not made from the configuration")
	}
	g, err := s.GetProxyGroup(context.Background(), GlobalGroup)
	if err != nil {
		t.Fatalf("get the global group: %v", err)
	}
	if len(g.URLs) != 1 || g.URLs[0] != "http://global:8080" {
		t.Errorf("the global group's addresses are %v", g.URLs)
	}
}

// TestTheGroupsInTheConfigurationAreSeeded says a group in the
// configuration is made when the database does not have it.
//
// 老库里有一批账号指着组名（组名指向的地址在配置文件里）。组回到库里，
// 这些号不用动就还指着原来的那条路。
func TestTheGroupsInTheConfigurationAreSeeded(t *testing.T) {
	s := openTest(t)
	res, err := s.SeedProxyGroups(context.Background(), "",
		map[string]string{"clash": "http://clash:8080"})
	if err != nil {
		t.Fatalf("seed the groups: %v", err)
	}
	if len(res.Created) != 1 || res.Created[0] != "clash" {
		t.Errorf("the groups that were made are %v", res.Created)
	}
	g, err := s.GetProxyGroup(context.Background(), "clash")
	if err != nil {
		t.Fatalf("get the group: %v", err)
	}
	if len(g.URLs) != 1 || g.URLs[0] != "http://clash:8080" {
		t.Errorf("the group's addresses are %v", g.URLs)
	}
}

// TestTheGroupInTheDatabaseCounts says the group in the database is not
// touched by the one in the configuration.
//
// 库里是老家：人在页面上改过的组，配置里的那个不算数。
func TestTheGroupInTheDatabaseCounts(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now()
	if err := s.SaveProxyGroup(ctx, ProxyGroup{
		Name: "clash", URLs: []string{"http://mine:8080"}, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("save the group: %v", err)
	}
	res, err := s.SeedProxyGroups(ctx, "", map[string]string{"clash": "http://theirs:8080"})
	if err != nil {
		t.Fatalf("seed the groups: %v", err)
	}
	if len(res.Shadowed) != 1 || res.Shadowed[0] != "clash" {
		t.Errorf("the groups that were shadowed are %v", res.Shadowed)
	}
	g, err := s.GetProxyGroup(ctx, "clash")
	if err != nil {
		t.Fatalf("get the group: %v", err)
	}
	if len(g.URLs) != 1 || g.URLs[0] != "http://mine:8080" {
		t.Errorf("the group's addresses are %v", g.URLs)
	}
}

// TestTheGroupNameThatIsNotThereIsSaid says an account that points at a
// group that is not in the database is named.
//
// 那个账号跟着全局组走（组名留着：人把组建出来，它就自己回去了）。但人
// 该知道有账号指着一条不存在的路。
func TestTheGroupNameThatIsNotThereIsSaid(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now()
	makeAccount(t, s, "acc_1")
	if err := s.SetAccountOutlet(ctx, "acc_1", "", "hk", now); err != nil {
		t.Fatalf("set the outlet: %v", err)
	}
	res, err := s.SeedProxyGroups(ctx, "http://global:8080", nil)
	if err != nil {
		t.Fatalf("seed the groups: %v", err)
	}
	if len(res.Unknown) != 1 || res.Unknown[0] != "hk" {
		t.Errorf("the unknown groups are %v", res.Unknown)
	}
	if res.UnknownAccounts != 1 {
		t.Errorf("%d account(s) point at a group that is not there", res.UnknownAccounts)
	}
}

```

- [ ] **Step 2: 跑测试，确认它失败**

Run: `go test ./internal/store/ -run "TestTheGlobalGroupIsMade|TestTheGroupsInTheConfiguration|TestTheGroupInTheDatabase|TestTheGroupNameThatIsNotThere" -v`
Expected: FAIL —— `undefined: SeedProxyGroups`。

- [ ] **Step 3: 种组**

`internal/store/proxy_groups.go` 末尾加：

```go
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

```

`internal/store/proxy_groups.go` 的 import 里加上 `"sort"`。

- [ ] **Step 4: 配置里的键改个名字**

`internal/config/config.go`：`Proxy` 结构体里的 `LegacyGroups` 改名 `Groups`（yaml 键还是 `groups`）：

```go
// Proxy holds the global outlet.
//
// 出口是四级的：账号自己的地址（写在库里，页面上每一行都能改），账号指
// 着的组，然后是全局组（也是一个组，名字是保留的 default）。两个都没有
// 就是直连 —— 直连是最后一级，不是错。
type Proxy struct {
	// Default is the address the global group is made from.
	//
	// 它只是种子：开机的时候全局组不在库里，就用它建一个。组在库里之
	// 后，页面上改的是库里那份，配置里这个就不再管事了。
	Default string `yaml:"default" json:"default"`
	// Groups are the outlet groups of an older version.
	//
	// 也是种子：库里没有的组用它们建出来 —— 老库里那些指着组名的号，靠
	// 这一步还指着原来的那条路。库里已经有了的就是库里的算数。
	//
	// 老部署的配置文件里还有这个键，加载器得认它：认不出的键是错字，直
	// 接报错，那台机器就起不来了，而起不来的机器上的号就全废了。
	Groups map[string]string `yaml:"groups" json:"groups"`
}

```

`Default()` 里改成 `Proxy{Default: "", Groups: map[string]string{}}`，`Load` 里改成 `if c.Proxy.Groups == nil { c.Proxy.Groups = map[string]string{} }`。

`internal/config/config_test.go` 里那个 `TestTheRetiredKeysAreStillRead` 改一下：键还是认的（`proxy.groups` 和 `batch.group`），但组的键现在叫 `Groups` 了。

- [ ] **Step 5: 开机的时候**

`cmd/mmxcode/root.go`，把 `e.egr = egress.New(cfg.Proxy.Default)` 之后那一段（现在种组的那一段）换成：

```go
		e.egr = egress.New(cfg.Proxy.Default)
		// 组：库是老家，配置里的组只是种子。库里没有的组用配置里的地址
		// 建出来 —— 老库里那些指着组名的号，靠这一步还指着原来的那条路。
		res, err := e.store.SeedProxyGroups(ctx, cfg.Proxy.Default, cfg.Proxy.Groups)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: the outlet groups could not be made: %v\n", err)
		} else {
			if res.GlobalSeeded {
				fmt.Fprintf(os.Stderr, "warning: the global group was made from proxy.default in the configuration. The configuration's proxy.default does not count any more - change the global group on the page.\n")
			}
			if res.GlobalShadowed {
				fmt.Fprintf(os.Stderr, "warning: proxy.default in the configuration does not count any more: the global group is in the database. Change the global group on the page.\n")
			}
			for _, n := range res.Created {
				fmt.Fprintf(os.Stderr, "the outlet group %q was made from the configuration.\n", n)
			}
			for _, n := range res.Shadowed {
				fmt.Fprintf(os.Stderr, "warning: the outlet group %q in the configuration is already in the database. The one in the database counts. That part can be taken out of the configuration file.\n", n)
			}
			if res.UnknownAccounts > 0 {
				fmt.Fprintf(os.Stderr, "warning: %d account(s) point at an outlet group that is not in the database (%s). They follow the global group for now.\n",
					res.UnknownAccounts, strings.Join(res.Unknown, ", "))
			}
		}
		// 解析器得知道组：登录、导入、网关都按组挑地址。
		groups, err := e.store.ListProxyGroups(ctx)
		if err != nil {
			return err
		}
		e.egr.SetGroups(egress.GroupMap(groups))
```

`internal/manager/proxy_groups.go` 和 `internal/store/legacy_proxy.go` 删掉（组回来了，不用收拾了；那两个文件是上一版的收拾用的）。

`internal/manager/proxy_groups.go` 删掉之后，`MaterializeProxyGroups` 就没有了 —— 检查还有谁在用它：

Run: `grep -rn "MaterializeProxyGroups\|LegacyProxyGroups" --include=*.go .`
Expected: 只有 `cmd/mmxcode/root.go`（已经改掉了）和 `internal/manager/proxy_groups_test.go`（跟着删）。

`internal/manager/proxy_groups_test.go` 删掉。

- [ ] **Step 6: 跑测试，确认它过了**

Run: `go test ./internal/store/ ./internal/config/ ./cmd/mmxcode/ -v`
Expected: PASS。

- [ ] **Step 7: 提交**

```bash
git add internal/store/proxy_groups.go internal/store/proxy_groups_test.go internal/config/config.go internal/config/config_test.go cmd/mmxcode/root.go
git rm internal/manager/proxy_groups.go internal/manager/proxy_groups_test.go internal/store/legacy_proxy.go
git commit -F .git/commit_msg.txt
```

`.git/commit_msg.txt` 写：

```
feat(start): 组回到库里，配置里的组只是种子

开机的时候：库里没有的组用配置里的地址建出来（老库里那些指着组名的号，靠
这一步还指着原来的那条路）；库里已经有了的就是库里的算数，stderr 上点
这个名。全局组（default）总是有的，配置里的 proxy.default 是它的种子。

上一版那个「把组名搬成账号自己的地址」的收拾退役了：组回来了，不用搬。
```

---

### Task 5: 请求失败时换地址

**Files:**
- Modify: `internal/manager/manager.go`（`Candidates`、`RefreshOutlets`、`MarkEgressBad`）
- Modify: `internal/gateway/gateway.go`（内层那个重试循环）
- Test: `internal/gateway/gateway_test.go`

- [ ] **Step 1: 写失败的测试**

加到 `internal/gateway/gateway_test.go`（那个文件里已经有起网关的帮手，用它）：

```go
// TestTheRequestGoesToTheNextAddress says a request whose address does
// not answer is asked again through the next one.
//
// 路不通的时候还坐着不动，就是把预算全烧在一个死地址上 —— 而换的号多半
// 还走同一个全局代理。
func TestTheRequestGoesToTheNextAddress(t *testing.T) {
	// 一个连不上的地址（127.0.0.1:1 上没有人在听），后面跟一个能答的。
	dead := "http://127.0.0.1:1"
	var hits int
	alive := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"type":"message","content":[{"type":"text","text":"ok"}]}`))
	}))
	defer alive.Close()

	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the request did not go through the proxy: %s", r.URL)
	})
	ctx := context.Background()
	if err := h.store.SetAccountOutlet(ctx, "acc_01", "", "pool", time.Now()); err != nil {
		t.Fatal(err)
	}
	h.gw.Mgr.Egress.SetGroups(map[string][]string{
		"pool": {dead, alive.URL},
	})

	body := `{"model":"MiniMax-M3","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"text":"ok"`) {
		t.Fatalf("body = %s", w.Body.String())
	}
	if hits == 0 {
		t.Fatal("the request did not go through the second address")
	}
}

```

- [ ] **Step 2: 跑测试，确认它失败**

Run: `go test ./internal/gateway/ -run TestTheRequestGoesToTheNextAddress -v`
Expected: FAIL —— `undefined: addGroup`（这个帮手要在这个任务里写，见下面 Step 4）。

- [ ] **Step 3: 管理器给出候选地址**

`internal/manager/manager.go` 加：

```go
// Candidates gives the addresses an account's requests may go through, in
// the order they are tried.
//
// 网关拿到的是一张表，不是一个地址：路不通的时候还有下一条。
func (m *Manager) Candidates(ctx context.Context, accountID string) ([]string, error) {
	a, err := m.Store.GetAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}
	return m.Egress.Candidates(a), nil
}

// RefreshOutlets reads the groups and their health from the database and
// gives them to the resolver.
//
// 组的地址是页面上改的，健康是探活探出来的 —— 两样都得进解析器，它挑地
// 址的时候才用得上。
func (m *Manager) RefreshOutlets(ctx context.Context) error {
	groups, err := m.Store.ListProxyGroups(ctx)
	if err != nil {
		return err
	}
	m.Egress.SetGroups(egress.GroupMap(groups))
	health, err := m.Store.EgressHealth(ctx)
	if err != nil {
		return err
	}
	for _, h := range health {
		m.Egress.SetHealth(h.URL, h.OK)
	}
	return nil
}

// MarkEgressBad writes down that an address did not answer.
//
// 只有「根本没连上」才记（egress.Unreachable）：上游慢、超时不算代理的
// 错 —— 记了就是踢掉一条好路。记了之后下一个请求不会从它开始，而健康检
// 查下一轮探通了就把记号擦掉。
//
// 写不下去不要紧：内存里的那份已经改了，而健康检查本来就会再探一遍。
func (m *Manager) MarkEgressBad(ctx context.Context, url string, cause error) error {
	if !egress.Unreachable(cause) {
		return nil
	}
	m.Egress.SetHealth(url, false)
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	return m.Store.SetEgressHealth(writeCtx, url, false, cause.Error())
}

```

- [ ] **Step 4: 网关换地址**

`internal/gateway/gateway.go`，聊天那条路的闭包里，把

```go
			client, _, err := s.Mgr.Client(ctx, plan.AccountID)
			if err != nil {
				lastErr = err
				return false
			}
```

换成

```go
			// 候选地址：账号自己的地址 → 它的组 → 全局组。路不通的时候还
			// 有下一条。
			cands, err := s.Mgr.Candidates(ctx, plan.AccountID)
			if err != nil {
				lastErr = err
				return false
			}
			ci := 0
			var transport http.RoundTripper
			if len(cands) > 0 {
				transport, err = s.Mgr.Egress.Transport(cands[0])
				if err != nil {
					lastErr = err
					return false
				}
			}
```

内层那个循环的 `for try := 0; try < attempts; try++` 里面，`s.attempt(ctx, chatReq, client.Transport, stream)` 两处都改成 `transport`。

网络错误那一段（在 `errors.As(err, &tooBig)` 之后，`lastErr = err` 之前）加上：

```go
					// 路不通：还有下一条路就立刻换。不是上游慢，是这条路不
					// 通，等没有用。
					if ci+1 < len(cands) {
						failed := cands[ci]
						ci++
						tr, terr := s.Mgr.Egress.Transport(cands[ci])
						if terr != nil {
							lastErr = terr
							return false
						}
						transport = tr
						// 换掉的那条路记一笔坏的：下一个请求不该从它开始。
						_ = s.Mgr.MarkEgressBad(ctx, failed, err)
						// 换地址这件事要说一声：看着是「那个号走了 A 路」，其
						// 实走了 B 路。
						fmt.Printf("egress: the account %s moved from %s to %s\n",
							plan.AccountID, egress.Redacted(failed), egress.Redacted(cands[ci]))
						// 新的一条路还没有预算：try 归零（循环末尾会加一，
						// 所以是 -1）。换的是路，不是重试。
						try = -1
						continue
					}
```

`internal/gateway/gateway.go` 的 import 里加上 `"github.com/lingoo12138/mmxcode/internal/egress"`（`fmt` 已经有了）。

- [ ] **Step 5: 跑测试，确认它过了**

Run: `go test ./internal/gateway/ -run TestTheRequestGoesToTheNextAddress -v`
Expected: PASS。

- [ ] **Step 6: 提交**

```bash
git add internal/manager/manager.go internal/gateway/gateway.go internal/gateway/gateway_test.go
git commit -F .git/commit_msg.txt
```

`.git/commit_msg.txt` 写：

```
feat(gateway): 路不通就换下一条路

内层那个重试循环现在把预算全烧在同一个死地址上：连着三次都是那个不通的地
方，然后才换号 —— 而换的号多半还走同一个全局代理。

现在是：还有下一条候选地址就立刻换（不 sleep —— 不是上游慢，是这条路不
通，等没有用），换掉的那条路记一笔坏的（只有「根本没连上」才记），新的
一条路从头给预算。
```

---

### Task 6: 健康检查查组里的地址

**Files:**
- Modify: `internal/scheduler/scheduler.go`（`CheckEgress`）
- Test: `internal/scheduler/scheduler_test.go`

- [ ] **Step 1: 写失败的测试**

加到 `internal/scheduler/scheduler_test.go`：

```go
// TestTheAddressesInTheGroupsAreChecked says the health check probes the
// addresses that are in the groups.
//
// 组里的地址也是有人在走的路：没人探它，就没人知道它通了没有。
func TestTheAddressesInTheGroupsAreChecked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`ok`))
	}))
	defer srv.Close()

	ctx := context.Background()
	s, err := store.Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	s.Migrate(ctx)
	defer s.Close()

	now := time.Now()
	if err := s.SaveProxyGroup(ctx, store.ProxyGroup{
		Name: "clash", URLs: []string{"http://clash:8080"},
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	up := upstream.New(upstream.Options{Timeout: 5 * time.Second})
	acc := account.New(up)
	mgr := manager.New(s, up, acc, egress.New(""), oauth.NewRenewer())
	sc := New(mgr, config.Default())
	sc.target = srv.URL

	sc.CheckEgress(ctx)

	rows, err := s.EgressHealth(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range rows {
		if r.URL == "http://clash:8080" {
			found = true
		}
	}
	if !found {
		t.Errorf("the address in the group was not checked: %+v", rows)
	}
}

```

- [ ] **Step 2: 跑测试，确认它失败**

Run: `go test ./internal/scheduler/ -run TestTheAddressesInTheGroupsAreChecked -v`
Expected: FAIL —— `undefined: addGroup`。

- [ ] **Step 3: 健康检查按组查**

`internal/scheduler/scheduler.go` 的 `CheckEgress` 改成：

```go
// CheckEgress tests every outlet that is in use and records the result.
//
// 查的是在用的那些：全局默认，加上账号自己写的地址。查那些没人走的地
// 址没有意义 —— 「可用」什么也不说明。
//
// The probe and the write have a context each. The probe's timeout is
// over when the answer is in, and a write under that context would be
// thrown away before it reaches the database: the health table would
// stay empty and the WebUI would say "not checked" for ever.
func (s *Scheduler) CheckEgress(ctx context.Context) {
	target := s.target
	if target == "" {
		target = egressTarget
	}
	accounts, err := s.Mgr.Store.ListAccounts(ctx)
	if err != nil {
		fmt.Printf("egress: %v\n", err)
		return
	}
	gs, err := s.Mgr.Store.ListProxyGroups(ctx)
	if err != nil {
		fmt.Printf("egress: %v\n", err)
		return
	}
	// 库里还没有全局组的时候，配置里那个全局默认就是全局组：解析器也是
	// 这么挑的地址，查的得是同一个。
	groups := egress.GroupsWithDefault(egress.GroupMap(gs), s.Cfg.Proxy.Default)
	for _, url := range egress.OutletURLs(accounts, groups) {
		probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err := s.Mgr.Egress.Check(probeCtx, url, target)
		cancel()
		msg := ""
		if err != nil {
			msg = err.Error()
		}
		// 内存里的那份也更新：下一个请求不该从一个刚探到不通的地址开始。
		s.Mgr.Egress.SetHealth(url, err == nil)
		// The write runs under a context of its own: the caller's may
		// be over already, and the health of the outlet has to be
		// written down all the same.
		writeCtx, writeCancel := context.WithTimeout(context.WithoutCancel(ctx), egressWriteTimeout)
		_ = s.Mgr.Store.SetEgressHealth(writeCtx, url, err == nil, msg)
		writeCancel()
	}
}

```

- [ ] **Step 4: 跑测试，确认它过了**

Run: `go test ./internal/scheduler/ -v`
Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/scheduler/scheduler.go internal/scheduler/scheduler_test.go
git commit -F .git/commit_msg.txt
```

`.git/commit_msg.txt` 写：

```
feat(scheduler): 健康检查查组里的地址

查的是在用的那些：全局组、每个组、账号自己写的地址。组里的地址也是有人
在走的路 —— 没人探它，就没人知道它通了没有。

探完顺手把结果灌进解析器：下一个请求不该从一个刚探到不通的地址开始。
```

---

### Task 7: 页面和 API

**Files:**
- Modify: `internal/webui/webui.go`（路由、组的 API、账号的 PATCH、两页的数据）
- Modify: `internal/webui/templates/accounts.html`（组的下拉框）
- Modify: `internal/webui/templates/egress.html`（组的那一段）
- Modify: `internal/webui/static/app.js`（改组、删组、地址和组二选一）
- Test: `internal/webui/webui_test.go`

- [ ] **Step 1: 写失败的测试**

加到 `internal/webui/webui_test.go`：

```go
// do asks the web UI a question and gives back the answer.
//
// 组的那几个接口要问的种类多：一个请求走一遍 recorder，比抄一遍四行强。
func (w *WebUI) do(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, r)
	return rec
}

// TestTheGroupIsMadeAndChanged says a group can be made and changed
// through the API.
//
// 组在库里，页面上能改 —— 这是这一版跟上一版最不一样的地方。
func TestTheGroupIsMadeAndChanged(t *testing.T) {
	w, _ := newTestWebUI(t)
	body := `{"name":"clash","urls":["http://a:8080","http://b:8080"]}`
	rec := w.do(t, http.MethodPost, "/api/proxy-groups", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("the answer is %d: %s", rec.Code, rec.Body.String())
	}
	g, err := w.Store.GetProxyGroup(context.Background(), "clash")
	if err != nil {
		t.Fatalf("get the group: %v", err)
	}
	if len(g.URLs) != 2 {
		t.Errorf("the group's addresses are %v", g.URLs)
	}
	// 改组：地址换掉。
	rec = w.do(t, http.MethodPost, "/api/proxy-groups", `{"name":"clash","urls":["http://c:8080"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("the answer is %d: %s", rec.Code, rec.Body.String())
	}
	g, err = w.Store.GetProxyGroup(context.Background(), "clash")
	if err != nil {
		t.Fatalf("get the group: %v", err)
	}
	if len(g.URLs) != 1 || g.URLs[0] != "http://c:8080" {
		t.Errorf("the group's addresses are %v", g.URLs)
	}
}

// TestTheGroupIsTakenAway says a group can be taken away, and that the
// accounts that pointed at it go back to the global group.
func TestTheGroupIsTakenAway(t *testing.T) {
	w, _ := newTestWebUI(t)
	ctx := context.Background()
	now := time.Now()
	if err := w.Store.CreateAccount(ctx, store.Account{
		ID: "acc_1", Name: "one", Status: store.StatusActive, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create the account: %v", err)
	}
	if err := w.Store.SetAccountOutlet(ctx, "acc_1", "", "clash", now); err != nil {
		t.Fatalf("set the outlet: %v", err)
	}
	rec := w.do(t, http.MethodPost, "/api/proxy-groups", `{"name":"clash","urls":["http://a:8080"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("the answer is %d: %s", rec.Code, rec.Body.String())
	}
	rec = w.do(t, http.MethodDelete, "/api/proxy-groups?name=clash", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("the answer is %d: %s", rec.Code, rec.Body.String())
	}
	a, err := w.Store.GetAccount(ctx, "acc_1")
	if err != nil {
		t.Fatalf("get the account: %v", err)
	}
	if a.ProxyGroup != "" {
		t.Errorf("the account still points at the group %q", a.ProxyGroup)
	}
}

// TestTheAccountPointsAtAGroup says an account can be pointed at a group
// through the API, and that the address and the group are one thing.
func TestTheAccountPointsAtAGroup(t *testing.T) {
	w, _ := newTestWebUI(t)
	ctx := context.Background()
	now := time.Now()
	if err := w.Store.CreateAccount(ctx, store.Account{
		ID: "acc_1", Name: "one", Status: store.StatusActive, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create the account: %v", err)
	}
	rec := w.do(t, http.MethodPatch, "/api/accounts/acc_1", `{"proxy_group":"clash"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("the answer is %d: %s", rec.Code, rec.Body.String())
	}
	a, err := w.Store.GetAccount(ctx, "acc_1")
	if err != nil {
		t.Fatalf("get the account: %v", err)
	}
	if a.ProxyGroup != "clash" {
		t.Errorf("the account's group is %q", a.ProxyGroup)
	}
	// 两个一起给是不行的：账号只有一条出口。
	rec = w.do(t, http.MethodPatch, "/api/accounts/acc_1",
		`{"proxy_group":"clash","proxy_url":"http://a:8080"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("the answer is %d: %s", rec.Code, rec.Body.String())
	}
}
```

- [ ] **Step 2: 跑测试，确认它失败**

Run: `go test ./internal/webui/ -run "TestTheGroupIs|TestTheAccountPointsAtAGroup" -v`
Expected: FAIL —— 404（没有这个接口）。

- [ ] **Step 3: 组的接口**

`internal/webui/webui.go` 的路由里加：

```go
	case "/api/proxy-groups":
		w.apiProxyGroups(rw, r)
		return
```

加上这些函数：

```go
// apiProxyGroups reads and writes the outlet groups.
//
// 组在库里，页面上能改：建一个、改一个、删一个。删掉的组，指着它的账号
// 回到全局组去。
func (w *WebUI) apiProxyGroups(rw http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		w.apiProxyGroupsList(rw, r)
	case http.MethodPost:
		w.apiProxyGroupsSave(rw, r)
	case http.MethodDelete:
		w.apiProxyGroupsDelete(rw, r)
	default:
		rw.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// apiProxyGroupsList gives the groups.
func (w *WebUI) apiProxyGroupsList(rw http.ResponseWriter, r *http.Request) {
	groups, err := w.Store.ListProxyGroups(r.Context())
	if err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}
	counts, err := w.Store.AccountGroups(r.Context())
	if err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}
	out := make([]map[string]any, 0, len(groups))
	for _, g := range groups {
		out = append(out, map[string]any{
			"Name":     g.Name,
			"Global":   g.Name == store.GlobalGroup,
			"URLs":     g.URLs,
			"Accounts": counts[g.Name],
		})
	}
	writeJSON(rw, map[string]any{"Groups": out})
}

// apiProxyGroupsSave makes a group or changes it.
//
// 名字是组的身份：这个名字已经有了，就是改它的地址。
func (w *WebUI) apiProxyGroupsSave(rw http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string   `json:"name"`
		URLs []string `json:"urls"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		http.Error(rw, "the group has no name", http.StatusBadRequest)
		return
	}
	// 页面显示的是去掉凭据的地址，送回来的也是去了凭据的：没改过的那
	// 几条，存回去的是带凭据的那一个。
	var stored []string
	if old, err := w.Store.GetProxyGroup(r.Context(), name); err == nil {
		stored = old.URLs
	}
	urls := mergeAddresses(body.URLs, stored)
	now := time.Now()
	if err := w.Store.SaveProxyGroup(r.Context(), store.ProxyGroup{
		Name: name, URLs: urls, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}
	// 解析器得马上知道：网关下一次挑地址就该按新的来。
	if err := w.Mgr.RefreshOutlets(r.Context()); err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(rw, map[string]any{"ok": true})
}

// apiProxyGroupsDelete takes a group away.
func (w *WebUI) apiProxyGroupsDelete(rw http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		http.Error(rw, "the group has no name", http.StatusBadRequest)
		return
	}
	back, err := w.Store.DeleteProxyGroup(r.Context(), name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(rw, "no such group: "+name, http.StatusNotFound)
			return
		}
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}
	_ = w.Mgr.RefreshOutlets(r.Context())
	writeJSON(rw, map[string]any{"ok": true, "back": back})
}

// mergeAddresses puts the credentials back into the addresses that were
// not really changed.
//
// 页面显示的是去掉凭据的地址（凭据是秘密），送回来的也是去了凭据的。
// 没改过的那几条，存回去的是带凭据的那一个 —— 否则改一下别的地址就把凭
// 据抹了。
func mergeAddresses(shown, stored []string) []string {
	used := make([]bool, len(stored))
	out := make([]string, 0, len(shown))
	for _, s := range shown {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		matched := false
		for i, old := range stored {
			if used[i] {
				continue
			}
			if s == egress.Redacted(old) {
				out = append(out, old)
				used[i] = true
				matched = true
				break
			}
		}
		if !matched {
			out = append(out, s)
		}
	}
	return out
}

```

- [ ] **Step 4: 账号的 PATCH 多一列组名**

`internal/webui/webui.go` 的 `apiAccount` 里，PATCH 的 body 改成：

```go
		var body struct {
			Name       *string `json:"name"`
			ProxyURL   *string `json:"proxy_url"`
			ProxyGroup *string `json:"proxy_group"`
		}
```

在 `if body.ProxyURL != nil {` 那一段之前加上：

```go
		if body.ProxyURL != nil && body.ProxyGroup != nil {
			// 地址和组是二选一：账号只有一条出口。
			http.Error(rw, "the outlet is an address or a group, not both", http.StatusBadRequest)
			return
		}
```

`if body.ProxyURL != nil {` 那一段改成：

```go
		if body.ProxyURL != nil {
			// The page shows the address without its credentials, so
			// the address that comes back unchanged is the one that
			// is stored, and not the redacted one. An address the
			// user really changed is written as it is.
			shown := egress.Redacted(a.ProxyURL)
			if *body.ProxyURL != shown || shown == a.ProxyURL {
				a.ProxyURL = *body.ProxyURL
			}
			// 给了地址，组就清掉：账号只有一条出口。
			a.ProxyGroup = ""
		}
		if body.ProxyGroup != nil {
			a.ProxyGroup = strings.TrimSpace(*body.ProxyGroup)
			a.ProxyURL = ""
		}
```

最后写库那一段改成：

```go
		if body.ProxyURL != nil || body.ProxyGroup != nil {
			if err := w.Store.SetAccountOutlet(r.Context(), a.ID, a.ProxyURL, a.ProxyGroup, time.Now()); err != nil {
				http.Error(rw, err.Error(), http.StatusInternalServerError)
				return
			}
		}
```

- [ ] **Step 5: 两页的数据**

`internal/webui/webui.go` 的 `accountsData` 改成（多读一次组，多给两个字段）：

```go
// accountsData is the accounts page's data.
func (w *WebUI) accountsData(ctx context.Context) (map[string]any, error) {
	accounts, err := w.Store.ListAccounts(ctx)
	if err != nil {
		return nil, err
	}
	groups, err := w.Store.ListProxyGroups(ctx)
	if err != nil {
		return nil, err
	}
	snaps, err := w.Store.LatestUsageSnapshots(ctx)
	if err != nil {
		return nil, err
	}
	byAccount := map[string]store.UsageSnapshot{}
	for _, s := range snaps {
		byAccount[s.AccountID] = s
	}
	out := make([]map[string]any, 0, len(accounts))
	for _, a := range accounts {
		snap := byAccount[a.ID]
		checkedIn, streak := w.todayCheckin(ctx, a.ID)
		out = append(out, map[string]any{
			"ID":            a.ID,
			"Name":          a.Name,
			"Email":         a.Email,
			"Status":        a.Status,
			"ProxyURL":      egress.Redacted(a.ProxyURL),
			"ProxyGroup":    a.ProxyGroup,
			"HasAmounts":    snap.HasAmounts,
			"Granted":       snap.Granted,
			"Remaining":     snap.Remaining,
			"Plan":          snap.Plan,
			"WindowsError":  snap.WindowsError,
			"WindowSummary": w.windowSummary(ctx, a.ID),
			"CheckedIn":     checkedIn,
			"Streak":        streak,
			// 账号在 error 或者 signed_out 的时候，光有状态是没用的：
			// 上一次为什么失败，才是那个人要做的事。
			"LastError": a.LastError,
		})
	}
	return map[string]any{"Accounts": out, "Groups": groupList(groups)}, nil
}

// groupList gives the groups for the page: the name and how many
// addresses it has.
func groupList(groups []store.ProxyGroup) []map[string]any {
	out := make([]map[string]any, 0, len(groups))
	for _, g := range groups {
		out = append(out, map[string]any{
			"Name":   g.Name,
			"Global": g.Name == store.GlobalGroup,
			"Count":  len(g.URLs),
		})
	}
	return out
}

```

`egressData` 改成（上面是组，下面是在用的地址）：

```go
// egressData is the outlets page's data.
//
// 列的是在用的那些：全局默认，加上账号自己写的地址。列那些没人走的地址
// 没有意义 —— 「可用」什么也不说明。
func (w *WebUI) egressData(ctx context.Context) (map[string]any, error) {
	health, err := w.Store.EgressHealth(ctx)
	if err != nil {
		return nil, err
	}
	byURL := map[string]store.EgressHealth{}
	for _, h := range health {
		byURL[h.URL] = h
	}
	accounts, err := w.Store.ListAccounts(ctx)
	if err != nil {
		return nil, err
	}
	groups, err := w.Store.ListProxyGroups(ctx)
	if err != nil {
		return nil, err
	}
	// 组：一个组一行 —— 名字、几条地址、那些地址（去掉凭据的）。
	var groupRows []map[string]any
	for _, g := range groups {
		text := make([]string, 0, len(g.URLs))
		for _, u := range g.URLs {
			text = append(text, egress.Redacted(u))
		}
		groupRows = append(groupRows, map[string]any{
			"Name":   g.Name,
			"Global": g.Name == store.GlobalGroup,
			"Count":  len(g.URLs),
			"Text":   strings.Join(text, "\n"),
		})
	}
	// 在用的地址：从哪来、地址本身、健不健康。
	// 库里还没有全局组的时候，配置里那个全局默认就是全局组。
	gm := egress.GroupsWithDefault(egress.GroupMap(groups), w.Cfg.Proxy.Default)
	var out []map[string]any
	for _, o := range egress.Outlets(accounts, gm) {
		h, ok := byURL[o.URL]
		out = append(out, map[string]any{
			"Name": o.Name, "URL": egress.Redacted(o.URL),
			"OK": ok && h.OK, "Checked": ok, "Error": h.Error,
		})
	}
	return map[string]any{"Groups": groupRows, "Egress": out}, nil
}

```

- [ ] **Step 6: 两个模板**

`internal/webui/templates/accounts.html` 的出口那一格换成：

```html
  <td>
    <select data-account="{{$acc.ID}}" data-field="proxy_group">
      <option value="">跟随全局</option>
      {{range $.Groups}}<option value="{{.Name}}"{{if eq .Name $acc.ProxyGroup}} selected{{end}}>{{.Name}}</option>{{end}}
    </select>
    <input size="24" placeholder="单独代理地址（空 = 跟随全局）" data-account="{{$acc.ID}}" data-field="proxy_url" value="{{$acc.ProxyURL}}">
  </td>
```

`internal/webui/templates/egress.html` 整个文件：

```html
{{define "content"}}
<h2>出口</h2>
<h3>组</h3>
<table>
<tr><th>名字</th><th>地址</th><th></th></tr>
{{range .Groups}}
<tr>
  <td>{{.Name}}{{if .Global}} <span class="muted">全局默认</span>{{end}}</td>
  <td class="muted">{{.Count}} 条</td>
  <td>
    <button data-group-edit="{{.Name}}" data-group-urls="{{.Text}}">改</button>
    {{if not .Global}}<button data-group-rm="{{.Name}}">删</button>{{end}}
  </td>
</tr>
{{end}}
</table>
{{if not .Groups}}<p class="muted">还没有组。<code>mmxcode groups set --name X --url 地址</code> 建一个。</p>{{end}}
<h3>在用的地址</h3>
<table>
<tr><th>从哪来</th><th>地址</th><th>健康</th><th></th></tr>
{{range .Egress}}
<tr>
  <td>{{.Name}}</td>
  <td class="muted">{{.URL}}</td>
  <td>{{if .OK}}<span class="good">可用</span>{{else if .Checked}}<span class="bad">不可用：{{.Error}}</span>{{else}}<span class="muted">未检查</span>{{end}}</td>
  <td><button data-egress="{{.URL}}">测试</button></td>
</tr>
{{end}}
</table>
{{end}}
```

- [ ] **Step 7: 页面上的脚本**

`internal/webui/static/app.js` 的 `click` 监听里加（在 `data-egress` 那段之后）：

```js
  const groupEdit = ev.target.closest('[data-group-edit]');
  if (groupEdit) {
    editGroup(groupEdit.dataset.groupEdit, groupEdit.dataset.groupUrls || '');
    return;
  }

  const groupRm = ev.target.closest('[data-group-rm]');
  if (groupRm) {
    const name = groupRm.dataset.groupRm;
    if (!confirm(`删掉组 ${name}？指着它的账号回到全局组。`)) return;
    groupRm.disabled = true;
    try {
      await del(`/api/proxy-groups?name=${encodeURIComponent(name)}`);
      location.reload();
    } catch (e) {
      document.getElementById('status').textContent = '删除失败：' + e.message;
      groupRm.disabled = false;
    }
    return;
  }
```

文件末尾加这个（改组）：

```js
// editGroup 改一个组。
//
// 组里的地址一行一个，顺序就是故障转移的顺序：第一个是主路，后面的都是
// 备路。
function editGroup(name, urls) {
  const box = document.createElement('div');
  box.className = 'card';
  const label = document.createElement('p');
  label.textContent = `组 ${name} 的地址（一行一个，第一个是主路）`;
  const input = document.createElement('textarea');
  input.rows = 6;
  input.cols = 48;
  input.value = urls;
  const button = document.createElement('button');
  button.textContent = '存回去';
  const msg = document.createElement('p');
  box.append(label, input, button, msg);
  button.addEventListener('click', async () => {
    button.disabled = true;
    try {
      const list = input.value.split('\n').map((s) => s.trim()).filter(Boolean);
      await post('/api/proxy-groups', { name, urls: list });
      location.reload();
    } catch (e) {
      msg.textContent = e.message;
      button.disabled = false;
    }
  });
  document.body.appendChild(box);
}
```

`change` 监听里改成（地址和组二选一）：

```js
document.addEventListener('change', async (ev) => {
  const sel = ev.target.closest('[data-field]');
  if (!sel) return;
  const body = {};
  body[sel.dataset.field] = sel.value;
  // 地址和组是二选一：选了组，地址框就清掉；填了地址，下拉框就回到空。
  if (sel.dataset.field === 'proxy_group' && sel.value) {
    const input = document.querySelector(`input[data-account="${sel.dataset.account}"][data-field="proxy_url"]`);
    if (input) input.value = '';
  }
  if (sel.dataset.field === 'proxy_url' && sel.value) {
    const pick = document.querySelector(`select[data-account="${sel.dataset.account}"][data-field="proxy_group"]`);
    if (pick) pick.value = '';
  }
  try {
    await patch(`/api/accounts/${sel.dataset.account}`, body);
    document.getElementById('status').textContent = '已保存';
  } catch (e) {
    document.getElementById('status').textContent = '保存失败：' + e.message;
  }
});
```

- [ ] **Step 8: 跑测试，确认它过了**

Run: `go test ./internal/webui/ -v`
Expected: PASS。

- [ ] **Step 9: 提交**

```bash
git add internal/webui/webui.go internal/webui/webui_test.go internal/webui/templates/accounts.html internal/webui/templates/egress.html internal/webui/static/app.js
git commit -F .git/commit_msg.txt
```

`.git/commit_msg.txt` 写：

```
feat(webui): 出口组在页面上能建能改能删

组在库里，页面上就能管：出口页上面是组（名字、几条地址、几个号指着、改和
删），下面是在用的地址（从哪来、地址本身、健不健康）。

账号页多一个「出口组」下拉框。地址和组是二选一：选了组，地址框清掉；填
了地址，下拉框回到空。

送回来的地址是去了凭据的（凭据是秘密），没改过的那几条存回去的是带凭据
的那一个 —— 否则改一下别的地址就把凭据抹了。
```

---

### Task 8: 命令行

**Files:**
- Create: `cmd/mmxcode/groups.go`
- Create: `cmd/mmxcode/groups_test.go`
- Modify: `cmd/mmxcode/accounts.go`（`--group`、列表多一列）
- Modify: `cmd/mmxcode/root.go`（挂上 `groups`）
- Test: `cmd/mmxcode/accounts_test.go`

- [ ] **Step 1: 写失败的测试**

`cmd/mmxcode/groups_test.go` 整个文件：

```go
package main

import (
	"context"
	"strings"
	"testing"
)

// TestTheGroupIsMadeAndChanged says a group can be made and changed from
// the command line.
func TestTheGroupIsMadeAndChanged(t *testing.T) {
	e := testEnv(t)
	ctx := context.Background()
	cmd := newGroupsSetCmd(e)
	cmd.SetArgs([]string{"--name", "clash", "--url", "http://a:8080", "--url", "http://b:8080"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("set the group: %v", err)
	}
	g, err := e.store.GetProxyGroup(ctx, "clash")
	if err != nil {
		t.Fatalf("get the group: %v", err)
	}
	if len(g.URLs) != 2 || g.URLs[0] != "http://a:8080" || g.URLs[1] != "http://b:8080" {
		t.Errorf("the group's addresses are %v", g.URLs)
	}
}

// TestTheGroupIsListed says the list shows the groups and their
// addresses, without the credentials.
//
// 地址里的账号和密码是秘密，而这份输出会进到终端的回滚里，也会进到 CI
// 的日志里。
func TestTheGroupIsListed(t *testing.T) {
	e := testEnv(t)
	cmd := newGroupsSetCmd(e)
	cmd.SetArgs([]string{"--name", "clash", "--url", "socks5h://bob:hunter2@127.0.0.1:1080"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("set the group: %v", err)
	}
	out := captureStdout(t, func() {
		cmd := newGroupsListCmd(e)
		cmd.SetOut(nil)
		cmd.SetErr(nil)
		if err := cmd.Execute(); err != nil {
			t.Errorf("groups list: %v", err)
		}
	})
	if strings.Contains(out, "hunter2") || strings.Contains(out, "bob") {
		t.Errorf("the proxy's password is in the output:\n%s", out)
	}
	if !strings.Contains(out, "clash") {
		t.Errorf("the group is not in the output:\n%s", out)
	}
}
```

`cmd/mmxcode/accounts_test.go` 里加两个：

```go
// TestTheAccountCanPointAtAGroup says an account's outlet can be a
// group, and that the address and the group are one thing.
//
// 地址和组是二选一：账号只有一条出口。给了组，账号自己的地址就不算了。
func TestTheAccountCanPointAtAGroup(t *testing.T) {
	e := testEnv(t)
	ctx := context.Background()
	now := time.Now()
	if err := e.store.CreateAccount(ctx, store.Account{
		ID: "acc_1", Name: "一号", Status: store.StatusActive,
		ProxyURL:  "http://old:8080",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create account: %v", err)
	}

	cmd := newAccountsProxyCmd(e)
	cmd.SetArgs([]string{"acc_1", "--group", "clash"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("accounts proxy: %v", err)
	}
	a, err := e.store.GetAccount(ctx, "acc_1")
	if err != nil {
		t.Fatal(err)
	}
	if a.ProxyGroup != "clash" {
		t.Errorf("the account's group is %q, want %q", a.ProxyGroup, "clash")
	}
	// 给了组，账号自己的地址就不算了。
	if a.ProxyURL != "" {
		t.Errorf("the account still has an address of its own: %q", a.ProxyURL)
	}

	// 两个一起给是不行的。
	cmd = newAccountsProxyCmd(e)
	cmd.SetArgs([]string{"acc_1", "--url", "http://a:8080", "--group", "clash"})
	if err := cmd.Execute(); err == nil {
		t.Error("the address and the group were both taken")
	}
}

// TestTheAccountListSaysTheGroup says the list says which group an
// account is on.
//
// 一列「出口组」：人不用去猜这个号走的是哪条路。
func TestTheAccountListSaysTheGroup(t *testing.T) {
	e := testEnv(t)
	now := time.Now()
	if err := e.store.CreateAccount(context.Background(), store.Account{
		ID: "acc_1", Name: "一号", Status: store.StatusActive,
		ProxyGroup: "clash", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create account: %v", err)
	}

	out := captureStdout(t, func() {
		cmd := newAccountsListCmd(e)
		cmd.SetOut(nil)
		cmd.SetErr(nil)
		if err := cmd.Execute(); err != nil {
			t.Errorf("accounts list: %v", err)
		}
	})
	if !strings.Contains(out, "clash") {
		t.Errorf("the group is not in the output:\n%s", out)
	}
```

- [ ] **Step 2: 跑测试，确认它失败**

Run: `go test ./cmd/mmxcode/ -run "TestTheGroupIs" -v`
Expected: FAIL —— `undefined: newGroupsSetCmd`。

- [ ] **Step 3: 命令**

`cmd/mmxcode/groups.go` 整个文件：

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/lingoo12138/mmxcode/internal/egress"
	"github.com/lingoo12138/mmxcode/internal/store"
)

func newGroupsCmd(e *env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "groups",
		Short: "出口组",
	}
	cmd.AddCommand(newGroupsListCmd(e))
	cmd.AddCommand(newGroupsSetCmd(e))
	cmd.AddCommand(newGroupsRemoveCmd(e))
	return cmd
}

func newGroupsListCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "列出出口组",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			groups, err := e.store.ListProxyGroups(ctx)
			if err != nil {
				return err
			}
			counts, err := e.store.AccountGroups(ctx)
			if err != nil {
				return err
			}
			if len(groups) == 0 {
				fmt.Println("还没有出口组。")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "组\t地址\t账号")
			for _, g := range groups {
				name := g.Name
				if g.Name == store.GlobalGroup {
					name = g.Name + "（全局默认）"
				}
				if len(g.URLs) == 0 {
					fmt.Fprintf(w, "%s\t（空）\t%d\n", name, counts[g.Name])
					continue
				}
				for i, u := range g.URLs {
					label := ""
					if i == 0 {
						label = name
					}
					// 地址里的账号和密码是秘密：打出来的是去了凭据的。
					fmt.Fprintf(w, "%s\t%s\t%d\n", label, egress.Redacted(u), counts[g.Name])
				}
			}
			return w.Flush()
		},
	}
}

func newGroupsSetCmd(e *env) *cobra.Command {
	var (
		name string
		urls []string
	)
	cmd := &cobra.Command{
		Use:   "set",
		Short: "建一个出口组或者改它",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			name = strings.TrimSpace(name)
			if name == "" {
				return fmt.Errorf("the group has no name")
			}
			if err := e.store.SaveProxyGroup(ctx, store.ProxyGroup{
				Name: name, URLs: urls, CreatedAt: nowUTC(), UpdatedAt: nowUTC(),
			}); err != nil {
				return err
			}
			fmt.Printf("the outlet group %q has %d address(es)\n", name, len(urls))
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "组的名字")
	cmd.Flags().StringArrayVar(&urls, "url", nil, "组里的地址（可以给多次，顺序就是顺序）")
	return cmd
}

func newGroupsRemoveCmd(e *env) *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "rm",
		Short: "删掉一个出口组",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			back, err := e.store.DeleteProxyGroup(ctx, name)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return fmt.Errorf("no such group: %s", name)
				}
				return err
			}
			fmt.Printf("the outlet group %q is gone; %d account(s) follow the global group now\n",
				name, back)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "组的名字")
	return cmd
}
```

（`strings` 别忘了 import。）

- [ ] **Step 4: 账号的出口多一个组**

`cmd/mmxcode/accounts.go` 的 `newAccountsProxyCmd` 改成：

```go
func newAccountsProxyCmd(e *env) *cobra.Command {
	var url, group string
	cmd := &cobra.Command{
		Use:   "proxy <id>",
		Short: "给账号指定单独的代理地址或者出口组",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			a, err := e.store.GetAccount(ctx, args[0])
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return fmt.Errorf("no such account: %s", args[0])
				}
				return err
			}
			// 地址和组是二选一：账号只有一条出口。
			if cmd.Flags().Changed("url") && cmd.Flags().Changed("group") {
				return fmt.Errorf("the outlet is an address or a group, not both")
			}
			if cmd.Flags().Changed("url") {
				a.ProxyURL = url
				a.ProxyGroup = ""
			}
			if cmd.Flags().Changed("group") {
				a.ProxyGroup = group
				a.ProxyURL = ""
			}
			// 只写出口那两列。整行写回去会把别的写者在这中间改的东西抹掉：
			// 登出刚写下的状态，会被这一次改代理抹回 active。
			return e.store.SetAccountOutlet(ctx, a.ID, a.ProxyURL, a.ProxyGroup, nowUTC())
		},
	}
	cmd.Flags().StringVar(&url, "url", "", "单独的代理地址")
	cmd.Flags().StringVar(&group, "group", "", "出口组的名字")
	return cmd
}

```

`newAccountsListCmd` 的表头和那一行改成（多一列「出口组」）：

```go
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "ID\t标签\t邮箱\t状态\t出口组\t代理")
			for _, a := range list {
				// 代理地址里的账号和密码是秘密，而这份输出会进到终端的
				// 回滚里，也会进到 CI 的日志里。egress.Redacted 就是
				// 为了这个。账号没有自己的地址就写全局默认那个。
				proxy := egress.Redacted(a.ProxyURL)
				if proxy == "" {
					proxy = egress.Redacted(e.cfg.Proxy.Default)
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
					a.ID, a.Name, a.Email, a.Status, a.ProxyGroup, proxy)
			}
```

- [ ] **Step 5: 挂上命令**

`cmd/mmxcode/root.go` 的 `root.AddCommand` 那里加一行：

```go
	root.AddCommand(newGroupsCmd(e))
```

- [ ] **Step 6: 跑测试，确认它过了**

Run: `go test ./cmd/mmxcode/ -v`
Expected: PASS。

- [ ] **Step 7: 提交**

```bash
git add cmd/mmxcode/groups.go cmd/mmxcode/groups_test.go cmd/mmxcode/accounts.go cmd/mmxcode/root.go
git commit -F .git/commit_msg.txt
```

`.git/commit_msg.txt` 写：

```
feat(cli): groups list/set/rm，账号的出口可以指一个组

groups set --name X --url A --url B 建一个组或者改它（--url 的顺序就是故
障转移的顺序）；groups rm 删掉它（指着它的账号回到全局组）。

accounts proxy 多一个 --group：地址和组是二选一。accounts list 多一列「出
口组」。
```

---

### Task 9: 文档和部署

**Files:**
- Modify: `README.md`
- Modify: `config.example.yaml`
- Modify: `config.docker.yaml`
- Modify: `.env.example`

- [ ] **Step 1: 配置样例**

`config.example.yaml` 的 `proxy:` 那一段换成：

```yaml
proxy:
  # 全局默认出口。"direct" 表示不走代理。
  # 出口是四级的：账号自己的地址（在网页上每一行都能填），账号指着的组，
  # 全局组（也是一个组，名字是 default），最后是直连。
  #
  # 这里这个地址只是全局组的种子：库里还没有全局组的时候用它建一个。
  # 组在库里之后，网页上改的是库里那份，这个键就不再管事了。
  default: "http://127.0.0.1:7890"
```

`config.docker.yaml` 里对应的那一段同样改（看那个文件里 proxy 那段本来写了什么，改成一样的话）。

- [ ] **Step 2: README**

`README.md` 的「代理模型」那一节换成：

```markdown
### 代理模型

出口按这个顺序挑：

1. 账号自己的代理地址：`mmxcode accounts proxy <id> --url <地址>`，或者在网页上
   每一行里填。
2. 账号指着的出口组：`mmxcode accounts proxy <id> --group <组名>`，或者网页上那
   个下拉框。
3. 全局组（名字是 `default`）：网页上「出口」那一页里改。
4. 直连。

一个组里有多条地址（一条也行）：组里的地址按顺序试，第一个通就用它，不通换下一
个 —— 故障转移，不是轮询。哪个地址活着由健康检查说了算（`schedule.egress_health`
那个间隔，默认两分钟）。

组在数据库里：`mmxcode groups set --name X --url A --url B` 建一个或者改它，
`mmxcode groups list` 看有哪些组，`mmxcode groups rm --name X` 删掉（指着它的账号回
到全局组）。网页上「出口」那一页也能建、改、删。

配置里的 `proxy.default` 只是全局组的种子：`proxy.groups` 是老部署的组，也是种子
—— 库里没有的组开机的时候用它们建出来，库里已经有了的就是库里的算数。
```

README 的接口表里加一行：

```markdown
| `/api/proxy-groups` | GET / POST / DELETE | 出口组（列出、建或改、删） |
```

README 的命令行那一节里，`accounts proxy --url` 后面加上：

```markdown
`accounts proxy --group <组名>`（这个账号走哪个组）、`groups list|set|rm`（出口组）。
```

- [ ] **Step 3: 提交**

```bash
git add README.md config.example.yaml config.docker.yaml .env.example
git commit -F .git/commit_msg.txt
```

`.git/commit_msg.txt` 写：

```
docs(proxy): 出口是四级的，组在库里，页面和命令行都能改

README、配置样例：proxy.default 是全局组的种子，proxy.groups 是老部署的组
的种子（库里没有的组开机时建出来）。
```

---

### Task 10: 体检

**Files:**
- Modify: `docs/superpowers/tools/check_plan_files.py`（新写的整个文件进名单）

- [ ] **Step 1: 计划里的 Go 块跟仓库对得上**

```bash
python -I docs/superpowers/tools/check_plan_files.py docs/superpowers/plans/2026-11-21-mmxcode-proxy-groups.md .
```

Expected: `blocks: N, unparseable: 0, stale declarations: 0`。

`WHOLE` 那份名单是攒着的：别的计划整文件带过去的文件（sign.go、secret.go、
webauth.go、provision.go、import.go）这一份计划不带，所以那几行
`plan does not carry ... verbatim` 是正常的 —— 这一版自己那六个文件不能
在里面。

`refresh_plan_blocks.py` **不要**再跑：它按名字找声明，仓库里同名的地方
不止一处（`TestTheGroupIsMadeAndChanged` 在 webui 和 cmd 里各有一个），它
会拿 cmd 的那份填进 webui 那一段。这一版里对齐了的部分，是照着仓库的文
件逐字放进计划里的。

- [ ] **Step 2: 名单里换掉删掉的文件**

`docs/superpowers/tools/check_plan_files.py` 的 `WHOLE` 里：

- 去掉 `"internal/store/legacy_proxy.go"`、`"internal/manager/proxy_groups.go"`、
  `"internal/manager/proxy_groups_test.go"`（这三个文件这一版删掉了）。
- 加上 `"internal/store/proxy_groups.go"`、`"internal/store/proxy_groups_test.go"`、
  `"internal/egress/groups.go"`、`"internal/egress/groups_test.go"`、
  `"cmd/mmxcode/groups.go"`、`"cmd/mmxcode/groups_test.go"`。

再跑一次 Step 1 那条命令，Expected 同上。

- [ ] **Step 3: 围栏**

```bash
python -I docs/superpowers/tools/check_plan_fences.py docs/superpowers/plans/2026-11-21-mmxcode-proxy-groups.md
```

Expected: `0 problem(s)`。

- [ ] **Step 4: 全部的测试、格式和 vet**

```bash
gofmt -l . 
go vet ./...
go test ./...
```

Expected: `gofmt -l` 没有输出；vet 没有话；测试全过。

- [ ] **Step 5: 端到端看一眼**

拿一份库副本（**不要动真的那份**）跑一遍：

```bash
cp mmxcode.db /tmp/mmxgroups.db
MMXCODE_DB=/tmp/mmxgroups.db ./mmxcode groups list
```

（数据库路径用 `--db /tmp/mmxgroups.db`。）

跑完把副本删掉：`rm /tmp/mmxgroups.db`。

- [ ] **Step 6: 提交**

```bash
git add docs/superpowers/tools/check_plan_files.py docs/superpowers/plans/2026-11-21-mmxcode-proxy-groups.md
git commit -F .git/commit_msg.txt
```

`.git/commit_msg.txt` 写：

```
docs(plan): 出口组 —— 规格和计划

计划里的 Go 块跟 shipped 的代码逐字对齐（check_plan_fences 0 problem(s)，
check_plan_files 没有 stale declaration）。
```
