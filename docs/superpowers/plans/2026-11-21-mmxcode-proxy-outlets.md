# 出口只剩两级：WARP 出项目，出口组退役 —— 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 出口只剩「账号自己的地址」和「全局默认」两级；WARP 出项目；老配置里的
`proxy.groups` 还认得出，但只被读一次 —— 开机的时候，账号身上那个组名变成账号
自己的地址。

**Architecture:** 组这个概念从数据模型里拿掉：`store.Account` 上没有 `ProxyGroup`
了，`SetAccountProxy` 只写地址；配置里那个 `proxy.groups` 键改成「退役的表」，
只被开机那次收拾读一次；出口的找法从三级变成两级；页面上、命令行上、导入与重登
里都没有组了；健康检查查的是在用的那些地址（在用的 = 全局默认 + 账号自己写的）。

**Tech Stack:** Go 1.26、SQLite（`modernc.org/sqlite`，纯 Go）、标准库。

**规格：** `docs/superpowers/specs/2026-11-21-mmxcode-proxy-outlets-design.md`

## 文件一览

| 文件 | 干什么 |
|---|---|
| `internal/config/config.go` | `proxy.groups` 和 `batch.group` 变成退役的键：还认，只是不再有人拿它们做事 |
| `internal/config/config_test.go` | 退役的键还得认（老部署得能起来） |
| `internal/egress/egress.go` | 出口两级；在用的那些地址 |
| `internal/egress/egress_test.go` | 两级的顺序；列在用的地址 |
| `internal/store/store.go` | 账号行上没有组了；写出口只写地址 |
| `internal/store/legacy_proxy.go` | 找出那些还带着组名的账号（只被收拾读一次） |
| `internal/store/store_test.go` | 找得到、写得掉 |
| `internal/manager/proxy_groups.go` | 开机那次收拾：组名变成账号自己的地址 |
| `internal/manager/proxy_groups_test.go` | 收拾的三种情形 |
| `cmd/mmxcode/root.go` | 开机的时候收拾；老配置里的键说一声 |
| `internal/webui/webui.go` | 账号页没有下拉框了；出口页列在用的地址 |
| `internal/webui/templates/accounts.html` | 出口那一格只留地址输入框 |
| `internal/webui/templates/egress.html` | 测试按钮传的是地址 |
| `internal/webui/static/app.js` | 那个按钮传的是地址 |
| `internal/scheduler/scheduler.go` | 健康检查查在用的那些地址 |
| `internal/provision/chain.go` | 导入不带出口了 |
| `cmd/mmxcode/import.go` | 导入走全局默认；重登走账号自己的出口 |
| `cmd/mmxcode/accounts.go` | `accounts proxy` 只剩 `--url` |
| `cmd/mmxcode/relogin.go` | `relogin` 没有 `--group` 了 |
| `cmd/mmxcode/serve.go` | 网页上传的导入走全局默认 |
| `docker-compose.yml`、`.env.example`、`config.example.yaml`、`config.docker.yaml` | WARP 出项目；预置的组没有了 |
| `README.md` | 代理模型改成两级；WARP 那一节删掉 |

---

### Task 1: 配置里那两个退役的键还认着

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`

**为什么先做这个：** 加载器认不出的键是错字，直接报错 —— 老部署的配置文件里
有 `proxy.groups` 和 `batch.group`，认不出它们，那台机器就起不来了，而起不来
的机器上的号就全废了。所以这两个键还得认，只是没有人再拿它们做事。

- [ ] **Step 1: 写失败的测试**

`internal/config/config_test.go`：

```go
// 老部署的配置文件里有两个退役的键。加载器认不出的键是错字，直接报错
// —— 那台机器就起不来了，而起不来的机器上的号就全废了。所以它们还得
// 认，只是没有人再拿它们做事。
func TestTheRetiredKeysAreStillRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := `
proxy:
  default: "http://127.0.0.1:7890"
  groups:
    clash: "http://127.0.0.1:7890"
batch:
  interval: 3s
  group: "clash"
`
	if err := writeFile(path, body); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Proxy.LegacyGroups["clash"] != "http://127.0.0.1:7890" {
		t.Fatalf("groups = %v", c.Proxy.LegacyGroups)
	}
	if c.Batch.LegacyGroup != "clash" {
		t.Fatalf("batch group = %q", c.Batch.LegacyGroup)
	}
}
```

- [ ] **Step 2: 跑测试，看它失败**

Run: `go test ./internal/config/ -run TestTheRetiredKeysAreStillRead`
Expected: FAIL —— `proxy.groups` 认不出来（unknown field）。

- [ ] **Step 3: 让那两个键还认着**

`internal/config/config.go` —— `Proxy` 里多一张退役的表，`Batch` 里多一个退役的
字符串：

```go
// Proxy holds the global outlet.
//
// 出口只有两级：账号自己的地址（写在库里，页面上每一行都能改），然后是
// 全局默认（写在这里）。两个都没有就是直连 —— 直连是最后一级，不是错。
type Proxy struct {
	Default string `yaml:"default" json:"default"`
	// LegacyGroups is the outlet groups of an older version.
	//
	// 退役了：组这个概念没有了。它只被读一次 —— 开机的时候，账号身上那个
	// 组名变成账号自己的地址。
	//
	// 老部署的配置文件里还有这个键，加载器得认它：认不出的键是错字，直
	// 接报错，那台机器就起不来了，而起不来的机器上的号就全废了。
	LegacyGroups map[string]string `yaml:"groups" json:"groups"`
}
```

```go
// Batch is how a batch import runs.
type Batch struct {
	// Interval is the wait between two accounts.
	Interval Duration `yaml:"interval" json:"interval"`
	// LegacyGroup is the outlet group the logins went through, of an
	// older version.
	//
	// 退役了：导入不再有「走哪个组」这件事，它走全局默认出口。这个键只
	// 是还认着 —— 老配置里有它，认不出的键是错字，直接报错，那台机器就
	// 起不来了。
	LegacyGroup string `yaml:"group" json:"group"`
	// Relogin is the automatic signing-in again.
	Relogin Relogin `yaml:"relogin" json:"relogin"`
}
```

- [ ] **Step 4: 跑测试，看它过了**

Run: `go test ./internal/config/`
Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "config: 退役的出口组键还认着，只是没有人再拿它做事"
```

---

### Task 2: 出口只剩两级

**Files:**
- Modify: `internal/egress/egress.go`
- Modify: `internal/egress/egress_test.go`

- [ ] **Step 1: 写失败的测试**

`internal/egress/egress_test.go`：

```go
func TestResolveOrder(t *testing.T) {
	r := New("http://127.0.0.1:7890")

	cases := []struct {
		name string
		acc  store.Account
		want string
	}{
		{"账号级优先", store.Account{ProxyURL: "http://1.2.3.4:8080"}, "http://1.2.3.4:8080"},
		{"再次全局", store.Account{}, "http://127.0.0.1:7890"},
		{"账号级是 direct", store.Account{ProxyURL: "direct"}, "direct"},
		{"账号级是空白", store.Account{ProxyURL: "   "}, "http://127.0.0.1:7890"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := r.Resolve(c.acc); got != c.want {
				t.Fatalf("Resolve = %q, want %q", got, c.want)
			}
		})
	}
}
```

```go
// 在用的出口就那些：全局默认，加上账号自己写的那些。列那些没人走的地址
// 没有意义 —— 查出来的「可用」什么也不说明。
func TestTheOutletsAreTheOnesInUse(t *testing.T) {
	accounts := []store.Account{
		{ProxyURL: "http://10.0.0.5:1080"},
		{ProxyURL: "http://10.0.0.5:1080"},
		{ProxyURL: "socks5h://10.0.0.6:1080"},
		{},
	}
	outs := Outlets(accounts, "http://127.0.0.1:7890")
	want := []Outlet{
		{Name: DefaultOutlet, URL: "http://127.0.0.1:7890"},
		{Name: OwnOutlet, URL: "http://10.0.0.5:1080"},
		{Name: OwnOutlet, URL: "socks5h://10.0.0.6:1080"},
	}
	if len(outs) != len(want) {
		t.Fatalf("outlets = %+v, want %+v", outs, want)
	}
	for i, o := range outs {
		if o != want[i] {
			t.Fatalf("outlet %d = %+v, want %+v", i, o, want[i])
		}
	}
	urls := OutletURLs(accounts, "http://127.0.0.1:7890")
	if len(urls) != 3 || urls[0] != "http://127.0.0.1:7890" {
		t.Fatalf("urls = %v", urls)
	}
}
```

- [ ] **Step 2: 跑测试，看它失败**

Run: `go test ./internal/egress/`
Expected: FAIL —— `New` 还要一张组表，`Resolve` 还在找组。

- [ ] **Step 3: 两级**

`internal/egress/egress.go`：

```go
// Resolver resolves an account's egress and hands out clients for it.
type Resolver struct {
	mu sync.Mutex
	// def is the global way out. It is set when the resolver is made
	// and is not written to afterwards.
	def string
	// clients are the HTTP clients, by way out.
	clients map[string]*http.Client
	// transports are the transports, by way out.
	transports map[string]http.RoundTripper
}
```

```go
// New makes a resolver over the global default.
func New(def string) *Resolver {
	return &Resolver{
		def:        def,
		clients:    map[string]*http.Client{},
		transports: map[string]http.RoundTripper{},
	}
}
```

```go
// Resolve gives the proxy URL an account's requests go through.
//
// Nothing configured means direct: the account has no proxy of its
// own, and there is no global default either.
//
// The lock is taken, so that the maps are read the same way the caches
// are.
func (r *Resolver) Resolve(a store.Account) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if u := strings.TrimSpace(a.ProxyURL); u != "" {
		return u
	}
	v := strings.TrimSpace(r.def)
	if v == "" {
		return Direct
	}
	return v
}
```

在用的那些地址（全局默认 + 账号自己写的，去掉重复的）：

```go
// Outlet is one address that traffic goes out through.
type Outlet struct {
	// Name is what the page calls it.
	Name string
	// URL is the address.
	URL string
}

// The names the outlets page gives the two kinds of outlet.
const (
	// DefaultOutlet is the global default.
	DefaultOutlet = "全局默认"
	// OwnOutlet is an address an account carries itself.
	OwnOutlet = "账号自带"
)
```

```go
// Outlets gives the addresses that are in use.
//
// The global default comes first (if there is one), then the addresses
// the accounts carry themselves — each of them once.
//
// 在用的出口就这些：全局默认一个，加上账号自己写的那些。健康检查查的就
// 是它们，页面列的也是它们 —— 列那些没人走的地址，查出来的「可用」什么
// 也不说明。
func Outlets(accounts []store.Account, def string) []Outlet {
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
	add(DefaultOutlet, def)
	for _, a := range accounts {
		add(OwnOutlet, a.ProxyURL)
	}
	return out
}
```

```go
// OutletURLs gives the addresses of the outlets, in order.
func OutletURLs(accounts []store.Account, def string) []string {
	outs := Outlets(accounts, def)
	urls := make([]string, 0, len(outs))
	for _, o := range outs {
		urls = append(urls, o.URL)
	}
	return urls
}
```

- [ ] **Step 4: 跑测试，看它过了**

Run: `go test ./internal/egress/`
Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/egress/
git commit -m "egress: 出口只有账号自己的地址和全局默认两级"
```

---

### Task 3: 账号行里没有组了

**Files:**
- Modify: `internal/store/store.go`
- Create: `internal/store/legacy_proxy.go`
- Modify: `internal/store/store_test.go`

**为什么列留着：** SQLite 删列要重建整张表，那张表上有触发器、有一百多个号在
用 —— 为了一个已经空了的列去重建它不值。代码不再写它、也不再读它，只有收拾的
时候读一次。

- [ ] **Step 1: 写失败的测试**

`internal/store/store_test.go`：

```go
// 出口组退役了，但那一列还在老账号的行上。收拾之前要先能找到它们。
func TestTheRetiredGroupsAreFound(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now()
	for _, a := range []Account{
		{ID: "acc_01", Name: "有组的", Email: "a@example.com", Status: StatusActive},
		{ID: "acc_02", Name: "没有组的", Email: "b@example.com", Status: StatusActive},
	} {
		if err := s.CreateAccount(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	// 老账号的行上还留着组名。
	if _, err := s.db.ExecContext(ctx,
		`UPDATE accounts SET proxy_group='clash' WHERE id='acc_01'`); err != nil {
		t.Fatal(err)
	}
	list, err := s.LegacyProxyGroups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("list = %+v, want one account", list)
	}
	if list[0].ID != "acc_01" || list[0].Group != "clash" {
		t.Fatalf("list = %+v", list[0])
	}
	// 写出口的时候，那一列顺手就清了。
	if err := s.SetAccountProxy(ctx, "acc_01", "http://127.0.0.1:1080", now); err != nil {
		t.Fatal(err)
	}
	list, err = s.LegacyProxyGroups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("list = %+v, want none", list)
	}
	got, err := s.GetAccount(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if got.ProxyURL != "http://127.0.0.1:1080" {
		t.Fatalf("got %+v", got)
	}
}
```

- [ ] **Step 2: 跑测试，看它失败**

Run: `go test ./internal/store/ -run TestTheRetiredGroupsAreFound`
Expected: FAIL —— `LegacyProxyGroups` 还没有。

- [ ] **Step 3: 账号行上只有地址**

`internal/store/store.go`：

```go
// Account is one MiniMax account.
type Account struct {
	ID        string
	Name      string
	UserID    string
	Email     string
	Status    string
	ProxyURL  string
	LastError string
	CreatedAt time.Time
	UpdatedAt time.Time
}
```

```go
// SetAccountProxy writes an account's outlet.
//
// It is narrow like UpdateAccountStatus: the address is the only thing
// it writes, so a caller that changes the outlet does not have to read
// the account first.
//
// 出口组退役了：那一列还留着（删列要重建整张表，那张表上有触发器、有
// 一百多个号），但它从此永远是空的。写出口的时候顺手把它清掉。
func (s *Store) SetAccountProxy(ctx context.Context, id, url string, updatedAt time.Time) error {
	return s.setAccountProxy(ctx, s.db, id, url, updatedAt)
}
```

```go
func (s *Store) setAccountProxy(ctx context.Context, db execer, id, url string, updatedAt time.Time) error {
	res, err := db.ExecContext(ctx, `
		UPDATE accounts SET proxy_url=?, proxy_group='', updated_at=? WHERE id=?`,
		url, ms(s.stamp(updatedAt)), id)
	if err != nil {
		return fmt.Errorf("set the outlet of the account %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("set the outlet of the account %s: %w", id, ErrNotFound)
	}
	return nil
}
```

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
		lastErr              sql.NullString
		createdAt, updatedAt int64
	)
	dest := []any{&a.ID, &a.Name, &userID, &email, &a.Status, &pu,
		&lastErr, &createdAt, &updatedAt}
	dest = append(dest, extra...)
	if err := sc.Scan(dest...); err != nil {
		return Account{}, err
	}
	a.UserID = userID.String
	a.Email = email.String
	a.ProxyURL = pu.String
	a.LastError = lastErr.String
	a.CreatedAt = fromMS(createdAt)
	a.UpdatedAt = fromMS(updatedAt)
	return a, nil
}
```

- [ ] **Step 4: 找得到那些还带着组名的账号**

`internal/store/legacy_proxy.go`（新文件）：

```go
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
```

- [ ] **Step 5: 跑测试，看它过了**

Run: `go test ./internal/store/`
Expected: PASS。

- [ ] **Step 6: 提交**

```bash
git add internal/store/
git commit -m "store: 账号行上只有地址，组名那一列留着一个空壳"
```

---

### Task 4: 开机那次收拾

**Files:**
- Create: `internal/manager/proxy_groups.go`
- Create: `internal/manager/proxy_groups_test.go`
- Modify: `cmd/mmxcode/root.go`

- [ ] **Step 1: 写失败的测试**

`internal/manager/proxy_groups_test.go`（新文件）：

```go
package manager

import (
	"context"
	"testing"

	"mmxcode/internal/store"
)

// 出口组退役了：开机的时候收拾一次 —— 组名变成账号自己的地址。
func TestTheOutletGroupsBecomeAddresses(t *testing.T) {
	m, path := newTestManagerWithPath(t, nil)
	ctx := context.Background()
	for _, a := range []struct {
		id    string
		group string
		url   string
	}{
		{"acc_01", "clash", ""},
		{"acc_02", "", "http://10.0.0.5:1080"},
		{"acc_03", "clash", "http://10.0.0.9:1080"},
		{"acc_04", "nope", ""},
	} {
		if err := m.Store.CreateAccount(ctx, store.Account{
			ID: a.id, Name: a.id, Email: a.id + "@example.com",
			Status: store.StatusActive, ProxyURL: a.url,
		}); err != nil {
			t.Fatal(err)
		}
		if a.group != "" {
			if _, err := rawDB(t, path).Exec(
				`UPDATE accounts SET proxy_group=? WHERE id=?`, a.group, a.id); err != nil {
				t.Fatal(err)
			}
		}
	}
	res, err := m.MaterializeProxyGroups(ctx, map[string]string{
		"clash": "http://127.0.0.1:7890",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Moved != 1 {
		t.Errorf("moved = %d, want 1", res.Moved)
	}
	if res.Kept != 1 {
		t.Errorf("kept = %d, want 1", res.Kept)
	}
	if res.Dropped != 1 || len(res.DroppedIDs) != 1 || res.DroppedIDs[0] != "acc_04" {
		t.Errorf("dropped = %d %v, want 1 [acc_04]", res.Dropped, res.DroppedIDs)
	}
	// 组里的地址成了账号自己的地址。
	got, err := m.Store.GetAccount(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if got.ProxyURL != "http://127.0.0.1:7890" {
		t.Errorf("acc_01's address = %q", got.ProxyURL)
	}
	// 自己有地址的账号，地址不动。
	got, err = m.Store.GetAccount(ctx, "acc_03")
	if err != nil {
		t.Fatal(err)
	}
	if got.ProxyURL != "http://10.0.0.9:1080" {
		t.Errorf("acc_03's address = %q", got.ProxyURL)
	}
	// 组名对不上的账号：地址空了，它回到全局默认。
	got, err = m.Store.GetAccount(ctx, "acc_04")
	if err != nil {
		t.Fatal(err)
	}
	if got.ProxyURL != "" {
		t.Errorf("acc_04's address = %q, want empty", got.ProxyURL)
	}
	// 收拾过就没有组名了：再收拾一次什么也不做。
	list, err := m.Store.LegacyProxyGroups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("list = %+v, want none", list)
	}
}
```

- [ ] **Step 2: 跑测试，看它失败**

Run: `go test ./internal/manager/ -run TestTheOutletGroupsBecomeAddresses`
Expected: FAIL —— `MaterializeProxyGroups` 还没有。

- [ ] **Step 3: 收拾**

`internal/manager/proxy_groups.go`（新文件）：

```go
package manager

import (
	"context"
	"strings"
	"time"
)

// ProxyGroupsResult says what the move of the outlet groups did.
type ProxyGroupsResult struct {
	// Moved is how many accounts took the address of their group as an
	// address of their own.
	Moved int
	// Kept is how many accounts already had an address of their own:
	// the group never decided anything for them, and their address
	// stays.
	Kept int
	// Dropped is how many accounts lost their group and got no address:
	// the group was not in the configuration.
	Dropped int
	// DroppedIDs are the ids of those accounts.
	DroppedIDs []string
}

// MaterializeProxyGroups turns the outlet group on an account into an
// address of its own.
//
// 出口组退役了：组名还在那些老账号的行上，而组名指向的地址在配置文件
// 里。开机的时候收拾一次 ——
//
//   - 账号自己已经有地址：地址本来就压过组，组名清掉，地址不动。
//   - 账号自己没有地址，配置里有那个组：组名清掉，账号自己的地址写成
//     那个组的地址。
//   - 账号自己没有地址，配置里也没有那个组（配置改过了，组名对不上）：
//     组名清掉，账号回到全局默认。这种账号的出口变了，要说出来。
//
// 它是开机时做一次的事。跑完一次，账号行上就没有组名了，以后开机不用
// 再管。
func (m *Manager) MaterializeProxyGroups(ctx context.Context, groups map[string]string) (ProxyGroupsResult, error) {
	var res ProxyGroupsResult
	list, err := m.Store.LegacyProxyGroups(ctx)
	if err != nil {
		return res, err
	}
	now := time.Now()
	for _, lp := range list {
		url := strings.TrimSpace(lp.URL)
		switch {
		case url != "":
			// 地址本来就压过组：组名清掉，地址不动。
			res.Kept++
		case strings.TrimSpace(groups[lp.Group]) != "":
			url = strings.TrimSpace(groups[lp.Group])
			res.Moved++
		default:
			// 组名在配置里对不上：账号回到全局默认。
			res.Dropped++
			res.DroppedIDs = append(res.DroppedIDs, lp.ID)
		}
		if err := m.Store.SetAccountProxy(ctx, lp.ID, url, now); err != nil {
			return res, err
		}
	}
	return res, nil
}
```

- [ ] **Step 4: 开机的时候叫它一声**

`cmd/mmxcode/root.go` —— 收拾跟擦旧记号挨着，失败只说一声，网关照样起：

```go
		// 出口组退役了：账号身上那个组名变成账号自己的地址。收拾失败的
		// 话，网关照样起 —— 那是一次收拾，不是网关的命。
		res, err := e.mgr.MaterializeProxyGroups(ctx, cfg.Proxy.LegacyGroups)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: the outlet groups of the accounts could not be moved: %v\n", err)
		} else {
			if res.Moved > 0 {
				fmt.Fprintf(os.Stderr, "%d account(s) took the address of their outlet group as an address of their own.\n", res.Moved)
			}
			if res.Dropped > 0 {
				fmt.Fprintf(os.Stderr, "warning: %d account(s) pointed at an outlet group that is not in the configuration (%s): they follow the global default now.\n",
					res.Dropped, strings.Join(res.DroppedIDs, ", "))
			}
		}
		if len(cfg.Proxy.LegacyGroups) > 0 {
			fmt.Fprintf(os.Stderr, "warning: proxy.groups is retired: the outlet groups of the accounts have become addresses of their own. That part can be taken out of the configuration file.\n")
		}
```

- [ ] **Step 5: 跑测试，看它过了**

Run: `go test ./internal/manager/ ./cmd/...`
Expected: PASS。

- [ ] **Step 6: 提交**

```bash
git add internal/manager/ cmd/mmxcode/root.go
git commit -m "manager: 开机的时候把账号身上那个组名变成账号自己的地址"
```

---

### Task 5: 页面上

**Files:**
- Modify: `internal/webui/webui.go`
- Modify: `internal/webui/templates/accounts.html`
- Modify: `internal/webui/templates/egress.html`
- Modify: `internal/webui/static/app.js`

- [ ] **Step 1: 账号页只留地址**

`internal/webui/templates/accounts.html`：

```html
  <td>
    <input size="24" placeholder="单独代理地址（空 = 跟随全局）" data-account="{{$acc.ID}}" data-field="proxy_url" value="{{$acc.ProxyURL}}">
  </td>
```

- [ ] **Step 2: 出口页列在用的那些地址**

`internal/webui/webui.go`：

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
	var out []map[string]any
	for _, o := range egress.Outlets(accounts, w.Cfg.Proxy.Default) {
		h, ok := byURL[o.URL]
		out = append(out, map[string]any{
			"Name": o.Name, "URL": egress.Redacted(o.URL),
			"OK": ok && h.OK, "Checked": ok, "Error": h.Error,
		})
	}
	return map[string]any{"Egress": out}, nil
}
```

```go
// apiEgressTest tests one outlet now.
func (w *WebUI) apiEgressTest(rw http.ResponseWriter, r *http.Request) {
	var body struct {
		URL string `json:"url"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	url, ok := w.outletURL(r.Context(), body.URL)
	if !ok {
		http.Error(rw, "no such outlet", http.StatusNotFound)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	err := w.Mgr.Egress.Check(ctx, url, "https://agent.minimax.io/")
	ok2 := err == nil
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	_ = w.Store.SetEgressHealth(r.Context(), url, ok2, msg)
	writeJSON(rw, map[string]any{"ok": ok2, "error": msg})
}
```

```go
// outletURL finds the address behind the one the page shows.
//
// 页面显示的是去掉凭据的地址（凭据是秘密，不能露在页面上），那个地址发
// 回来的时候也是去了凭据的。它得对上真的那个地址才测得动。
func (w *WebUI) outletURL(ctx context.Context, shown string) (string, bool) {
	accounts, err := w.Store.ListAccounts(ctx)
	if err != nil {
		return "", false
	}
	for _, o := range egress.Outlets(accounts, w.Cfg.Proxy.Default) {
		if egress.Redacted(o.URL) == shown {
			return o.URL, true
		}
	}
	return "", false
}
```

- [ ] **Step 3: 测试按钮传的是地址**

页面显示的是去掉凭据的地址（凭据是秘密），那个地址发回来的时候也是去了凭据
的 —— 服务端拿它去对上真的那个地址再测。

`internal/webui/templates/egress.html`：

```html
  <td><button data-egress="{{.URL}}">测试</button></td>
```

`internal/webui/static/app.js`：

```javascript
  const egress = ev.target.closest('[data-egress]');
  if (egress) {
    // 页面上显示的是去掉凭据的地址，服务器拿它去对上真的那个地址。
    const url = egress.dataset.egress;
    egress.disabled = true;
    egress.textContent = '测试中……';
    try {
      const out = await post('/api/egress/test', { url });
```

- [ ] **Step 4: 改账号的时候不再有组**

`internal/webui/webui.go` —— `apiAccount`：

```go
// apiAccount handles one account: patch, delete, usage, check-in.
func (w *WebUI) apiAccount(rw http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/accounts/")
	parts := strings.Split(rest, "/")
	id := parts[0]
	if id == "" {
		http.NotFound(rw, r)
		return
	}
	if len(parts) == 2 {
		switch parts[1] {
		case "checkin":
			w.apiCheckin(rw, r, id)
			return
		case "usage":
			w.apiAccountUsage(rw, r, id)
			return
		}
		http.NotFound(rw, r)
		return
	}
	switch r.Method {
	case http.MethodPatch:
		var body struct {
			Name     *string `json:"name"`
			ProxyURL *string `json:"proxy_url"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		a, err := w.Store.GetAccount(r.Context(), id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				http.Error(rw, "no such account: "+id, http.StatusNotFound)
				return
			}
			http.Error(rw, err.Error(), http.StatusInternalServerError)
			return
		}
		if body.ProxyURL != nil {
			// The page shows the address without its credentials, so
			// the address that comes back unchanged is the one that
			// is stored, and not the redacted one. An address the
			// user really changed is written as it is.
			shown := egress.Redacted(a.ProxyURL)
			if *body.ProxyURL != shown || shown == a.ProxyURL {
				a.ProxyURL = *body.ProxyURL
			}
		}
		// 名字和出口分开写。整行写回去会把别的写者在这中间改的东西抹掉：
		// 管理器刚写下的登出，会被这一次改名字抹回 active。
		if body.Name != nil && *body.Name != a.Name {
			if err := w.Store.SetAccountName(r.Context(), a.ID, *body.Name, time.Now()); err != nil {
				http.Error(rw, err.Error(), http.StatusInternalServerError)
				return
			}
		}
		if body.ProxyURL != nil {
			if err := w.Store.SetAccountProxy(r.Context(), a.ID, a.ProxyURL, time.Now()); err != nil {
				http.Error(rw, err.Error(), http.StatusInternalServerError)
				return
			}
		}
		writeJSON(rw, map[string]any{"ok": true})
	case http.MethodDelete:
		if err := w.Store.DeleteAccount(r.Context(), id); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				http.Error(rw, "no such account: "+id, http.StatusNotFound)
				return
			}
			http.Error(rw, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(rw, map[string]any{"ok": true})
	default:
		rw.WriteHeader(http.StatusMethodNotAllowed)
	}
}
```

- [ ] **Step 5: 跑测试，看它过了**

Run: `go test ./internal/webui/`
Expected: PASS。

- [ ] **Step 6: 提交**

```bash
git add internal/webui/
git commit -m "webui: 账号页没有出口组了，出口页列在用的那些地址"
```

---

### Task 6: 命令行

**Files:**
- Modify: `cmd/mmxcode/accounts.go`
- Modify: `cmd/mmxcode/relogin.go`
- Modify: `cmd/mmxcode/serve.go`
- Modify: `cmd/mmxcode/import.go`
- Modify: `cmd/mmxcode/import_test.go`

- [ ] **Step 1: `accounts proxy` 只剩 `--url`**

`cmd/mmxcode/accounts.go`：

```go
func newAccountsListCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "列出账号",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			list, err := e.store.ListAccounts(ctx)
			if err != nil {
				return err
			}
			if len(list) == 0 {
				fmt.Println("还没有账号。先跑 mmxcode login。")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "ID\t标签\t邮箱\t状态\t代理")
			for _, a := range list {
				// 代理地址里的账号和密码是秘密，而这份输出会进到终端的
				// 回滚里，也会进到 CI 的日志里。egress.Redacted 就是
				// 为了这个。账号没有自己的地址就写全局默认那个。
				proxy := egress.Redacted(a.ProxyURL)
				if proxy == "" {
					proxy = egress.Redacted(e.cfg.Proxy.Default)
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
					a.ID, a.Name, a.Email, a.Status, proxy)
			}
			return w.Flush()
		},
	}
}
```

```go
func newAccountsProxyCmd(e *env) *cobra.Command {
	var url string
	cmd := &cobra.Command{
		Use:   "proxy <id>",
		Short: "给账号指定单独的代理地址",
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
			if url != "" {
				a.ProxyURL = url
			}
			// 只写出口那一列。整行写回去会把别的写者在这中间改的东西抹掉：
			// 登出刚写下的状态，会被这一次改代理抹回 active。
			return e.store.SetAccountProxy(ctx, a.ID, a.ProxyURL, nowUTC())
		},
	}
	cmd.Flags().StringVar(&url, "url", "", "单独的代理地址")
	return cmd
}
```

- [ ] **Step 2: `relogin` 没有 `--group` 了**

`cmd/mmxcode/relogin.go`：

```go
func newReloginCmd(e *env) *cobra.Command {
	var all bool
	var accountID string
	cmd := &cobra.Command{
		Use:   "relogin",
		Short: "把签出的账号登回去",
		Long: "用存着的密码，把签出（signed_out）的账号重新登一遍。\n" +
			"没有存密码的账号登不回去——那个键没有就是没有。\n" +
			"同一个账号不会同时登两遍，不过这只在一个进程里算数：这个命令跟\n" +
			"一个正在跑着的 mmxcode serve 用同一个库的时候，两个进程可能同时登\n" +
			"同一个账号。要是不想撞上，就别一边开着 serve 一边跑这一条。",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRelogin(cmd.Context(), e, cmd.OutOrStdout(), accountID, all)
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "重登所有签出且有密码的账号")
	cmd.Flags().StringVar(&accountID, "account", "", "只重登这一个账号")
	return cmd
}
```

```go
// runRelogin signs the signed-out accounts in again.
//
// Either one account or all of them: "all of them" without saying so
// is a surprise waiting to happen, and a named account is not a
// surprise at all.
func runRelogin(ctx context.Context, e *env, out io.Writer, accountID string, all bool) error {
	if accountID == "" && !all {
		return fmt.Errorf("要么说 --all，要么给一个 --account")
	}
	if accountID != "" {
		if err := e.mgr.Relogin(ctx, accountID); err != nil {
			return err
		}
		fmt.Fprintf(out, "重登完成：%s\n", accountID)
		return nil
	}
	res, err := e.mgr.ReloginAll(ctx, 0)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "重登完成：成功 %d，失败 %d。\n", res.Fixed, res.Failed)
	for _, r := range res.Reasons {
		fmt.Fprintf(out, "  %s\n", r)
	}
	if res.Failed > 0 {
		return fmt.Errorf("有 %d 个账号没登回去", res.Failed)
	}
	return nil
}
```

- [ ] **Step 3: 导入走全局默认，重登走账号自己的出口**

`cmd/mmxcode/import.go`（整个文件）：

```go
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"mmxcode/internal/account"
	"mmxcode/internal/manager"
	"mmxcode/internal/oauth"
	"mmxcode/internal/provision"
	"mmxcode/internal/secret"
	"mmxcode/internal/site"
	"mmxcode/internal/store"
	"mmxcode/internal/upstream"
	"mmxcode/internal/webauth"
)

// PasswordKeyEnv is the environment variable that holds the key the
// imported passwords are encrypted with.
//
// The key is not in the configuration file: the file and the database
// usually sit in the same volume, and a key next to the cipher text
// is no key at all.
const PasswordKeyEnv = "MMXCODE_PASSWORD_KEY"

// webTimeout is how long one request to the account site may take.
//
// The site answers in seconds: the device code, the sign-in and the
// approval are all quick. A request that has not answered in half a
// minute is not going to.
const webTimeout = 30 * time.Second

func newImportCmd(e *env) *cobra.Command {
	var file string
	var interval time.Duration
	var noStore bool
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "import",
		Short: "按清单批量登录账号",
		Long: "读一份每行「邮箱|密码」的清单，把这些账号自动登录进来。\n" +
			"密码登录之后默认加密存进库里（密钥在 " + PasswordKeyEnv + "），\n" +
			"没有这个环境变量就登录一次就丢。",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if timeout > 0 {
				// 整趟导入有个时限：清单很大的时候，人不愿意等到
				// 天亮才知道它卡住了。
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, timeout)
				defer cancel()
			}
			return runImport(ctx, e, cmd.OutOrStdout(), file, interval, noStore)
		},
	}
	cmd.Flags().StringVar(&file, "file", "", "账号清单（每行 邮箱|密码）")
	cmd.Flags().DurationVar(&interval, "interval", 3*time.Second, "每个账号之间的间隔")
	cmd.Flags().BoolVar(&noStore, "no-store-password", false, "登录成功就丢密码，不入库")
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "整趟导入的时限（0 = 不限）")
	_ = cmd.MarkFlagRequired("file")
	return cmd
}

// runImport brings in the accounts of a list.
func runImport(ctx context.Context, e *env, out io.Writer, file string, interval time.Duration, noStore bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	entries, bad := provision.Parse(bytes.NewReader(raw))
	fmt.Fprintf(out, "读入 %d 个账号。\n", len(entries))
	for _, b := range bad {
		// 坏行的那句话里没有密码：清单的解析器把密码摘掉了，
		// 这一行是要写进数据库的导入留痕里的。
		fmt.Fprintf(out, "第 %d 行：%s（%s）\n", b.Line, b.Reason, b.Text)
	}
	if len(entries) == 0 {
		return fmt.Errorf("清单里没有能用的账号")
	}

	keeper, err := passwordKeeper(noStore)
	if err != nil {
		return err
	}
	runner, err := e.runner(interval, keeper)
	if err != nil {
		return err
	}

	runID := newRunID()
	if e.store != nil {
		if err := e.store.CreateImportRun(ctx, store.ImportRun{
			ID: runID, Source: "cli", StartedAt: time.Now(), Total: len(entries),
		}); err != nil {
			return err
		}
	}

	results, runErr := runner.Run(ctx, entries)
	// 清单里的第几行：出错的账号要能找回去。
	lines := map[string]int{}
	for _, en := range entries {
		lines[strings.ToLower(en.Email)] = en.Line
	}
	ok := 0
	for _, r := range results {
		if r.OK {
			ok++
			fmt.Fprintf(out, "  ✔ %s\t入库成功", r.Email)
			// 账号进来了但密码没存下，也要说：不然下一次掉线时，
			// 这个账号只能等人手工重新入号。
			if r.Message != "" {
				fmt.Fprintf(out, "（%s）", r.Message)
			}
			fmt.Fprintln(out)
			// 刚进来的账号马上读一次账本：不等调度器那一趟（15 分
			// 钟），人才看得到它有多少积分。读不到不算导入失败——
			// 账号是进来了的，账本读不到是另一件事。
			if e.mgr != nil && r.AccountID != "" {
				if _, err := e.mgr.RefreshUsage(ctx, r.AccountID); err != nil {
					fmt.Fprintf(out, "      积分读不到：%v\n", err)
				}
			}
			continue
		}
		fmt.Fprintf(out, "  ✘ %s\t%s：%s", r.Email, r.Stage, r.Message)
		if line, known := lines[strings.ToLower(r.Email)]; known {
			fmt.Fprintf(out, "（第 %d 行", line)
		} else {
			fmt.Fprint(out, "（")
		}
		if r.Code != 0 {
			fmt.Fprintf(out, "错误码 %d", r.Code)
		}
		fmt.Fprintln(out, "）")
	}
	fmt.Fprintf(out, "\n已处理 %d 个，成功 %d，失败 %d。\n", len(results), ok, len(results)-ok)
	switch {
	case keeper != nil:
		fmt.Fprintf(out, "密码已加密入库（%s）。\n", PasswordKeyEnv)
	case noStore:
		fmt.Fprintln(out, "密码用完就丢（不入库）。")
	default:
		// 只有「没设密钥」才要说这件事：--no-store-password 是人自己
		// 说的，说他「没有密钥」是冤枉他。
		fmt.Fprintf(out, "密码没有存进库里（没有 %s）。\n", PasswordKeyEnv)
	}
	if runErr != nil {
		// 说清楚这趟是被掐断的：不是所有账号都失败了，是有人不想让它
		// 再跑下去了。已经进来的那些在上面写着。
		fmt.Fprintf(out, "清单里共 %d 个，还有 %d 个没轮到：这趟导入被中途截断了（%v）。\n",
			len(entries), len(entries)-len(results), runErr)
	}
	if e.store != nil {
		// 收尾的这一笔不能被掐断的上下文掐掉：时限到了的时候，已经
		// 进来的那些账号要留在记录里，不能白跑一场。
		if err := e.store.FinishImportRun(context.WithoutCancel(ctx), runID, time.Now(),
			ok, len(results)-ok, provision.ResultsJSON(results)); err != nil {
			// 收尾没写成也是错，但截断的原因不能跟着它一起没了。
			return errors.Join(runErr, err)
		}
	}
	return runErr
}

// passwordKeeper gives the function that encrypts a password, or nil
// when the password is not to be kept.
//
// The key is not set: no key, no password kept. The key is set but
// wrong: that is a mistake, and the import stops on it -- quietly
// throwing the password away would leave a database of accounts that
// nobody can sign in again.
func passwordKeeper(noStore bool) (func(string) (string, error), error) {
	if noStore {
		return nil, nil
	}
	if strings.TrimSpace(os.Getenv(PasswordKeyEnv)) == "" {
		return nil, nil
	}
	key, err := secret.FromEnv(PasswordKeyEnv)
	if err != nil {
		return nil, err
	}
	return key.Seal, nil
}

// reloginFn makes the function that signs an account in again.
//
// The account is already in the database: only the token is new. The
// way out is the account's own: the address it carries, or the global
// default when it carries none. The sign-in comes from where the
// account usually comes from.
func (e *env) reloginFn() manager.ReloginFunc {
	return func(ctx context.Context, acc store.Account, password string) error {
		chain := e.chain(e.egr.Resolve(acc), nil)
		tok, err := chain.SignIn(ctx, acc.Email, password)
		if err != nil {
			return err
		}
		return e.store.SaveToken(ctx, store.Token{
			AccountID: acc.ID,
			Access:    tok.Access,
			Refresh:   tok.Refresh,
			ExpiresAt: tok.Expires,
			Subject:   tok.Subject,
			UpdatedAt: time.Now(),
		})
	}
}

// runner makes the batch driver.
//
// The way out is settled once, here, and not once per account: an
// address that does not work is a mistake in the configuration, and it
// is said once rather than once per account.
//
// 导入走全局默认出口：新账号身上没有自己的地址，它跟着全局默认走，全
// 局默认改了它也跟着改。
func (e *env) runner(interval time.Duration, keeper func(string) (string, error)) (*provision.Runner, error) {
	if e.makeRunner != nil {
		return e.makeRunner(interval, keeper), nil
	}
	if _, err := e.egress(e.egr.Resolve(store.Account{})); err != nil {
		return nil, err
	}
	return &provision.Runner{
		Importer: &provision.Importer{
			Import: func(ctx context.Context, email, password string) (provision.Result, error) {
				chain := e.chain(e.egr.Resolve(store.Account{}), keeper)
				return chain.Import(ctx, email, password)
			},
		},
		Interval: interval,
	}, nil
}

// egress settles the way out of an address.
//
// The address says which way the traffic goes: "direct" means no proxy.
func (e *env) egress(proxyURL string) (http.RoundTripper, error) {
	return e.egr.Transport(proxyURL)
}

// chain makes the thing that does the real work.
//
// The keeper encrypts the password so that it can be kept; a nil one
// throws the password away after the sign-in.
//
// Every call makes a new chain, and a new chain has a new web client:
// the site keeps the session in a cookie, and two accounts sharing one
// session would be signing in as each other.
func (e *env) chain(proxyURL string, keeper func(string) (string, error)) *provision.Chain {
	tr, trErr := e.egress(proxyURL)
	up := e.up
	hc := &http.Client{Timeout: webTimeout}
	if trErr == nil {
		// The sign-in, the device code and the token all go out the
		// same way: the site sees one account from one address.
		hc.Transport = tr
		up = upstream.New(upstream.Options{Timeout: webTimeout, Transport: tr})
	}
	web := webauth.New(webauth.Options{
		BaseURL: site.AccountHost,
		HTTP:    hc,
		Lang:    site.Lang,
		// 网页那一套的公共 query 里有 timezone_offset：东八区是
		// 28800 秒，正是浏览器给的那个数。
		TZOffset: localTZOffset(),
	})
	c := &provision.Chain{
		Web: web,
		Start: func(ctx context.Context) (*oauth.Device, error) {
			if trErr != nil {
				return nil, trErr
			}
			return oauth.StartDevice(ctx, up, site.AccountURL(site.DeviceCodePath))
		},
		Poll: func(ctx context.Context, d *oauth.Device) (*oauth.Token, error) {
			return d.Poll(ctx, up, site.AccountURL(site.TokenPath))
		},
		Identity: func(ctx context.Context, access, subject string) (account.Identity, error) {
			// 账户信息也从这条路走：换了出口却用旧出口读账户，站点
			// 看到的是两个地方来的同一个号。
			return e.acc.With(up).Identity(ctx, access, subject)
		},
		Keeper: keeper,
		Now:    time.Now,
		NewID:  newBatchAccountID,
	}
	if e.store != nil {
		c.Store = e.store
	}
	return c
}

// localTZOffset gives the timezone offset in seconds east of UTC.
//
// It is the number the browser sends: the site's web query carries
// -60 × getTimezoneOffset(), and JavaScript counts the minutes west
// of UTC, so the sign is the other way round.
func localTZOffset() int {
	_, offset := time.Now().Zone()
	return offset
}

// newRunID makes the name of one import run.
//
// The time alone is not enough: two runs that start in the same
// millisecond would write the same row, and the second one would be
// refused.
func newRunID() string {
	if r := randomHex(4); r != "" {
		return fmt.Sprintf("run_%d_%s", time.Now().UnixMilli(), r)
	}
	return fmt.Sprintf("run_%d", time.Now().UnixMilli())
}

// newBatchAccountID makes an account id for a batch import.
//
// The time alone is not enough: a batch signs in many accounts in a
// row, and two of them can land in the same millisecond.
func newBatchAccountID() string {
	if r := randomHex(4); r != "" {
		return fmt.Sprintf("acc_%d_%s", time.Now().UnixMilli(), r)
	}
	return fmt.Sprintf("acc_%d", time.Now().UnixMilli())
}

// randomHex gives a few random bytes as hex, or an empty string when
// there are none to give.
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return hex.EncodeToString(b)
}
```

`cmd/mmxcode/import_test.go`（整个文件）：

```go
package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mmxcode/internal/config"
	"mmxcode/internal/egress"
	"mmxcode/internal/provision"
	"mmxcode/internal/store"
)

// aFakeEnv is an env whose batch does not touch the network.
func aFakeEnv(t *testing.T) *env {
	t.Helper()
	e := &env{}
	e.makeRunner = func(interval time.Duration, keeper func(string) (string, error)) *provision.Runner {
		return &provision.Runner{
			Importer: &provision.Importer{
				Import: func(ctx context.Context, email, password string) (provision.Result, error) {
					if password != "pw" {
						return provision.Result{Email: email}, &provision.Error{
							Stage: provision.StagePassword, Code: 1200046, Message: "密码不对",
						}
					}
					return provision.Result{Email: email, OK: true, AccountID: "acc_1"}, nil
				},
			},
			Interval: interval,
			Sleep:    func(context.Context, time.Duration) error { return nil },
		}
	}
	return e
}

// TestTheImportReadsTheCreditsOfTheNewAccount says an account that
// just came in has its credits read.
//
// 导入完了要等上一趟扫描（usage.refresh.scan_every，默认 30 秒）才看
// 得到积分，是不让人话的：刚进来的账号马上就读一次账本。
func TestTheImportReadsTheCreditsOfTheNewAccount(t *testing.T) {
	var reads atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/minimax-cloud/api/v1/credit/details":
			reads.Add(1)
			w.Write([]byte(`{"total_count":1,"details":[{"credit_type":1,"granted_amount":"600","consumed_amount":"0","remaining_amount":"600","granted_at_ms":1763000000000,"expire_at_ms":1900000000000}]}`))
		case "/backend/account/token_plan/remains_percent":
			w.Write([]byte(`{"base_resp":{"status_code":0},"model_remains":[]}`))
		default:
			w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()
	e := testEnvWithServer(t, srv)
	seedAccount(t, e, time.Now().Add(time.Hour))
	e.makeRunner = func(interval time.Duration, keeper func(string) (string, error)) *provision.Runner {
		return &provision.Runner{
			Importer: &provision.Importer{
				Import: func(ctx context.Context, email, password string) (provision.Result, error) {
					return provision.Result{Email: email, OK: true, AccountID: "acc_1"}, nil
				},
			},
			Interval: interval,
			Sleep:    func(context.Context, time.Duration) error { return nil },
		}
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.txt")
	if err := os.WriteFile(path, []byte("a@example.com|pw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runImport(context.Background(), e, &out, path, time.Millisecond, false); err != nil {
		t.Fatal(err)
	}
	if reads.Load() == 0 {
		t.Fatalf("the credits of the new account were not read:\n%s", out.String())
	}
}

func TestImportReadsTheFileAndSaysWhatHappened(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.txt")
	if err := os.WriteFile(path, []byte("a@example.com|pw\nb@example.com|pw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runImport(context.Background(), aFakeEnv(t), &out, path, time.Millisecond, false); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.Contains(text, "a@example.com") || !strings.Contains(text, "b@example.com") {
		t.Fatalf("out = %q", text)
	}
	if !strings.Contains(text, "成功 2") {
		t.Fatalf("out = %q", text)
	}
}

func TestImportSaysALineItCouldNotRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.txt")
	// 清单里得有一个能用的号：光有坏行的话，整趟导入是空跑。
	if err := os.WriteFile(path, []byte("not-an-email\nb@example.com|pw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runImport(context.Background(), aFakeEnv(t), &out, path, time.Millisecond, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "第 1 行") {
		t.Fatalf("out = %q", out.String())
	}
}

func TestImportSaysWhyAnAccountDidNotMakeIt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.txt")
	if err := os.WriteFile(path, []byte("a@example.com|wrong\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runImport(context.Background(), aFakeEnv(t), &out, path, time.Millisecond, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "密码") {
		t.Fatalf("out = %q", out.String())
	}
}

func TestImportStopsAtItsTimeout(t *testing.T) {
	// 整趟导入有个时限：清单很大的时候，人不想等到天亮才知道它卡住了。
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.txt")
	if err := os.WriteFile(path, []byte("a@example.com|pw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := aFakeEnv(t)
	e.makeRunner = func(interval time.Duration, keeper func(string) (string, error)) *provision.Runner {
		return &provision.Runner{
			Importer: &provision.Importer{
				Import: func(ctx context.Context, email, password string) (provision.Result, error) {
					// 站点那边没有回答：这一趟就卡在这里。
					<-ctx.Done()
					return provision.Result{Email: email}, ctx.Err()
				},
			},
			Interval: interval,
			Sleep:    func(context.Context, time.Duration) error { return nil },
		}
	}
	cmd := newImportCmd(e)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--file", path, "--timeout", "100ms"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("the import ran past its timeout")
	}
}

func TestAnImportThatIsCutShortKeepsWhatItGot(t *testing.T) {
	// 时限到了的时候，已经进来的账号不能白跑：记录要收尾，页面上这趟
	// 导入不能永远显示成「还在跑」。
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.txt")
	if err := os.WriteFile(path, []byte("a@example.com|pw\nb@example.com|pw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := testEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.makeRunner = func(interval time.Duration, keeper func(string) (string, error)) *provision.Runner {
		return &provision.Runner{
			Importer: &provision.Importer{
				Import: func(ctx context.Context, email, password string) (provision.Result, error) {
					if email == "a@example.com" {
						return provision.Result{Email: email, OK: true, AccountID: "acc_1"}, nil
					}
					// 时限到了：这一趟就掐在这里。
					cancel()
					return provision.Result{Email: email}, ctx.Err()
				},
			},
			Interval: interval,
			Sleep:    func(context.Context, time.Duration) error { return nil },
		}
	}
	var out bytes.Buffer
	if err := runImport(ctx, e, &out, path, time.Millisecond, false); err == nil {
		t.Fatal("the import ran past its deadline")
	}
	// 已经进来的那个账号要说出来，也要说清楚这趟是被掐断的。
	if !strings.Contains(out.String(), "a@example.com") {
		t.Fatalf("out = %q", out.String())
	}
	if !strings.Contains(out.String(), "截断") {
		t.Fatalf("out = %q, want it to say the run was cut short", out.String())
	}
	runs, err := e.store.ListImportRuns(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("got %d runs", len(runs))
	}
	if runs[0].FinishedAt.IsZero() {
		t.Fatal("the run was never finished")
	}
	if runs[0].OK != 1 {
		t.Fatalf("ok = %d, want the one that got in", runs[0].OK)
	}
	if !strings.Contains(runs[0].Results, "a@example.com") {
		t.Fatalf("results = %q", runs[0].Results)
	}
}

// 导入走全局默认出口：新账号身上没有自己的地址，它跟着全局默认走。
func TestTheImportGoesOutOfTheGlobalDefault(t *testing.T) {
	e := &env{
		cfg: config.Default(),
		egr: egress.New("direct"),
	}
	c := e.chain(e.egr.Resolve(store.Account{}), nil)
	if c == nil {
		t.Fatal("the chain is not there")
	}
}
```

- [ ] **Step 4: 网页上传的导入也走全局默认**

`cmd/mmxcode/serve.go`：

```go
// wireImport puts the batch import and the re-login on the page.
//
// 页面上传和命令行导入走的是同一件事：批量入号和重登。
//
// 密钥有问题的时候导入要停下来：悄悄把密码丢掉，会留下一库谁也登不
// 回去的账号。
//
// 没有密钥，密码就不入库。页面上得说清楚：账号掉线以后没有人能自己登
// 回去，人得知道这件事。
//
// 这一段单独拿出来，是为了能测它：那句提醒是接线上的，不是模板里的。
func (e *env) wireImport(ui *webui.WebUI) {
	keeper, keeperErr := passwordKeeper(false)
	ui.PasswordNote = passwordNote(keeper, keeperErr)
	ui.Import = func(ctx context.Context, entries []provision.Entry) ([]provision.Result, error) {
		if keeperErr != nil {
			return nil, keeperErr
		}
		runner, err := e.runner(e.cfg.Batch.Interval.Duration(), keeper)
		if err != nil {
			return nil, err
		}
		return runner.Run(ctx, entries)
	}
	ui.Relogin = func(ctx context.Context) (manager.ReloginResult, error) {
		return e.mgr.ReloginAll(ctx, e.cfg.Batch.Relogin.MaxAccounts)
	}
}
```

- [ ] **Step 5: 跑测试，看它过了**

Run: `go test ./cmd/...`
Expected: PASS。

- [ ] **Step 6: 提交**

```bash
git add cmd/mmxcode/
git commit -m "cli: 命令行上没有出口组了，导入走全局默认，重登走账号自己的出口"
```

---

### Task 7: 导入链上不带出口了

**Files:**
- Modify: `internal/provision/chain.go`
- Modify: `internal/provision/chain_test.go`

- [ ] **Step 1: 链上那个组名没有**

`internal/provision/chain.go`（整个文件）：

```go
package provision

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"mmxcode/internal/account"
	"mmxcode/internal/oauth"
	"mmxcode/internal/site"
	"mmxcode/internal/store"
	"mmxcode/internal/webauth"
)

// Store is where an imported account goes.
//
// 找和建要在一个事务里：不然两个并发的导入会各自看见「还没有」这一
// 行，然后各建一行——同一个站点账号两行都是活的。
type Store interface {
	// Tx runs fn inside one write transaction.
	Tx(ctx context.Context, fn func(store.Tx) error) error
	// SetPassword writes an account's encrypted password.
	SetPassword(ctx context.Context, id, cipher string, updatedAt time.Time) error
	// UpdateAccountStatus writes the account's state and its last
	// error.
	UpdateAccountStatus(ctx context.Context, id, status, lastError string, updatedAt time.Time) error
	// SetAccountName writes the account's name.
	SetAccountName(ctx context.Context, id, name string, updatedAt time.Time) error
	// SetAccountProxy writes the account's outlet.
	SetAccountProxy(ctx context.Context, id, url string, updatedAt time.Time) error
}

// Web is the part of the account site the chain talks to.
//
// It is an interface so that a test can bring an account in without a
// network: the real one is a webauth.Client, and the session it keeps
// belongs to one account.
type Web interface {
	RegisterDevice(ctx context.Context) (string, error)
	Probe(ctx context.Context, email, deviceID, redirect string) error
	Password(ctx context.Context, email, password, deviceID, redirect string) error
	Approve(ctx context.Context, userCode string) error
}

// The real ones fit the interfaces, so that a caller can put them in
// without an adapter.
var (
	_ Web   = (*webauth.Client)(nil)
	_ Store = (*store.Store)(nil)
)

// Chain is the importer that does the real work.
//
// It signs in to the account site with the email and the password,
// uses that session to approve a device code, and puts the account in
// the database.
type Chain struct {
	// Store is where the account goes.
	Store Store
	// Web signs in to the account site. It is one per account: the
	// site keeps the session in a cookie, and two accounts sharing one
	// session would be signing in as each other.
	Web Web
	// Start begins a device authorization.
	Start func(ctx context.Context) (*oauth.Device, error)
	// Poll waits for the sign-in to finish.
	Poll func(ctx context.Context, d *oauth.Device) (*oauth.Token, error)
	// Identity reads who the account is. A nil one means the account's
	// email is the one from the list and its user id is the token's
	// subject.
	Identity func(ctx context.Context, access, subject string) (account.Identity, error)
	// Keeper encrypts the password so that it can be kept. A nil one
	// means the password is not kept: it is used once and thrown away.
	Keeper func(password string) (string, error)
	// Now is the clock.
	Now func() time.Time
	// NewID makes an account id.
	NewID func() string
	// Sleep waits between two tries of the same step. A nil one waits
	// with the context.
	Sleep func(ctx context.Context, d time.Duration) error
}

// stepTries is how many times one step of the chain is tried at most.
//
// 一次网络抖动不该把整条链重跑一遍 —— 重跑一遍意味着重新登一次密码，站
// 点那边看得到（密码错了会算尝试次数，可能锁号）。所以抖的是哪一步，就
// 补哪一步。
const stepTries = 3

// step runs one step of the chain and tries it again when it is worth
// trying again.
//
// 只补失败的那一步。整条链重来是「重登」那个动作的事（mmxcode relogin、
// 调度器），不是导入链内部的兜底：唯一一处整链重来是设备码自己废了的时候
// （见 Error.Restart），那是换一张新码，不是重登。
func step[T any](ctx context.Context, c *Chain, fn func() (T, error)) (T, error) {
	var zero T
	var last error
	for attempt := 0; attempt < stepTries; attempt++ {
		v, err := fn()
		if err == nil {
			return v, nil
		}
		last = err
		var pe *Error
		if !errors.As(err, &pe) || !pe.Retry {
			return zero, err
		}
		if attempt == stepTries-1 {
			// 最后一趟了：再等下去只是让答案来得更晚。
			break
		}
		if pe.Tries > 0 && pe.Tries <= attempt+1 {
			// 有些错值不值得试三次：站点在限流的时候，试一次就够，再去问
			// 只是让它更烦。
			break
		}
		wait := pe.Wait
		if wait <= 0 {
			wait = backoffWait(attempt)
		}
		if werr := c.wait(ctx, wait); werr != nil {
			return zero, werr
		}
	}
	return zero, last
}

// stepErr is step for a step that has nothing to give back.
func stepErr(ctx context.Context, c *Chain, fn func() error) error {
	_, err := step(ctx, c, func() (struct{}, error) {
		return struct{}{}, fn()
	})
	return err
}

// wait waits, and gives up when the run is over.
func (c *Chain) wait(ctx context.Context, d time.Duration) error {
	if c.Sleep != nil {
		return c.Sleep(ctx, d)
	}
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// SignIn signs one account in and gives back the token.
//
// The steps are the ones the design document lists: a device code, a
// sign-in, an approval, and then the token.
//
// An account that is already in the database only needs the token; an
// account that is new also needs a row of its own. Import does both,
// SignIn only the first.
func (c *Chain) SignIn(ctx context.Context, email, password string) (*oauth.Token, error) {
	if c.Web == nil {
		return nil, &Error{Stage: StageProbe, Message: "没有站点可说话"}
	}
	if c.Start == nil {
		return nil, &Error{Stage: StageToken, Message: "没有设备码可等"}
	}
	d, err := step(ctx, c, func() (*oauth.Device, error) {
		dev, err := c.Start(ctx)
		if err != nil {
			return nil, c.startError(ctx, err)
		}
		return dev, nil
	})
	if err != nil {
		return nil, err
	}
	deviceID, err := step(ctx, c, func() (string, error) {
		id, err := c.Web.RegisterDevice(ctx)
		if err != nil {
			return "", c.registerError(ctx, err)
		}
		return id, nil
	})
	if err != nil {
		return nil, err
	}
	// The page the sign-in sends the browser back to is the page that
	// shows the code. The site builds this path itself when it says a
	// login is needed.
	redirect := "/oauth-authorize?client_id=" + url.QueryEscape(site.ClientID) +
		"&user_code=" + url.QueryEscape(d.UserCode)

	if err := stepErr(ctx, c, func() error {
		if err := c.Web.Probe(ctx, email, deviceID, redirect); err != nil {
			return c.webError(ctx, StageProbe, err)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if err := stepErr(ctx, c, func() error {
		if err := c.Web.Password(ctx, email, password, deviceID, redirect); err != nil {
			return c.webError(ctx, StagePassword, err)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if err := stepErr(ctx, c, func() error {
		if err := c.Web.Approve(ctx, d.UserCode); err != nil {
			if errors.Is(err, webauth.ErrNoDefaultGroup) {
				// 有好几个组、没有一个是默认的：重试多少次都一样，而且这
				// 一条得把站点答的原文带着 —— 人得看见站点到底答了什么，
				// 才知道是去站点上设默认组，还是我们没认出来。
				return &Error{Stage: StageApprove, Message: err.Error()}
			}
			return c.webError(ctx, StageApprove, err)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if c.Poll == nil {
		return nil, &Error{Stage: StageToken, Message: "没有地方等这个令牌"}
	}
	tok, err := step(ctx, c, func() (*oauth.Token, error) {
		t, err := c.Poll(ctx, d)
		if err != nil {
			return nil, c.pollError(ctx, err)
		}
		return t, nil
	})
	if err != nil {
		return nil, err
	}
	return tok, nil
}

// Import signs one account in and puts it in the database.
//
// An account that is already in the database is signed in again and its
// row is updated: one site account must not have two live rows.
func (c *Chain) Import(ctx context.Context, email, password string) (Result, error) {
	// 地址前后带的空格是要去掉的：地址是去重的键之一，同一个地址写成
	// 两种样子，库里就会看成两个账号。
	email = strings.TrimSpace(email)
	tok, err := c.SignIn(ctx, email, password)
	if err != nil {
		return Result{Email: email}, err
	}
	if c.Store == nil {
		return Result{Email: email}, &Error{Stage: StageStore, Message: "没有数据库可放"}
	}

	id := account.Identity{}
	var notes []string
	if c.Identity != nil {
		got, err := c.Identity(ctx, tok.Access, tok.Subject)
		if err == nil {
			got.Email = strings.TrimSpace(got.Email)
			id = got
		} else {
			// 身份没问出来，账号还是在的，只是不知道站点那边是不是就认
			// 这一份。这件事得说出来：不然人以为每一条都对，而邮箱要是
			// 也空了，这个号以后就再也登不回来了。
			notes = append(notes, "没能确认账号身份："+err.Error())
		}
	}
	now := c.now()
	// 邮箱从清单里兜底：站点那边一时问不出来身份，账号的邮箱也不能
	// 就这么空着。没有邮箱的账号落线以后是登不回来的——手里有密码
	// 也用不上，因为不知道该拿它去登谁。
	mail := id.Email
	if mail == "" {
		mail = email
	}

	// 这个账号是不是已经在库里了？重跑一次导入是常事：上一批有一部分
	// 失败了，重来一遍是补救的办法。库里已经有这一行的时候再建一行，
	// 同一个站点账号就有两个活着的行——两个都在轮转池里，页面把它列
	// 两遍，而且其中一个的令牌迟早被另一个的重登作废。
	//
	// 找和写在一个事务里：查了再做的事，两个并发的导入一起做就会做两
	// 遍（网页上传两趟，或者两个进程各跑一趟）。令牌跟这一行一起写：
	// 读者不会看见一行没有令牌的账号。
	var existed bool
	var row store.Account
	err = stepErr(ctx, c, func() error {
		// 写库这一步自己也是一步：库忙的时候（locked、busy）补这一趟就
		// 行，不用把登录重来一遍——重来一遍是重登那个动作的事。
		//
		// 这一趟的留言等写成功了才算数：写失败的时候下一趟重新来，留言
		// 也就不能留两份。
		var said []string
		if err := c.Store.Tx(ctx, func(tx store.Tx) error {
			existing, found, err := findIn(ctx, tx, mail, email, tok.Subject, id.RealUserID)
			if err != nil {
				return err
			}
			said = append(said, found...)
			// 这一行该有的地址和用户 id：站点那边报出来的最准，问不出来就
			// 拿手里有的那个顶上。已经在库里那一行原来就有的，比清单上的
			// 地址更靠得住：那个地址是站点自己认的。
			wantMail := id.Email
			if wantMail == "" {
				wantMail = email
				if existing.ID != "" {
					wantMail = existing.Email
				}
			}
			wantUserID := id.RealUserID
			if wantUserID == "" {
				wantUserID = tok.Subject
				if existing.ID != "" && existing.UserID != "" {
					wantUserID = existing.UserID
				}
			}
			if existing.ID != "" {
				existed = true
				row = existing
				// 站点那边报出来的真邮箱和真用户 id 知道了，就把这一行对齐：
				// 邮箱是去重的键之一，键停在旧的地方，下一次导入就会把这个
				// 账号当成新账号，再插一行。
				if wantMail != row.Email {
					// 别处已经有这个地址了就别拿：两个账号共用一个地址，掉线
					// 以后重登就分不清该登谁了——密码是对着其中一个的。
					other, err := tx.AccountByEmail(ctx, wantMail)
					switch {
					case err == nil && other.ID != row.ID:
						said = append(said, "另一个账号（"+other.ID+
							"）也在用这个地址，地址没有换过去："+wantMail)
						wantMail = row.Email
					case err != nil && !errors.Is(err, store.ErrNotFound):
						said = append(said, "没能查这个地址有没有别人在用，地址没有换过去："+
							wantMail+"（"+err.Error()+"）")
						wantMail = row.Email
					}
				}
				if wantMail != row.Email || wantUserID != row.UserID {
					if err := tx.SetAccountIdentity(ctx, row.ID, wantMail, wantUserID, now); err != nil {
						return err
					}
					if wantMail != row.Email {
						// 换了地址要说：那是从清单上写的那个，换成了站点自己
						// 认的那个。掉线以后重登用的是这一栏，地址换了人得
						// 知道。
						said = append(said, "这个账号的邮箱换成了站点那边的："+wantMail)
					}
					row.Email = wantMail
					row.UserID = wantUserID
				}
				return tx.SaveToken(ctx, store.Token{
					AccountID: row.ID,
					Access:    tok.Access,
					Refresh:   tok.Refresh,
					ExpiresAt: tok.Expires,
					Subject:   tok.Subject,
					UpdatedAt: now,
				})
			}
			acc := store.Account{
				ID:        c.newID(),
				Name:      mail,
				UserID:    wantUserID,
				Email:     wantMail,
				Status:    store.StatusActive,
				CreatedAt: now,
				UpdatedAt: now,
			}
			if err := tx.CreateAccount(ctx, acc); err != nil {
				return err
			}
			if err := tx.SaveToken(ctx, store.Token{
				AccountID: acc.ID,
				Access:    tok.Access,
				Refresh:   tok.Refresh,
				ExpiresAt: tok.Expires,
				Subject:   tok.Subject,
				UpdatedAt: now,
			}); err != nil {
				return err
			}
			row = acc
			return nil
		}); err != nil {
			return c.storeError(ctx, err)
		}
		notes = append(notes, said...)
		return nil
	})
	if err != nil {
		msg := err.Error()
		var pe *Error
		if errors.As(err, &pe) {
			msg = pe.Message
		}
		if existed {
			// 原来那一行没动过：它还带着一条能用的链，报错就是，不用
			// 把这一行删掉——删掉就是毁掉一个还能用的账号。
			msg = "令牌没能存下，库里原来那一行没动：" + msg
		}
		return Result{Email: email}, &Error{Stage: StageStore, Message: msg}
	}

	if existed {
		notes = c.refresh(ctx, row, id.Email, now, notes)
		notes = append(notes, "这个账号已经在库里了，更新了原来那一行")
	}
	notes, stored := c.keep(ctx, row.ID, password, now, notes)
	res := Result{Email: email, OK: true, AccountID: row.ID, Message: strings.Join(notes, "；")}
	if !stored {
		res.Stage = StageStore
	}
	return res, nil
}

// refresh puts the new sign-in on the row that is already there.
//
// 状态、名字、出口组这三样在事务外写：它们各写各的列，写不进去不
// 影响别的——令牌已经是新的了，只是状态还没跟上，这种事要说出来，不
// 是导入失败。
//
// 名字只在真的问出身份的时候才改：问不出来的时候手里没有真名字，拿
// 清单上的邮箱把原来的名字盖掉，是把知道的东西换成不知道的。
func (c *Chain) refresh(ctx context.Context, acc store.Account,
	knownName string, now time.Time, notes []string) []string {
	// 它刚刚登进去了：状态回到 active。这笔写不进去不是导入失败——
	// 令牌已经是新的了，只是状态还没跟上，要说出来。
	if err := c.Store.UpdateAccountStatus(ctx, acc.ID, store.StatusActive, "", now); err != nil {
		notes = append(notes, "令牌换新了，但状态没能写成 active："+err.Error())
	}
	if knownName != "" && knownName != acc.Name {
		if err := c.Store.SetAccountName(ctx, acc.ID, knownName, now); err != nil {
			notes = append(notes, "名字没能更新："+err.Error())
		}
	}
	// 出口：导入不动账号自己那个地址 —— 那是人自己设的，不能因为一次导
	// 入就没了。账号没有地址的时候，它跟着全局默认走；全局默认改了，
	// 它也跟着改。
	return notes
}

// findIn gives the account row that belongs to this site account.
//
// 三样都问一遍，先问最靠得住的那一样：
//
//   - subject：令牌里带的那个用户 id。一个站点账号一个，重登多少次都是
//     它。清单上的邮箱可能是别名，站点那边报的邮箱又可能一时问不出来，
//     只有它是一直在的。
//   - 站点那边报的邮箱
//   - 清单上写的邮箱
//   - 站点那边报的那个真的 user id（realUserID）：有的账号站点那边报不
//     出邮箱，令牌里也没有 sub，只有它认得出是哪一个站点账号。
//
// 找到两样却指向两个不一样的行，说明库里本来就有同一个站点账号的两行：
// 那不是这次导入造成的，但要说出来——两行都在轮转池里，页面把它列两遍。
//
// 找不到不是错：那就是一个新账号。
func findIn(ctx context.Context, tx store.Tx, mail, email, subject, userID string) (store.Account, []string, error) {
	var notes []string
	var found store.Account
	have := false
	add := func(acc store.Account) {
		if !have {
			found, have = acc, true
			return
		}
		if acc.ID != found.ID {
			notes = append(notes, "库里已经有同一个站点账号的两行了（"+
				found.ID+" 和 "+acc.ID+"）")
		}
	}
	if subject != "" {
		acc, err := tx.AccountBySubject(ctx, subject)
		switch {
		case err == nil:
			add(acc)
		case errors.Is(err, store.ErrNotFound):
		default:
			return store.Account{}, nil, err
		}
	}
	asked := map[string]bool{}
	for _, key := range []string{mail, email} {
		if key == "" || asked[strings.ToLower(key)] {
			continue
		}
		asked[strings.ToLower(key)] = true
		acc, err := tx.AccountByEmail(ctx, key)
		switch {
		case err == nil:
			add(acc)
		case errors.Is(err, store.ErrNotFound):
		default:
			return store.Account{}, nil, err
		}
	}
	// 站点给的那个真的 user id：地址和令牌都认不出是哪一个站点账号的
	// 时候，它是最后那把钥匙。
	if userID != "" {
		acc, err := tx.AccountByUserID(ctx, userID)
		switch {
		case err == nil:
			add(acc)
		case errors.Is(err, store.ErrNotFound):
		default:
			return store.Account{}, nil, err
		}
	}
	return found, notes, nil
}

// keep encrypts the password and writes it down, when there is a
// keeper.
//
// It says whether the password made it: an account that is in without a
// password cannot sign itself back in, and the person has to know.
func (c *Chain) keep(ctx context.Context, id, password string, now time.Time, notes []string) ([]string, bool) {
	if c.Keeper == nil {
		return notes, true
	}
	cipher, err := c.Keeper(password)
	if err != nil {
		return append(notes, "密码没能加密存下："+err.Error()), false
	}
	if err := c.Store.SetPassword(ctx, id, cipher, now); err != nil {
		return append(notes, "密码没能存下："+err.Error()), false
	}
	return notes, true
}

// startError turns what the device code request said into what the chain
// needs to know.
//
// 设备码开不出来，多半是连接那边的事：再问一次就好（补这一步，不是把整
// 条链重跑一遍）。
func (c *Chain) startError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return &Error{Stage: StageToken, Message: err.Error()}
	}
	return &Error{Stage: StageToken, Message: err.Error(), Retry: transient(err)}
}

// registerError turns what the device registration said into what the
// chain needs to know.
func (c *Chain) registerError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return &Error{Stage: StageProbe, Message: err.Error()}
	}
	return &Error{Stage: StageProbe, Message: err.Error(), Retry: transient(err)}
}

// storeError turns what the library said into what the chain needs to
// know.
//
// 库忙（locked、busy）是它自己的事：等一下再写就是了 —— 补写库那一步就
// 够，不用把登录重来一遍（重来一遍是重登那个动作的事）。
func (c *Chain) storeError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return &Error{Stage: StageStore, Message: err.Error()}
	}
	return &Error{Stage: StageStore, Message: err.Error(), Retry: store.Busy(err)}
}

// transient says whether a failure is worth another go at the same step.
//
// 站点明确拒绝的（密码错、账号要设密码、没绑手机）不值得：再来一次还是
// 这个答案。站点自己出了毛病（5xx、限流）和连接上的问题值得。
func transient(err error) bool {
	var se *webauth.SiteError
	if errors.As(err, &se) {
		return retryableCode(se.Code)
	}
	var oe *oauth.Error
	if errors.As(err, &oe) {
		switch oe.Code {
		case "expired_token", "access_denied", "invalid_grant":
			// 这个设备码废了：再问也是这个答案，而且换一张新码才有意
			// 思（那是整条链重来，见 Error.Restart）。
			return false
		}
		return oe.HTTPStatus == http.StatusTooManyRequests || oe.HTTPStatus >= 500
	}
	// 传输上的问题：连接断了、读超时了。
	return true
}

// webError turns what the site said into what the runner needs to
// know: the stage, the site's own code, and whether trying again could
// work.
//
// A wrong password is not worth trying again. A dropped connection is.
//
// ctx 是这一趟导入的上下文：它没了的时候这一趟就结束了，再试也是白试，
// 那一条不该再说「值得再试一次」。
func (c *Chain) webError(ctx context.Context, stage Stage, err error) error {
	dead := ctx.Err() != nil
	var se *webauth.SiteError
	if errors.As(err, &se) {
		pe := &Error{
			Stage: stage,
			Code:  se.Code,
			// ExplainText 是一句话的说法：有错误码就用错误码，没有
			// 就用站点自己那个词（login_required 那种）。
			Message: se.ExplainText(),
		}
		if retryableCode(se.Code) && !dead {
			pe.Retry = true
		}
		return pe
	}
	return &Error{Stage: stage, Message: err.Error(), Retry: !dead}
}

// pollError turns what the token endpoint said into what the runner
// needs to know.
//
// The refusal and the transport failure are told apart: a device code
// that is used up or refused will be refused again, and a dropped
// connection is worth another go. The same goes for a failure one step
// earlier in the chain, and the two are told apart the same way.
//
// 设计文档 §5.3：网络错误、5xx、超时退避重试 2 次（1s、4s）；429 退避
// 30s 重试 1 次；设备码过期不重试。
//
// ctx 是这一趟导入的上下文：只有它自己没了的时候，超时才算「不用再试
// 了」。客户端自己的超时（http.Client.Timeout）报出来的错也是
// context.DeadlineExceeded，但那时候这个上下文还好好的活着——那是连接
// 那边的事，跟前面几步的连接断了是一回事，值得再试一次。
func (c *Chain) pollError(ctx context.Context, err error) error {
	// 上下文没了：再试也是白试，等的那一下也是白等。
	if ctx.Err() != nil {
		return &Error{Stage: StageToken, Message: err.Error()}
	}
	var oe *oauth.Error
	if errors.As(err, &oe) {
		switch oe.Code {
		case "expired_token", "access_denied", "invalid_grant":
			// 这个设备码废了：再问也是这个答案。整条链重来一次（换一张新
			// 码）—— 这是唯一一处整条链重来的地方。
			return &Error{Stage: StageToken, Message: oe.Error(), Restart: true}
		case "missing_refresh_token":
			// 没有刷新令牌的账号活不过一小时：这是账号的事，不是连接
			// 的事，再来一次也是没有。
			return &Error{Stage: StageToken, Message: oe.Error()}
		}
		if oe.HTTPStatus == http.StatusTooManyRequests {
			// 站点在限流：等久一点再来一次。这样的错试三次没有意义，
			// 去了只是让它更烦。
			return &Error{Stage: StageToken, Message: oe.Error(), Retry: true,
				Wait: rateLimitWait, Tries: 2}
		}
		// 5xx 和别的站点自己的错：再来一次可能就成了。
		return &Error{Stage: StageToken, Message: oe.Error(), Retry: true}
	}
	// 传输上的问题：连接断了、读超时了。
	return &Error{Stage: StageToken, Message: err.Error(), Retry: true}
}

// rateLimitWait is how long to wait when the site says it is limiting
// how much it answers.
//
// 设计文档 §5.3：429、站点风控退避更久（30s）重试 1 次。
const rateLimitWait = 30 * time.Second

// retryableCode says whether the site's own code is worth trying again.
//
// Most of them are not: a password that is wrong will be wrong the
// next time as well. The one that is different is the login that has
// gone stale -- a new sign-in may well work.
func retryableCode(code int) bool {
	switch code {
	case 1200007:
		// UNAUTHENTICATED: 登录状态无效，重试一次。
		return true
	}
	return false
}

// now is the clock.
func (c *Chain) now() time.Time {
	if c.Now == nil {
		return time.Now()
	}
	return c.Now()
}

// newID makes an account id.
//
// The time alone is not enough: two accounts that are brought in in the
// same millisecond get the same id, and the second one is refused by
// the primary key. So the id has a random part too.
func (c *Chain) newID() string {
	if c.NewID != nil {
		return c.NewID()
	}
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		// 没有随机数的时候只能用时间：撞车的机会很小，撞上了也
		// 只是这一个账号白做。
		return fmt.Sprintf("acc_%d", c.now().UnixMilli())
	}
	return "acc_" + hex.EncodeToString(b)
}
```

`internal/provision/chain_test.go`（整个文件）：

```go
package provision

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"mmxcode/internal/account"
	"mmxcode/internal/oauth"
	"mmxcode/internal/store"
	"mmxcode/internal/webauth"
)

// fakeStore keeps what it is given.
//
// 它也照着真库的样子来：事务期间别人进不来（真库是一开始就把写锁拿
// 着），写错了要退回去。
type fakeStore struct {
	// mu is the write lock.
	mu       sync.Mutex
	accounts []string
	tokens   []string
	// subjects are the token subjects that were written.
	subjects []string
	password []string
	// failToken makes the token refuse to be saved.
	failToken bool
	// lockFailures is how many times the library says it is busy before
	// it lets the write through.
	lockFailures int
	// rows are the accounts themselves.
	rows []store.Account
	// names are the names that were written.
	names []string
	// statuses are the states that were written.
	statuses []string
	// groups are the outlets that were written.
	groups []string
	// make is where the rows are made, when the test wants two imports
	// to meet there.
	make *barrier
}

// barrier lets a fixed number of callers meet.
//
// 真库里两个并发的导入是排队的：等不到对方就放行，这里就是这个意思。
type barrier struct {
	mu   sync.Mutex
	n    int
	want int
	ch   chan struct{}
}

func newBarrier(want int) *barrier {
	return &barrier{want: want, ch: make(chan struct{})}
}

// wait blocks until the wanted number of callers have arrived, or until
// the wait is over.
//
// 等不到也要放行：真库里的事务是排队的，不是卡死的。
func (b *barrier) wait() {
	b.mu.Lock()
	b.n++
	if b.n >= b.want {
		select {
		case <-b.ch:
		default:
			close(b.ch)
		}
		b.mu.Unlock()
		return
	}
	b.mu.Unlock()
	select {
	case <-b.ch:
	case <-time.After(200 * time.Millisecond):
	}
}

// snapshot is what the fake holds right now.
type snapshot struct {
	accounts []string
	tokens   []string
	subjects []string
	password []string
	rows     []store.Account
	names    []string
	statuses []string
	groups   []string
}

// snapshot takes what the fake holds, so that it can be put back.
func (s *fakeStore) snapshot() snapshot {
	return snapshot{
		accounts: append([]string(nil), s.accounts...),
		tokens:   append([]string(nil), s.tokens...),
		subjects: append([]string(nil), s.subjects...),
		password: append([]string(nil), s.password...),
		rows:     append([]store.Account(nil), s.rows...),
		names:    append([]string(nil), s.names...),
		statuses: append([]string(nil), s.statuses...),
		groups:   append([]string(nil), s.groups...),
	}
}

// restore puts back what the fake held before.
//
// 事务里写下去的东西，一眼吞掉就要退回去。
func (s *fakeStore) restore(before snapshot) {
	s.accounts = before.accounts
	s.tokens = before.tokens
	s.subjects = before.subjects
	s.password = before.password
	s.rows = before.rows
	s.names = before.names
	s.statuses = before.statuses
	s.groups = before.groups
}

// Tx runs fn inside a transaction.
//
// 真库的事务一开始就把写锁拿着：两个并发的导入是排队进来的，不是同时
// 进来的。写错了就退回去。
func (s *fakeStore) Tx(ctx context.Context, fn func(store.Tx) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// 库忙：真库上两个写碰上了就是这样（SQLITE_BUSY）。这一趟没写进去
	// 任何东西，等一下再来一趟就行。
	//
	// 那句话是照着驱动说的写的：库说忙，是它的结果码（store.Busy 认的
	// 就是那个），不是句子里有没有「locked」两个字。
	if s.lockFailures > 0 {
		s.lockFailures--
		return errors.New("database is locked (5) (SQLITE_BUSY)")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	before := s.snapshot()
	if err := fn(fakeTx{s}); err != nil {
		s.restore(before)
		return err
	}
	return nil
}

// fakeTx is one transaction of the fake store.
type fakeTx struct {
	s *fakeStore
}

func (t fakeTx) AccountByEmail(ctx context.Context, email string) (store.Account, error) {
	return t.s.lookup(email)
}

func (t fakeTx) AccountBySubject(ctx context.Context, subject string) (store.Account, error) {
	return t.s.lookupSubject(subject)
}

func (t fakeTx) AccountByUserID(ctx context.Context, userID string) (store.Account, error) {
	return t.s.lookupUserID(userID)
}

// lookupUserID gives the account with that site user id.
//
// 真的库里是按 accounts.user_id 找的：站点给的那个真的那个 id。一个站
// 点账号一个，重登多少次都是它。
func (s *fakeStore) lookupUserID(userID string) (store.Account, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return store.Account{}, fmt.Errorf("get the account by user id: %w", store.ErrNotFound)
	}
	var found store.Account
	have := false
	for _, r := range s.rows {
		if strings.TrimSpace(r.UserID) != userID {
			continue
		}
		if !have || r.CreatedAt.Before(found.CreatedAt) ||
			(r.CreatedAt.Equal(found.CreatedAt) && r.ID < found.ID) {
			found, have = r, true
		}
	}
	if !have {
		return store.Account{}, fmt.Errorf("get the account by user id %s: %w", userID, store.ErrNotFound)
	}
	return found, nil
}

func (t fakeTx) CreateAccount(ctx context.Context, a store.Account) error {
	t.s.waitMake()
	return t.s.createAccount(a)
}

func (t fakeTx) SaveToken(ctx context.Context, tok store.Token) error {
	return t.s.saveToken(tok)
}

func (t fakeTx) SetAccountIdentity(ctx context.Context, id, email, userID string, updatedAt time.Time) error {
	return t.s.setAccountIdentity(id, email, userID)
}

func (t fakeTx) SetAccountName(ctx context.Context, id, name string, updatedAt time.Time) error {
	return t.s.setAccountName(id, name)
}

func (t fakeTx) SetAccountProxy(ctx context.Context, id, url string, updatedAt time.Time) error {
	return t.s.setAccountProxy(id, url)
}

func (t fakeTx) UpdateAccountStatus(ctx context.Context, id, status, lastError string, updatedAt time.Time) error {
	return t.s.updateAccountStatus(id, status)
}

// waitMake lets two imports meet before they make a row.
//
// 两个并发的导入都要按邮箱找这一行，都找到没有，都要建一行——这就是要
// 防的那件事。让它们在门口碰头，这事就一定会发生，而不是碰运气。
//
// 等的时候不拿着锁：不然另一个导入连看都看不了，就成了排队，不是并发。
func (s *fakeStore) waitMake() {
	if s.make != nil {
		s.make.wait()
	}
}

// createAccount makes a row.
func (s *fakeStore) createAccount(a store.Account) error {
	s.accounts = append(s.accounts, a.ID+":"+a.Email)
	s.rows = append(s.rows, a)
	return nil
}

// saveToken writes a token.
func (s *fakeStore) saveToken(tok store.Token) error {
	if s.failToken {
		return errors.New("the disk is full")
	}
	s.tokens = append(s.tokens, tok.AccountID+":"+tok.Refresh)
	s.subjects = append(s.subjects, tok.AccountID+":"+tok.Subject)
	return nil
}

// setAccountIdentity writes the address and the user id.
func (s *fakeStore) setAccountIdentity(id, email, userID string) error {
	for i := range s.rows {
		if s.rows[i].ID == id {
			s.rows[i].Email = email
			s.rows[i].UserID = userID
		}
	}
	return nil
}

// setAccountName writes a name.
func (s *fakeStore) setAccountName(id, name string) error {
	s.names = append(s.names, id+":"+name)
	for i := range s.rows {
		if s.rows[i].ID == id {
			s.rows[i].Name = name
		}
	}
	return nil
}

// setAccountProxy writes an outlet.
func (s *fakeStore) setAccountProxy(id, url string) error {
	s.groups = append(s.groups, id+":"+url)
	for i := range s.rows {
		if s.rows[i].ID == id {
			s.rows[i].ProxyURL = url
		}
	}
	return nil
}

// updateAccountStatus writes a state.
func (s *fakeStore) updateAccountStatus(id, status string) error {
	s.statuses = append(s.statuses, id+":"+status)
	for i := range s.rows {
		if s.rows[i].ID == id {
			s.rows[i].Status = status
		}
	}
	return nil
}

// lookup gives the account with that email.
//
// 真的库里比的时候两边都折成小写、去空格：清单上写的是哪个样子，库里
// 存的是哪个样子，人看着是同一个就是同一个。
func (s *fakeStore) lookup(email string) (store.Account, error) {
	for _, r := range s.rows {
		if strings.EqualFold(strings.TrimSpace(r.Email), strings.TrimSpace(email)) {
			return r, nil
		}
	}
	return store.Account{}, fmt.Errorf("get the account by email %s: %w", email, store.ErrNotFound)
}

// lookupSubject gives the account whose token carries that subject.
//
// 真的库里是顺着令牌那一行找的：令牌里的那个 sub 是站点账号自己的 id。
// 找到好几行的时候取最早的那一行（先建的先算），跟真的库一个样子。
func (s *fakeStore) lookupSubject(subject string) (store.Account, error) {
	var found store.Account
	have := false
	for _, t := range s.subjects {
		at := strings.LastIndex(t, ":")
		if at < 0 || t[at+1:] != subject {
			continue
		}
		id := t[:at]
		for _, r := range s.rows {
			if r.ID != id {
				continue
			}
			if !have || r.CreatedAt.Before(found.CreatedAt) ||
				(r.CreatedAt.Equal(found.CreatedAt) && r.ID < found.ID) {
				found, have = r, true
			}
		}
	}
	if !have {
		return store.Account{}, fmt.Errorf("get the account by subject %s: %w", subject, store.ErrNotFound)
	}
	return found, nil
}

func (s *fakeStore) CreateAccount(ctx context.Context, a store.Account) error {
	s.waitMake()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.createAccount(a)
}

// AccountByEmail gives the account with that email.
func (s *fakeStore) AccountByEmail(ctx context.Context, email string) (store.Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lookup(email)
}

// SetAccountName writes the account's name.
func (s *fakeStore) SetAccountName(ctx context.Context, id, name string, updatedAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.setAccountName(id, name)
}

// UpdateAccountStatus writes the account's state.
func (s *fakeStore) UpdateAccountStatus(ctx context.Context, id, status, lastError string, updatedAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.updateAccountStatus(id, status)
}

// SetAccountProxy writes the account's outlet.
func (s *fakeStore) SetAccountProxy(ctx context.Context, id, url string, updatedAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.setAccountProxy(id, url)
}

func (s *fakeStore) SaveToken(ctx context.Context, t store.Token) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveToken(t)
}

func (s *fakeStore) SetPassword(ctx context.Context, id, cipher string, updatedAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.password = append(s.password, id+":"+cipher)
	return nil
}

// DeleteAccount takes an account back out.
//
// A context that is already done is refused, the way a real database
// would refuse the delete: the test wants to see that the chain does
// not need the caller's context to be alive.
func (s *fakeStore) DeleteAccount(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, a := range s.accounts {
		if strings.HasPrefix(a, id+":") {
			s.accounts = append(s.accounts[:i], s.accounts[i+1:]...)
			break
		}
	}
	return nil
}

// fakeWeb stands in for the account site.
type fakeWeb struct {
	deviceID   string
	probed     []string
	signed     []string
	approved   []string
	email      string
	password   string
	redirect   string
	approveErr error
	// probeErrs are the errors the probe step gives, one per call.
	probeErrs []error
	// passwordErrs are the errors the password step gives, one per call.
	passwordErrs []error
}

func (w *fakeWeb) RegisterDevice(ctx context.Context) (string, error) {
	return w.deviceID, nil
}

func (w *fakeWeb) Probe(ctx context.Context, email, deviceID, redirect string) error {
	w.probed = append(w.probed, email)
	if len(w.probeErrs) > 0 {
		err := w.probeErrs[0]
		w.probeErrs = w.probeErrs[1:]
		return err
	}
	w.email = email
	w.redirect = redirect
	return nil
}

func (w *fakeWeb) Password(ctx context.Context, email, password, deviceID, redirect string) error {
	w.signed = append(w.signed, email)
	if len(w.passwordErrs) > 0 {
		err := w.passwordErrs[0]
		w.passwordErrs = w.passwordErrs[1:]
		return err
	}
	w.email = email
	w.password = password
	w.redirect = redirect
	return nil
}

func (w *fakeWeb) Approve(ctx context.Context, userCode string) error {
	w.approved = append(w.approved, userCode)
	return w.approveErr
}

// aChain is a chain that does everything but talk to the site.
//
// It takes the Store interface: the fake one and the real library both fit,
// so a test can run the same chain against either.
func aChain(st Store) *Chain {
	return &Chain{
		Store: st,
		Start: func(ctx context.Context) (*oauth.Device, error) {
			return &oauth.Device{UserCode: "ABCD"}, nil
		},
		Poll: func(ctx context.Context, d *oauth.Device) (*oauth.Token, error) {
			return &oauth.Token{Access: "acc", Refresh: "ref", Subject: "sub"}, nil
		},
		Identity: func(ctx context.Context, access, subject string) (account.Identity, error) {
			return account.Identity{Email: "a@example.com", RealUserID: "u1"}, nil
		},
		Web:   &fakeWeb{deviceID: "574786429807779842"},
		Now:   func() time.Time { return time.Unix(1763000000, 0) },
		NewID: func() string { return "acc_1" },
	}
}

func TestTheChainSignsInAndStores(t *testing.T) {
	st := &fakeStore{}
	c := aChain(st)
	c.Keeper = func(password string) (string, error) { return "cipher", nil }

	res, err := c.Import(context.Background(), "a@example.com", "hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.AccountID != "acc_1" {
		t.Fatalf("res = %+v", res)
	}
	if len(st.accounts) != 1 || st.accounts[0] != "acc_1:a@example.com" {
		t.Fatalf("accounts = %v", st.accounts)
	}
	if len(st.tokens) != 1 || st.tokens[0] != "acc_1:ref" {
		t.Fatalf("tokens = %v", st.tokens)
	}
	if len(st.password) != 1 || st.password[0] != "acc_1:cipher" {
		t.Fatalf("passwords = %v", st.password)
	}
}

func TestTheChainRunsTheWebStepsInOrder(t *testing.T) {
	st := &fakeStore{}
	c := aChain(st)
	web := c.Web.(*fakeWeb)

	if _, err := c.Import(context.Background(), "a@example.com", "hunter2"); err != nil {
		t.Fatal(err)
	}
	if len(web.probed) != 1 || web.probed[0] != "a@example.com" {
		t.Fatalf("probed = %v", web.probed)
	}
	if len(web.signed) != 1 || web.signed[0] != "a@example.com" {
		t.Fatalf("signed = %v", web.signed)
	}
	if web.password != "hunter2" {
		t.Fatalf("password = %q", web.password)
	}
	if len(web.approved) != 1 || web.approved[0] != "ABCD" {
		t.Fatalf("approved = %v", web.approved)
	}
	if !strings.Contains(web.redirect, "user_code=ABCD") {
		t.Fatalf("redirect = %q", web.redirect)
	}
}

func TestTheChainDoesNotKeepThePasswordWithoutAKey(t *testing.T) {
	st := &fakeStore{}
	c := aChain(st)

	if _, err := c.Import(context.Background(), "a@example.com", "hunter2"); err != nil {
		t.Fatal(err)
	}
	if len(st.password) != 0 {
		t.Fatalf("the password was kept: %v", st.password)
	}
}

func TestAWrongPasswordSaysSoAndIsNotTriedAgain(t *testing.T) {
	c := &Chain{}
	err := c.webError(context.Background(), StagePassword, &webauth.SiteError{Code: 1200046})

	var pe *Error
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v", err)
	}
	if pe.Code != 1200046 || pe.Retry {
		t.Fatalf("err = %+v", pe)
	}
	if !strings.Contains(pe.Message, "密码") {
		t.Fatalf("message = %q", pe.Message)
	}
}

func TestARefusalWithoutACodeIsStillExplained(t *testing.T) {
	c := &Chain{}
	err := c.webError(context.Background(), StageApprove, &webauth.SiteError{Reason: "login_required"})

	var pe *Error
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v", err)
	}
	if pe.Message == "" || !strings.Contains(pe.Message, "登录") {
		t.Fatalf("message = %q", pe.Message)
	}
}

func TestAMissingDefaultGroupIsNotTriedAgain(t *testing.T) {
	c := &Chain{
		Start: func(ctx context.Context) (*oauth.Device, error) {
			return &oauth.Device{UserCode: "ABCD"}, nil
		},
		Web: &fakeWeb{deviceID: "d1", approveErr: webauth.ErrNoDefaultGroup},
	}
	_, err := c.SignIn(context.Background(), "a@example.com", "hunter2")
	if err == nil {
		t.Fatal("no error")
	}
	var pe *Error
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v", err)
	}
	if pe.Stage != StageApprove || pe.Retry {
		t.Fatalf("err = %+v", pe)
	}
}

func TestAStaleLoginIsTriedAgain(t *testing.T) {
	c := &Chain{}
	err := c.webError(context.Background(), StagePassword, &webauth.SiteError{Code: 1200007})

	var pe *Error
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v", err)
	}
	if !pe.Retry {
		t.Fatalf("err = %+v", pe)
	}
}

func TestALostConnectionIsTriedAgain(t *testing.T) {
	c := &Chain{}
	err := c.webError(context.Background(), StageProbe, errors.New("connection reset"))

	var pe *Error
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v", err)
	}
	if !pe.Retry {
		t.Fatalf("err = %+v", pe)
	}
	// 等多久是标准退避的事（1s、4s），不是这一处说了算。
	if pe.Wait != 0 {
		t.Fatalf("wait = %v, want the standard backoff", pe.Wait)
	}
}

func TestALostConnectionWhilePollingIsTriedAgain(t *testing.T) {
	// 轮询的时候连接断了，跟前面几步的连接断了是一回事：再来一次。
	c := &Chain{}
	err := c.pollError(context.Background(), errors.New("device token: connection reset"))

	var pe *Error
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v", err)
	}
	if !pe.Retry {
		t.Fatalf("err = %+v", pe)
	}
}

func TestARefusedDeviceCodeIsNotTriedAgain(t *testing.T) {
	// 设备码过期了，或者站点不让这个码进来：再来一次也是这个答案。
	for _, code := range []string{"expired_token", "access_denied"} {
		c := &Chain{}
		err := c.pollError(context.Background(), &oauth.Error{Code: code, What: oauth.WhatSignIn})

		var pe *Error
		if !errors.As(err, &pe) {
			t.Fatalf("err = %v", err)
		}
		if pe.Retry {
			t.Fatalf("%s: err = %+v, want it not to be tried again", code, pe)
		}
	}
}

func TestASiteThatIsLimitingIsWaitedFor(t *testing.T) {
	// 429：等久一点再来一次，试一次就够。
	c := &Chain{}
	err := c.pollError(context.Background(), &oauth.Error{Code: "too_many_requests", HTTPStatus: 429, What: oauth.WhatSignIn})

	var pe *Error
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v", err)
	}
	if !pe.Retry || pe.Wait != 30*time.Second || pe.Tries != 2 {
		t.Fatalf("err = %+v", pe)
	}
}

func TestACancelledPollIsNotTriedAgain(t *testing.T) {
	// 上下文没了：再试也是白试。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := &Chain{}
	err := c.pollError(ctx, context.Canceled)

	var pe *Error
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v", err)
	}
	if pe.Retry {
		t.Fatalf("err = %+v", pe)
	}
}

func TestAClientSideTimeoutWhilePollingIsTriedAgain(t *testing.T) {
	// 客户端自己的超时（http.Client.Timeout）报出来的错也是
	// context.DeadlineExceeded，但那是连接那边的事：调用方的上下文还
	// 好好地活着，这种要再试一次，跟前面几步的连接断了是一回事。
	//
	// 真的一次超时长什么样：拿真的客户端去问一个不吭声的服务。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
	}))
	defer srv.Close()
	hc := &http.Client{Timeout: 30 * time.Millisecond}
	_, err := hc.Get(srv.URL)
	if err == nil {
		t.Fatal("the request did not time out")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want it to be a deadline", err)
	}

	c := &Chain{}
	pe := pollErrorOf(t, c.pollError(context.Background(), err))
	if !pe.Retry {
		t.Fatalf("err = %+v, want it to be tried again", pe)
	}
}

func TestAPollTimeoutIsNotTriedAgainWhenTheRunIsOver(t *testing.T) {
	// 上下文没了：再试也是白试。这时候超时就是超时，不再试。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	c := &Chain{}
	pe := pollErrorOf(t, c.pollError(ctx, context.DeadlineExceeded))
	if pe.Retry {
		t.Fatalf("err = %+v, want it not to be tried again", pe)
	}
}

// pollErrorOf says what the error the chain made was.
func pollErrorOf(t *testing.T, err error) *Error {
	t.Helper()
	var pe *Error
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v", err)
	}
	return pe
}

func TestTheChainIsTriedAgainWhenThePollLosesTheConnection(t *testing.T) {
	// 轮询时的一次连接断了：补轮询那一步就行，整条链不用重来。
	st := &fakeStore{}
	c := aChain(st)
	starts := 0
	c.Start = func(ctx context.Context) (*oauth.Device, error) {
		starts++
		return &oauth.Device{UserCode: "ABCD"}, nil
	}
	polls := 0
	c.Poll = func(ctx context.Context, d *oauth.Device) (*oauth.Token, error) {
		polls++
		if polls == 1 {
			return nil, errors.New("device token: connection reset")
		}
		return &oauth.Token{Access: "acc", Refresh: "ref", Subject: "sub"}, nil
	}
	r := &Runner{
		Importer: &Importer{
			Import: func(ctx context.Context, email, password string) (Result, error) {
				return c.Import(ctx, email, password)
			},
		},
		Sleep: func(context.Context, time.Duration) error { return nil },
	}
	out, err := r.Run(context.Background(), []Entry{{Email: "a@example.com", Password: "pw"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || !out[0].OK {
		t.Fatalf("out = %+v", out)
	}
	if polls != 2 {
		t.Fatalf("polled %d times, want 2", polls)
	}
	// 整条链没有重来：重来一遍等于重新登一次密码，站点那边看得到。
	if starts != 1 {
		t.Fatalf("the chain ran %d times, want 1", starts)
	}
}

// TestTheStepThatJittersIsTriedAgainAndOnlyThatStep says a step that
// hiccups is tried again, and only that step.
//
// 网络抖一下不该把整条链重跑一遍：重跑一遍意味着重新登一次密码，站点那
// 边看得到（密码错了会算尝试次数，可能锁号）。
func TestTheStepThatJittersIsTriedAgainAndOnlyThatStep(t *testing.T) {
	st := &fakeStore{}
	c := aChain(st)
	web := c.Web.(*fakeWeb)
	web.passwordErrs = []error{errors.New("connection reset by peer")}
	starts := 0
	c.Start = func(ctx context.Context) (*oauth.Device, error) {
		starts++
		return &oauth.Device{UserCode: "ABCD"}, nil
	}

	res, err := c.Import(context.Background(), "a@example.com", "hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Fatalf("res = %+v", res)
	}
	// 密码那一步试了两次。
	if len(web.signed) != 2 {
		t.Fatalf("signed = %v, want the password step tried twice", web.signed)
	}
	// 前面那几步没有跟着重来：整条链只跑了一遍。
	if starts != 1 {
		t.Fatalf("the chain ran %d times, want 1", starts)
	}
	if len(web.probed) != 1 {
		t.Fatalf("probed = %v, want the probe step not to run again", web.probed)
	}
	if len(web.approved) != 1 {
		t.Fatalf("approved = %v, want the approve step not to run again", web.approved)
	}
}

// TestTheStepIsNotTriedEndlessly says a step is tried at most three
// times.
//
// 试一次加两次：网络抖一下值得再来一次，密码错了不值得，三次足够把这两
// 种分开，又不至于把站点敲烦。
func TestTheStepIsNotTriedEndlessly(t *testing.T) {
	st := &fakeStore{}
	c := aChain(st)
	web := c.Web.(*fakeWeb)
	for i := 0; i < 5; i++ {
		web.passwordErrs = append(web.passwordErrs, errors.New("connection reset"))
	}
	c.Sleep = func(context.Context, time.Duration) error { return nil }

	if _, err := c.Import(context.Background(), "a@example.com", "hunter2"); err == nil {
		t.Fatal("no error")
	}
	if len(web.signed) != 3 {
		t.Fatalf("signed = %v, want the step tried 3 times", web.signed)
	}
}

// TestTheWaitsAreTheOnesTheDesignSays says the step waits a second and
// then four.
//
// 设计文档 §5.3：网络错误、5xx、超时退避重试 2 次（1s、4s）。
func TestTheWaitsAreTheOnesTheDesignSays(t *testing.T) {
	st := &fakeStore{}
	c := aChain(st)
	web := c.Web.(*fakeWeb)
	web.passwordErrs = []error{errors.New("connection reset"), errors.New("connection reset")}
	var waits []time.Duration
	c.Sleep = func(ctx context.Context, d time.Duration) error {
		waits = append(waits, d)
		return nil
	}

	if _, err := c.Import(context.Background(), "a@example.com", "hunter2"); err != nil {
		t.Fatal(err)
	}
	if len(waits) != 2 || waits[0] != time.Second || waits[1] != 4*time.Second {
		t.Fatalf("waits = %v, want 1s then 4s", waits)
	}
}

// TestASiteThatIsLimitingIsWaitedForOnce says a site that is limiting is
// waited for and tried once more.
//
// 429：等久一点（30s）再来一次，试一次就够 —— 再去问只是让它更烦。
func TestASiteThatIsLimitingIsWaitedForOnce(t *testing.T) {
	st := &fakeStore{}
	c := aChain(st)
	polls := 0
	c.Poll = func(ctx context.Context, d *oauth.Device) (*oauth.Token, error) {
		polls++
		return nil, &oauth.Error{Code: "too_many_requests", HTTPStatus: 429, What: oauth.WhatSignIn}
	}
	var waits []time.Duration
	c.Sleep = func(ctx context.Context, d time.Duration) error {
		waits = append(waits, d)
		return nil
	}

	if _, err := c.Import(context.Background(), "a@example.com", "hunter2"); err == nil {
		t.Fatal("no error")
	}
	if polls != 2 {
		t.Fatalf("polled %d times, want 2", polls)
	}
	if len(waits) != 1 || waits[0] != 30*time.Second {
		t.Fatalf("waits = %v, want one wait of 30s", waits)
	}
}

// TestTheChainIsRunAgainWhenTheDeviceCodeIsDead says the whole chain is
// run again when the device code is dead.
//
// 设备码废了（过期、被拒）的时候，换一张新码才有意义 —— 这是唯一一处整
// 条链重来的地方。别的地方都是补那一步：整条链重来是「重登」那个动作的
// 事（mmxcode relogin、调度器），不是导入链内部的兜底。
func TestTheChainIsRunAgainWhenTheDeviceCodeIsDead(t *testing.T) {
	st := &fakeStore{}
	c := aChain(st)
	starts := 0
	c.Start = func(ctx context.Context) (*oauth.Device, error) {
		starts++
		return &oauth.Device{UserCode: "ABCD"}, nil
	}
	polls := 0
	c.Poll = func(ctx context.Context, d *oauth.Device) (*oauth.Token, error) {
		polls++
		if polls == 1 {
			return nil, &oauth.Error{Code: "expired_token", What: oauth.WhatSignIn}
		}
		return &oauth.Token{Access: "acc", Refresh: "ref", Subject: "sub"}, nil
	}
	r := &Runner{
		Importer: &Importer{
			Import: func(ctx context.Context, email, password string) (Result, error) {
				return c.Import(ctx, email, password)
			},
		},
		Sleep: func(context.Context, time.Duration) error { return nil },
	}
	out, err := r.Run(context.Background(), []Entry{{Email: "a@example.com", Password: "pw"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || !out[0].OK {
		t.Fatalf("out = %+v", out)
	}
	if starts != 2 {
		t.Fatalf("the chain ran %d times, want 2", starts)
	}
}

// TestTheChainIsNotRunAgainWhenTheSiteRefuses says a refusal is not run
// again.
//
// 密码错、账号要设密码、没绑手机：再来一次还是这个答案，而且整条链重来等
// 于重新登一次密码 —— 站点那边会记尝试次数。
func TestTheChainIsNotRunAgainWhenTheSiteRefuses(t *testing.T) {
	st := &fakeStore{}
	c := aChain(st)
	web := c.Web.(*fakeWeb)
	web.passwordErrs = []error{&webauth.SiteError{Code: 1200046}}
	starts := 0
	c.Start = func(ctx context.Context) (*oauth.Device, error) {
		starts++
		return &oauth.Device{UserCode: "ABCD"}, nil
	}
	r := &Runner{
		Importer: &Importer{
			Import: func(ctx context.Context, email, password string) (Result, error) {
				return c.Import(ctx, email, password)
			},
		},
		Sleep: func(context.Context, time.Duration) error { return nil },
	}
	out, err := r.Run(context.Background(), []Entry{{Email: "a@example.com", Password: "pw"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].OK {
		t.Fatalf("out = %+v", out)
	}
	if starts != 1 {
		t.Fatalf("the chain ran %d times, want 1", starts)
	}
	if len(web.signed) != 1 {
		t.Fatalf("signed = %v, want the password step tried once", web.signed)
	}
}

// TestTheWritingIsTriedAgainWhenTheLibraryIsBusy says the writing is
// tried again when the library is busy.
//
// 真跑出来过：令牌换新了，库说「database is locked」，写不进去。那是写库
// 那一步的事 —— 补写库那一步就行，不用把登录重来一遍。
func TestTheWritingIsTriedAgainWhenTheLibraryIsBusy(t *testing.T) {
	st := &fakeStore{lockFailures: 1}
	c := aChain(st)
	web := c.Web.(*fakeWeb)

	res, err := c.Import(context.Background(), "a@example.com", "hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Fatalf("res = %+v", res)
	}
	if len(st.accounts) != 1 {
		t.Fatalf("accounts = %v", st.accounts)
	}
	if len(st.tokens) != 1 {
		t.Fatalf("tokens = %v", st.tokens)
	}
	// 登录没有重来：重来一遍是重登那个动作的事。
	if len(web.signed) != 1 {
		t.Fatalf("signed = %v, want the sign-in done once", web.signed)
	}
}

func TestAnAccountWithoutATokenIsTakenBackOut(t *testing.T) {
	st := &fakeStore{failToken: true}
	c := aChain(st)

	_, err := c.Import(context.Background(), "a@example.com", "hunter2")
	if err == nil {
		t.Fatal("no error")
	}
	var pe *Error
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v", err)
	}
	if pe.Stage != StageStore {
		t.Fatalf("err = %+v", pe)
	}
	if len(st.accounts) != 0 {
		t.Fatalf("the broken account was left behind: %v", st.accounts)
	}
}

func TestAFailedIdentityLookupKeepsTheEmailOfTheList(t *testing.T) {
	// 站点那边问不出身份的时候，邮箱得从清单里兜底：邮箱空着的账号
	// 以后落线了是登不回来的——有密码也不知道该登谁。
	st := &fakeStore{}
	c := aChain(st)
	c.Identity = func(ctx context.Context, access, subject string) (account.Identity, error) {
		return account.Identity{}, errors.New("the account API answered 503")
	}

	res, err := c.Import(context.Background(), "a@example.com", "hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Fatalf("res = %+v", res)
	}
	if len(st.accounts) != 1 || st.accounts[0] != "acc_1:a@example.com" {
		t.Fatalf("accounts = %v", st.accounts)
	}
	if !strings.Contains(res.Message, "身份") {
		t.Fatalf("message = %q, want it to say the identity was not confirmed", res.Message)
	}
	if strings.Contains(res.Message, "hunter2") {
		t.Fatalf("message = %q, want no password in it", res.Message)
	}
}

func TestTheSameAccountTwiceIsOneRow(t *testing.T) {
	// 重跑一次导入是常事：同一个站点账号只能有一行活着。
	st := &fakeStore{}
	c := aChain(st)

	first, err := c.Import(context.Background(), "a@example.com", "hunter2")
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.Import(context.Background(), "a@example.com", "hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if len(st.accounts) != 1 {
		t.Fatalf("accounts = %v, want one row", st.accounts)
	}
	if second.AccountID != first.AccountID {
		t.Fatalf("account id = %q, want %q", second.AccountID, first.AccountID)
	}
	if !second.OK {
		t.Fatalf("res = %+v", second)
	}
	if !strings.Contains(second.Message, "已经在库里") {
		t.Fatalf("message = %q, want it to say it was an update", second.Message)
	}
	// 令牌是换新的：这一行现在用的是刚登进去的那条链。
	if len(st.tokens) != 2 {
		t.Fatalf("tokens = %v, want the token written twice", st.tokens)
	}
	if len(st.statuses) != 1 || st.statuses[0] != "acc_1:"+store.StatusActive {
		t.Fatalf("statuses = %v", st.statuses)
	}
}

func TestAFailedUpdateKeepsTheRowThatIsThere(t *testing.T) {
	// 令牌没写下去的时候，库里原来那一行不能动：它还带着一条能用的
	// 链，把它删掉就是毁掉一个还能用的账号。
	//
	// 而且动的得是原来那一行，不是新来的一行：新来的一行号不一样，
	// 原来那条链也没有被换掉。这两样都要看得见——「还剩一行」是看不
	// 出来的（新来的一行建了又删，看着也像没动过）。
	st := &fakeStore{}
	c := aChain(st)
	// 两次导入给两个不同的号：新来的一行一眼就看得出来。
	ids := 0
	c.NewID = func() string {
		ids++
		return fmt.Sprintf("acc_%d", ids)
	}
	// 两次登进去的令牌不一样：换了令牌一眼就看得出来。
	polls := 0
	c.Poll = func(ctx context.Context, d *oauth.Device) (*oauth.Token, error) {
		polls++
		return &oauth.Token{Access: "acc", Refresh: fmt.Sprintf("ref%d", polls), Subject: "sub"}, nil
	}

	res, err := c.Import(context.Background(), "a@example.com", "hunter2")
	if err != nil {
		t.Fatal(err)
	}
	first := res.AccountID
	firstToken := st.tokens[0]

	st.failToken = true
	res, err = c.Import(context.Background(), "a@example.com", "hunter2")
	if err == nil {
		t.Fatal("no error for a token that could not be saved")
	}
	if res.OK {
		t.Fatalf("res = %+v", res)
	}
	if len(st.accounts) != 1 || st.accounts[0] != first+":a@example.com" {
		t.Fatalf("accounts = %v, want the original row %q", st.accounts, first)
	}
	if len(st.rows) != 1 || st.rows[0].ID != first {
		t.Fatalf("rows = %v, want the original row %q", st.rows, first)
	}
	// 令牌没有换：写失败的时候不能换。
	if len(st.tokens) != 1 || st.tokens[0] != firstToken {
		t.Fatalf("tokens = %v, want the token that is there", st.tokens)
	}
	// 第二次导入没有另起一行：它用的就是原来那一行。
	if ids != 1 {
		t.Fatalf("the import made %d ids, want 1", ids)
	}
}

// TestTwoConcurrentImportsOfOneEmailAreOneRow says two imports of one
// email do not make two rows.
//
// 两个并发的导入（网页上传两趟，或者两个进程各跑一趟）：都要按邮箱找
// 这一行，都找到没有，都建一行——同一个站点账号两行都是活的，页面把
// 它列两遍，其中一个的令牌迟早被另一个的重登作废。
//
// 找和建要在一个事务里：事务一开始就把写锁拿着，第二个进来的那个看见
// 的是第一个建好的那一行。
func TestTwoConcurrentImportsOfOneEmailAreOneRow(t *testing.T) {
	st := &fakeStore{make: newBarrier(2)}
	c := aChain(st)
	// 两次导入给两个不同的号：两行就看得出来是两行。
	var mu sync.Mutex
	n := 0
	c.NewID = func() string {
		mu.Lock()
		defer mu.Unlock()
		n++
		return fmt.Sprintf("acc_%d", n)
	}

	var wg sync.WaitGroup
	results := make([]Result, 2)
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = c.Import(context.Background(), "a@example.com", "hunter2")
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("import %d: %v", i, err)
		}
	}
	if len(st.accounts) != 1 {
		t.Fatalf("accounts = %v, want one row", st.accounts)
	}
	if results[0].AccountID != results[1].AccountID {
		t.Fatalf("account ids = %q and %q, want the same row",
			results[0].AccountID, results[1].AccountID)
	}
}

// 导入不给新账号写地址：它跟着全局默认走，全局默认改了它也跟着改。
func TestTheImportedAccountHasNoAddressOfItsOwn(t *testing.T) {
	st := &fakeStore{}
	c := aChain(st)

	if _, err := c.Import(context.Background(), "a@example.com", "hunter2"); err != nil {
		t.Fatal(err)
	}
	if len(st.rows) != 1 {
		t.Fatalf("rows = %v", st.rows)
	}
	if st.rows[0].ProxyURL != "" {
		t.Fatalf("address = %q, want none", st.rows[0].ProxyURL)
	}
	// 而且出口那一行根本没动过。
	if len(st.groups) != 0 {
		t.Fatalf("the outlet was written %d times, want none", len(st.groups))
	}
}

// 导入不动账号自己那个地址 —— 那是人自己设的。
//
// 组退役之前，导入带的组说了算，账号自己那个地址会被清掉。现在导入根本
// 不带出口：地址是人自己设的，一次导入不能把它抹掉。
func TestAnImportKeepsTheAddressThatIsThere(t *testing.T) {
	st := &fakeStore{}
	c := aChain(st)

	res, err := c.Import(context.Background(), "a@example.com", "hunter2")
	if err != nil {
		t.Fatal(err)
	}
	// 人给这个账号手设了一个地址。
	if err := st.SetAccountProxy(context.Background(), res.AccountID,
		"socks5h://127.0.0.1:1080", time.Now()); err != nil {
		t.Fatal(err)
	}
	if st.rows[0].ProxyURL == "" {
		t.Fatalf("the address was not set: %+v", st.rows[0])
	}
	// 再导一次。
	if _, err := c.Import(context.Background(), "a@example.com", "hunter2"); err != nil {
		t.Fatal(err)
	}
	if st.rows[0].ProxyURL != "socks5h://127.0.0.1:1080" {
		t.Fatalf("address = %q, want it to stay", st.rows[0].ProxyURL)
	}
}

func TestTheBrokenAccountIsTakenBackOutWhenTheRunWasCutShort(t *testing.T) {
	// 导入被掐断的时候，令牌写不进去：那个半成的行要拿走。删它用的
	// 上下文不能是调用方那个——那时候它已经没了。
	st := &fakeStore{failToken: true}
	c := aChain(st)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := c.Import(ctx, "a@example.com", "hunter2"); err == nil {
		t.Fatal("no error")
	}
	if len(st.accounts) != 0 {
		t.Fatalf("the broken account was left behind: %v", st.accounts)
	}
}

// TestAnAccountIsFoundByItsSubjectEvenWhenTheAddressIsDifferent says
// the token's subject is the key, not the address.
//
// 清单上写的地址可能是别名，站点那边报的地址又可能一时问不出来（站点
// 503 了）。靠地址去重就会漏：同一个站点账号在库里会有两行都是活的，
// 两个都在轮转池里，其中一个的令牌迟早被另一个的重登作废。
//
// 令牌里带的那个 sub 是站点账号自己的 id：一个站点账号一个，重登多少
// 次都是它。
func TestAnAccountIsFoundByItsSubjectEvenWhenTheAddressIsDifferent(t *testing.T) {
	st := &fakeStore{}
	c := aChain(st)

	if _, err := c.Import(context.Background(), "a@example.com", "hunter2"); err != nil {
		t.Fatal(err)
	}
	if len(st.rows) != 1 {
		t.Fatalf("rows = %v", st.rows)
	}

	// 第二趟：站点那边问不出身份了（503），清单上的地址也换了。
	c.Identity = func(ctx context.Context, access, subject string) (account.Identity, error) {
		return account.Identity{}, errors.New("the site is busy")
	}
	res, err := c.Import(context.Background(), "other@example.com", "hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if len(st.rows) != 1 {
		t.Fatalf("rows = %v, want one row for one site account", st.rows)
	}
	if res.AccountID != "acc_1" {
		t.Fatalf("account id = %q, want the row that was already there", res.AccountID)
	}
}

// TestTheAddressIsTrimmed says the address is written without the
// spaces around it.
//
// 地址是去重的键之一：同一个地址写成两种样子（一边带空格一边不带），
// 库里就会看成两个账号。
func TestTheAddressIsTrimmed(t *testing.T) {
	st := &fakeStore{}
	c := aChain(st)
	// 站点那边报的地址两头带着空格。
	c.Identity = func(ctx context.Context, access, subject string) (account.Identity, error) {
		return account.Identity{Email: "  real@example.com  "}, nil
	}
	if _, err := c.Import(context.Background(), "alias@example.com", "hunter2"); err != nil {
		t.Fatal(err)
	}
	if st.rows[0].Email != "real@example.com" {
		t.Fatalf("email = %q, want it trimmed", st.rows[0].Email)
	}

	// 清单上的地址两头带着空格，站点那边又问不出身份。
	st2 := &fakeStore{}
	c2 := aChain(st2)
	c2.Identity = nil
	if _, err := c2.Import(context.Background(), "  alias@example.com  ", "hunter2"); err != nil {
		t.Fatal(err)
	}
	if st2.rows[0].Email != "alias@example.com" {
		t.Fatalf("email = %q, want it trimmed", st2.rows[0].Email)
	}
}

// TestTheAddressIsNotTakenOverFromAnotherAccount says an address that
// another account already has is not written onto this one.
//
// 两个账号共用一个地址不是好事：掉线以后重登，分不清该拿哪个密码去登
// ——密码是对着其中一个的，登错了就是把这个账号也登坏了。
//
// 这一行是按令牌里那个 id 找到的（那是站点账号自己的 id），站点那边报
// 出来的地址却是另一个账号的：这时候地址不能换过去。
func TestTheAddressIsNotTakenOverFromAnotherAccount(t *testing.T) {
	st := &fakeStore{
		rows: []store.Account{
			{ID: "acc_a", Name: "a", Email: "taken@example.com", Status: store.StatusActive},
			{ID: "acc_b", Name: "b", Email: "b@example.com", Status: store.StatusActive},
		},
		subjects: []string{"acc_b:sub-b"},
	}
	c := aChain(st)
	c.NewID = func() string { return "acc_new" }
	c.Poll = func(ctx context.Context, d *oauth.Device) (*oauth.Token, error) {
		return &oauth.Token{Access: "acc", Refresh: "ref", Subject: "sub-b"}, nil
	}
	c.Identity = func(ctx context.Context, access, subject string) (account.Identity, error) {
		return account.Identity{Email: "taken@example.com", RealUserID: "u9"}, nil
	}

	res, err := c.Import(context.Background(), "b@example.com", "hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if res.AccountID != "acc_b" {
		t.Fatalf("account id = %q, want acc_b", res.AccountID)
	}
	if st.rows[1].Email != "b@example.com" {
		t.Fatalf("email = %q, want the address the account already had", st.rows[1].Email)
	}
	if !strings.Contains(res.Message, "acc_a") {
		t.Fatalf("message = %q, want it to name the account that has the address", res.Message)
	}
}

// TestTheRealLibraryHasOneRowForOneSiteAccount says the real library
// keeps one row for one site account, even when the address on the list
// is not the one the site knows.
//
// 假的库和真的库不是一回事：真的库按令牌里那个 sub 顺着令牌那一行找，假
// 的那套是自己的一套规矩。这一条拿真的库走一遍：清单上的地址换了，站点
// 那边一时又问不出身份，库里还是只有一行。
func TestTheRealLibraryHasOneRowForOneSiteAccount(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	c := aChain(s)
	// 第一趟：站点那边报了地址，那一行上的地址是站点认的那个。
	if _, err := c.Import(ctx, "alias@example.com", "hunter2"); err != nil {
		t.Fatal(err)
	}
	// 第二趟：清单上的地址换了，站点那边问不出身份了。
	c.Identity = nil
	if _, err := c.Import(ctx, "other@example.com", "hunter2"); err != nil {
		t.Fatal(err)
	}

	rows, err := s.ListAccounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("accounts = %v, want one row for one site account", rows)
	}
	if rows[0].Email != "a@example.com" {
		t.Fatalf("email = %q, want the one the site says", rows[0].Email)
	}
}

// TestTheRowIsBroughtInLineWithWhatTheSiteSays says the address and
// the user id on the row are the ones the site says.
//
// 去重的时候邮箱是键之一：键停在旧的地方，下一次导入就会把这个账号
// 当成新账号，再插一行。站点那边报出来的真地址知道了，就得写上去。
func TestTheRowIsBroughtInLineWithWhatTheSiteSays(t *testing.T) {
	st := &fakeStore{}
	c := aChain(st)
	// 第一趟：身份没问，邮箱只能拿清单上的。
	c.Identity = nil
	if _, err := c.Import(context.Background(), "alias@example.com", "hunter2"); err != nil {
		t.Fatal(err)
	}
	if st.rows[0].Email != "alias@example.com" {
		t.Fatalf("email = %q, want the one from the list", st.rows[0].Email)
	}

	// 第二趟：站点那边说了它是谁。
	c.Identity = func(ctx context.Context, access, subject string) (account.Identity, error) {
		return account.Identity{Email: "real@example.com", RealUserID: "u9"}, nil
	}
	res, err := c.Import(context.Background(), "alias@example.com", "hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if len(st.rows) != 1 {
		t.Fatalf("rows = %v, want one row", st.rows)
	}
	if st.rows[0].Email != "real@example.com" {
		t.Fatalf("email = %q, want the one the site says", st.rows[0].Email)
	}
	if st.rows[0].UserID != "u9" {
		t.Fatalf("user id = %q, want the one the site says", st.rows[0].UserID)
	}
	// 换了地址要说：掉线以后重登用的是这一栏。
	if !strings.Contains(res.Message, "real@example.com") {
		t.Fatalf("message = %q, want it to say the address changed", res.Message)
	}
}

// TestTheAddressOnTheRowIsNotThrownAwayWhenTheSiteSaysNothing says a
// row that is already there keeps its address when the site does not
// say one.
//
// 站点那边一时问不出身份，手里的地址就只剩原来那一行的：那是站点自己
// 认过的，比清单上的更靠得住。
//
// 这一条要能分清两件事：地址留在原来那一行，和地址换成清单上那个。这两
// 件事在清单上的地址跟原来那一行一样的时候是一样的事——所以这里清单上
// 的地址跟那一行的不一样，换成清单上那个是看得出来的。
func TestTheAddressOnTheRowIsNotThrownAwayWhenTheSiteSaysNothing(t *testing.T) {
	st := &fakeStore{}
	c := aChain(st)
	// 第一趟：站点那边说了它是谁，那一行上的地址是站点认的那个。
	if _, err := c.Import(context.Background(), "alias@example.com", "hunter2"); err != nil {
		t.Fatal(err)
	}
	if st.rows[0].Email != "a@example.com" {
		t.Fatalf("email = %q, want the one the site says", st.rows[0].Email)
	}

	// 第二趟：站点那边问不出身份了，清单上的地址是另一个。
	c.Identity = nil
	if _, err := c.Import(context.Background(), "alias@example.com", "hunter2"); err != nil {
		t.Fatal(err)
	}
	if st.rows[0].Email != "a@example.com" {
		t.Fatalf("email = %q, want the one that was already there", st.rows[0].Email)
	}
}

// TestTwoRowsForOneSiteAccountAreSaidOut says the import says so when
// the library already has two rows for one site account.
//
// 那不是这次导入造成的，但两行都在轮转池里，页面把它列两遍，重登一次
// 会把另一个的令牌作废。人得知道这件事。
func TestTwoRowsForOneSiteAccountAreSaidOut(t *testing.T) {
	st := &fakeStore{}
	// 库里已经有同一个站点账号的两行（老数据）。
	st.rows = append(st.rows,
		store.Account{ID: "acc_1", Email: "a@example.com", UserID: "u1", Status: store.StatusActive},
		store.Account{ID: "acc_2", Email: "b@example.com", UserID: "u2", Status: store.StatusActive})
	st.subjects = append(st.subjects, "acc_1:sub", "acc_2:sub")
	c := aChain(st)

	res, err := c.Import(context.Background(), "b@example.com", "hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Message, "两行") {
		t.Fatalf("message = %q, want it to say the library has two rows", res.Message)
	}
}

// TestAFailureIsNotWorthTryingAgainWhenTheRunIsOver says a finished
// run does not get a failure that says "try again".
//
// 这一趟已经结束了：再试也是白试，等的那一下也是白等。说了「值得再试」
// 只是让结果表上的那一行写着它做不到的事。
func TestAFailureIsNotWorthTryingAgainWhenTheRunIsOver(t *testing.T) {
	c := &Chain{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := c.webError(ctx, StageProbe, errors.New("connection reset"))
	var pe *Error
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v", err)
	}
	if pe.Retry {
		t.Fatalf("err = %+v, want it not to be tried again", pe)
	}
	if pe.Message != "connection reset" {
		t.Fatalf("message = %q, want what went wrong", pe.Message)
	}
}

// TestAnAccountIsFoundByItsUserIDEvenWhenTheAddressIsDifferent says the
// site user id finds the account.
//
// 有的账号站点那边报不出邮箱：清单上写的地址是别名，令牌里也没有 sub。
// 只有站点给的那个真的 user id 认得出是哪一个站点账号——少了这一步，再
// 导一次就多一行活的，同一个站点账号两行都在轮转池里。
func TestAnAccountIsFoundByItsUserIDEvenWhenTheAddressIsDifferent(t *testing.T) {
	st := &fakeStore{}
	c := aChain(st)
	// 站点那边报不出邮箱，也没有别的线索，只有那个 user id。
	c.Identity = func(ctx context.Context, access, subject string) (account.Identity, error) {
		return account.Identity{RealUserID: "u1"}, nil
	}
	c.Poll = func(ctx context.Context, d *oauth.Device) (*oauth.Token, error) {
		return &oauth.Token{Access: "acc", Refresh: "ref"}, nil
	}
	if _, err := c.Import(context.Background(), "a@example.com", "hunter2"); err != nil {
		t.Fatal(err)
	}
	if len(st.rows) != 1 {
		t.Fatalf("rows = %v", st.rows)
	}

	// 第二趟：清单上的地址换了，站点那边报的还是同一个 user id。
	res, err := c.Import(context.Background(), "other@example.com", "hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if len(st.rows) != 1 {
		t.Fatalf("rows = %v, want one row for one site account", st.rows)
	}
	if res.AccountID != "acc_1" {
		t.Fatalf("account id = %q, want the row that was already there", res.AccountID)
	}
}
```

- [ ] **Step 2: 跑测试，看它过了**

Run: `go test ./internal/provision/`
Expected: PASS。

- [ ] **Step 3: 提交**

```bash
git add internal/provision/
git commit -m "provision: 导入不带出口了，账号自己那个地址也不再被清掉"
```

---

### Task 8: 健康检查查在用的那些地址

**Files:**
- Modify: `internal/scheduler/scheduler.go`

- [ ] **Step 1: 改查什么**

`internal/scheduler/scheduler.go`：

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
	for _, url := range egress.OutletURLs(accounts, s.Cfg.Proxy.Default) {
		probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err := s.Mgr.Egress.Check(probeCtx, url, target)
		cancel()
		msg := ""
		if err != nil {
			msg = err.Error()
		}
		// The write runs under a context of its own: the caller's may
		// be over already, and the health of the outlet has to be
		// written down all the same.
		writeCtx, writeCancel := context.WithTimeout(context.WithoutCancel(ctx), egressWriteTimeout)
		_ = s.Mgr.Store.SetEgressHealth(writeCtx, url, err == nil, msg)
		writeCancel()
	}
}
```

- [ ] **Step 2: 跑测试，看它过了**

Run: `go test ./internal/scheduler/`
Expected: PASS。

- [ ] **Step 3: 提交**

```bash
git add internal/scheduler/
git commit -m "scheduler: 出口健康检查查的是在用的那些地址"
```

---

### Task 9: WARP 出项目

**Files:**
- Modify: `docker-compose.yml`
- Modify: `.env.example`
- Modify: `config.example.yaml`
- Modify: `config.docker.yaml`
- Modify: `README.md`

- [ ] **Step 1: compose 里那个 WARP 容器没有了**

`docker-compose.yml`：

```yaml
services:
  mmxcode:
    build: .
    image: mmxcode-gateway:latest
    restart: unless-stopped
    ports:
      # 只绑本机，管理界面不暴露到公网
      - "127.0.0.1:8080:8080"
    volumes:
      - ${MMXCODE_DATA_DIR:-mmxcode-data}:/data
      - ${MMXCODE_CONFIG_FILE:-./config.docker.yaml}:/app/config.yaml:ro
    environment:
      MMXCODE_CONFIG: /app/config.yaml
      MMXCODE_API_KEYS: "${MMXCODE_API_KEYS:-}"
      MMXCODE_PROXY_DEFAULT: "${MMXCODE_PROXY_DEFAULT:-http://host.docker.internal:7890}"
      MMXCODE_PASSWORD_KEY: ${MMXCODE_PASSWORD_KEY:-}
      TZ: Asia/Shanghai
    extra_hosts:
      - "host.docker.internal:host-gateway"

volumes:
  mmxcode-data:
```

- [ ] **Step 2: 预置的组没有了**

`config.example.yaml`：

```yaml
proxy:
  # 全局默认出口。"direct" 表示不走代理。
  # 出口只有两级：账号自己的地址（在网页上每一行都能填），然后是这里
  # 的全局默认。两个都没有就是直连。
  default: "http://127.0.0.1:7890"
```

`config.docker.yaml`：

```yaml
proxy:
  # 全局默认出口。出口只有两级：账号自己的地址（在网页上每一行都能
  # 填），然后是这里。两个都没有就是直连。
  default: "http://host.docker.internal:7890"
```

`.env.example` 里 WARP 那三行注释没有了。

- [ ] **Step 3: 文档**

`README.md`：代理模型那节改成两级；「Cloudflare WARP」那一节删掉；配置表里
`proxy.groups` 那一行删掉；`batch.group` 那一行删掉；命令那几节的例子改掉。

- [ ] **Step 4: 看一遍还有没有漏的**

```bash
grep -rn "warp\|WARP" --include=*.go --include=*.yaml --include=*.yml --include=*.md . | grep -v "^./docs/"
```

Expected: 没有出口（`docs/` 里的是老计划和老规格，是历史，留着）。

- [ ] **Step 5: 跑全套的测试**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: 全过。

- [ ] **Step 6: 提交**

```bash
git add docker-compose.yml .env.example config.example.yaml config.docker.yaml README.md
git commit -m "build: WARP 出项目，配置里不再预置出口组"
```

---

## 前后两遍

|  | 前 | 后 |
|---|---|---|
| 出口的级别 | 三级：地址 → 组 → 全局 | 两级：地址 → 全局 |
| 配置里的 `proxy.groups` | 出口组的名字表 | 退役：还认，只被开机读一次 |
| 配置里的 `batch.group` | 导入走哪个组 | 退役：还认，不用 |
| 账号行上的组名 | 账号指着一组 | 没有了：开机变成账号自己的地址 |
| 账号页 | 组下拉框 + 地址输入框 | 只有地址输入框 |
| 出口页 | 配置里的那些组 | 在用的那些地址 |
| 健康检查 | 查配置里的组 | 查在用的那些地址 |
| 导入 | 走指定的那个组 | 走全局默认 |
| 重登 | 走指定的那个组或账号自己的组 | 走账号自己的出口 |
| compose | 有个 WARP 容器（默认不起） | 没有了 |
