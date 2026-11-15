# 后台申请 api key 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在后台页面上申请聊天用的 api key，申请来的那一把能开聊天接口，开不了管理界面。

**Architecture:** `internal/apikey` 做 key 本身（生成、哈希、可以摆出来的那一段）；`internal/store` 存它的哈希（表 `api_keys`），key 本身不入库；`internal/gateway` 分两扇门 —— 聊天那扇（`/v1/`）认两把 key，管理页那扇（`/api/` 和页面）只认配置里那把；`internal/webui` 的 `/keys` 页和 `/api/keys` 管申请和撤销。

**Tech Stack:** Go 1.26、`modernc.org/sqlite`、`net/http`、`html/template`、原生 JS

---

## 三条必须守住的不变量

1. **key 本身不入库。** 库里存的是它的哈希（SHA-256）。数据库被人拿走的时候，里面没有一把能开门的 key。
2. **申请来的 key 开不了管理界面。** 它是给别的机器聊天用的；能开管理页的是配置里那把（后台登录 key）。
3. **key 只给得出一次。** 申请的那一次回答里有它本人，往后页面上只有它的开头（`sk-` 加八个字符）。

### Task 1: apikey 包

**Files:**
- Create: `internal/apikey/apikey.go`
- Create: `internal/apikey/apikey_test.go`

- [ ] **Step 1: 写失败的测试**

创建 `internal/apikey/apikey_test.go`：

```go
package apikey

import (
	"strings"
	"testing"
)

// TestANewKeyIsDifferentEveryTime says two keys are never the same.
func TestANewKeyIsDifferentEveryTime(t *testing.T) {
	a, err := New()
	if err != nil {
		t.Fatal(err)
	}
	b, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatalf("two keys are the same: %q", a)
	}
	if !strings.HasPrefix(a, prefix) {
		t.Errorf("key = %q, want the %q prefix", a, prefix)
	}
}

// TestTheHashIsTheSameForTheSameKey says a key always hashes the same.
//
// 网关要靠这个认出「来的是不是那一把 key」：请求带来的 key 算一次
// 哈希，库里存的也是一次哈希。
func TestTheHashIsTheSameForTheSameKey(t *testing.T) {
	k, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if Hash(k) != Hash(k) {
		t.Fatal("the same key hashed two ways")
	}
	if Hash(k) == k {
		t.Fatal("the key is stored as it is")
	}
}

// TestTwoKeysDoNotHashTheSame says two keys do not share a hash.
func TestTwoKeysDoNotHashTheSame(t *testing.T) {
	a, _ := New()
	b, _ := New()
	if Hash(a) == Hash(b) {
		t.Fatal("two keys hash the same")
	}
}

// TestTheHashOfNothingIsTheHashOfNothing says an empty key still hashes.
//
// 空 key 不是一把 key：网关拒绝它，不是因为它算不出哈希，是因为它跟
// 配置里那把空的一样，什么都能开。
func TestTheHashOfNothingIsTheHashOfNothing(t *testing.T) {
	if Hash("") != Hash("") {
		t.Fatal("the empty key hashed two ways")
	}
}

// TestTheDisplayIsNotTheKey says what a page shows is not the key.
//
// 页面上要认得出哪一把是哪一把，又不能把它整个摊在那儿。
func TestTheDisplayIsNotTheKey(t *testing.T) {
	k, _ := New()
	d := Display(k)
	if d == k {
		t.Fatal("the display is the whole key")
	}
	if !strings.HasPrefix(d, prefix) {
		t.Errorf("display = %q, want the %q prefix", d, prefix)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

```
go test ./internal/apikey/
```

失败：`undefined: New`。

- [ ] **Step 3: 实现 apikey**

创建 `internal/apikey/apikey.go`：

```go
package apikey

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// The prefix a key carries.
//
// 站点自己的 key 也是这个开头：网关不认它，只是让人一眼看出这是一把
// key，而不是别的一串字符。
const prefix = "sk-"

// New makes a key.
//
// 32 个字节的乱数，base64url 编出来。key 猜不出来：256 位的乱数不是
// 能穷举的，而它又是这个网关唯一认的东西。
func New() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("random: %w", err)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// Hash gives the hash of a key.
//
// 库里存的是哈希，不是 key 本身：数据库被人拿走的时候，里面没有一把能
// 用的 key。
//
// 哈希是 SHA-256：key 自己有 256 位的乱数，慢哈希在这里什么也换不来
// ——慢哈希是为了猜密码的，不是为了猜乱数的。
func Hash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// Display gives the part of a key that may be shown.
//
// 它是给人看的：让人认得出「这一把是我刚才那把」，又认不出它能开门。
// 前八个字符加上开头，够认了。
func Display(key string) string {
	if len(key) <= len(prefix)+8 {
		return key
	}
	return key[:len(prefix)+8] + "…"
}
```

- [ ] **Step 4: 运行测试确认通过**

```
go test ./internal/apikey/
```

- [ ] **Step 5: 提交**

```
git add internal/apikey
git commit -m "feat(apikey): key 自己会生成，也会算哈希"
```

### Task 2: 库里的 api_keys 表

**Files:**
- Modify: `internal/store/schema.go`（加 `api_keys` 表）
- Modify: `internal/store/store.go`（`APIKey` 和它的五个方法）
- Modify: `internal/store/store_test.go`

- [ ] **Step 1: 写失败的测试**

在 `internal/store/store_test.go` 里加：

```go
// TestAKeyIsWrittenAndFound says a key that is written down can be
// found again by its hash.
//
// 网关认一把 key，靠的就是它的哈希：库里存哈希，请求带来的 key 算一次
// 哈希，两边对上了就是那一把。
func TestAKeyIsWrittenAndFound(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now()

	plain, err := apikey.New()
	if err != nil {
		t.Fatal(err)
	}
	hash := apikey.Hash(plain)
	if err := s.CreateAPIKey(ctx, APIKey{
		ID: "key_1", Name: "给我的笔记本", KeyHash: hash,
		Prefix: apikey.Display(plain), CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	got, err := s.APIKeyByHash(ctx, hash)
	if err != nil {
		t.Fatalf("the key by its hash: %v", err)
	}
	if got.ID != "key_1" {
		t.Fatalf("key = %+v, want the one that was written", got)
	}
	if got.Name != "给我的笔记本" {
		t.Fatalf("name = %q", got.Name)
	}
	if got.RevokedAt != nil {
		t.Fatalf("the key is already revoked: %v", got.RevokedAt)
	}
}

// TestTheKeyItselfIsNotInTheDatabase says the database does not keep the
// key.
//
// 数据库被人拿走的时候，里面不该有一把能开门的 key：它存的是哈希。
func TestTheKeyItselfIsNotInTheDatabase(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now()

	plain, err := apikey.New()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateAPIKey(ctx, APIKey{
		ID: "key_1", Name: "n", KeyHash: apikey.Hash(plain),
		Prefix: apikey.Display(plain), CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	keys, err := s.ListAPIKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Fatalf("keys = %v, want one", keys)
	}
	if strings.Contains(keys[0].KeyHash, plain) {
		t.Fatal("the key itself is in the database")
	}
	if keys[0].KeyHash != apikey.Hash(plain) {
		t.Fatalf("hash = %q, want the hash of the key", keys[0].KeyHash)
	}
}

// TestARevokedKeyIsNotFound says a revoked key does not open the gate.
//
// 撤销是把门关上：撤了的那一把不该再能进门。它还在库里——人得看得见
// 它曾经有过，也得说得清它什么时候没的。
func TestARevokedKeyIsNotFound(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now()

	plain, _ := apikey.New()
	hash := apikey.Hash(plain)
	if err := s.CreateAPIKey(ctx, APIKey{
		ID: "key_1", Name: "n", KeyHash: hash,
		Prefix: apikey.Display(plain), CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeAPIKey(ctx, "key_1", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.APIKeyByHash(ctx, hash); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want not found", err)
	}
	keys, err := s.ListAPIKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Fatalf("keys = %v, want the revoked one still to be there", keys)
	}
	if keys[0].RevokedAt == nil {
		t.Fatal("the key does not say when it was revoked")
	}
}

// TestTheKeysAreListedNewestFirst says the newest key comes first.
func TestTheKeysAreListedNewestFirst(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now()

	for i, id := range []string{"key_1", "key_2"} {
		if err := s.CreateAPIKey(ctx, APIKey{
			ID: id, Name: id, KeyHash: apikey.Hash(id),
			Prefix: "sk-…", CreatedAt: now.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
	}
	keys, err := s.ListAPIKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 {
		t.Fatalf("keys = %v, want two", keys)
	}
	if keys[0].ID != "key_2" {
		t.Fatalf("the first key is %q, want the newest one", keys[0].ID)
	}
}

// TestAKeyThatIsNotThereIsNotFound says a key that was never written is
// not found.
func TestAKeyThatIsNotThereIsNotFound(t *testing.T) {
	s := openTest(t)
	if _, err := s.APIKeyByHash(context.Background(), "nothing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want not found", err)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

```
go test ./internal/store/ -run "Key"
```

失败：`unknown: APIKey`。

- [ ] **Step 3: 加表**

`internal/store/schema.go` 的 `tables` 里加这一条（这张表不属于任何一个账号，所以它不在 `childTables` 里）：

```go
	{
		// The keys that open the chat API. They are not an account's:
		// a key belongs to the gateway, not to a MiniMax account, so it
		// has no account to be taken away with.
		//
		// The key itself is not in here: the hash of it is. The database
		// is worth stealing; a key in it would open the gate.
		name: "api_keys",
		body: `(
  id         TEXT PRIMARY KEY,
  name       TEXT NOT NULL,
  key_hash   TEXT NOT NULL UNIQUE,
  prefix     TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  revoked_at INTEGER
)`,
		copy: []copyColumn{
			notNull("id", "''"),
			notNull("name", "''"),
			notNull("key_hash", "''"),
			notNull("prefix", "''"),
			notNull("created_at", "0"),
			nullable("revoked_at"),
		},
	},
```

- [ ] **Step 4: 实现库的方法**

`internal/store/store.go` 里加：

```go
// APIKey is one key that opens the chat API.
//
// It is the gateway's key, not a MiniMax account's: it has no account
// under it, and it goes when it is revoked, not when an account is
// deleted.
type APIKey struct {
	// ID is the key's own id.
	ID string `json:"id"`
	// Name is the label the person gave it.
	Name string `json:"name"`
	// KeyHash is the hash of the key.
	//
	// The key itself is not kept: the database is worth stealing, and a
	// key in it would open the gate.
	KeyHash string `json:"-"`
	// Prefix is the part of the key that may be shown.
	Prefix string `json:"prefix"`
	// CreatedAt is when the key was made.
	CreatedAt time.Time `json:"created_at"`
	// RevokedAt is when the key was taken away.
	//
	// It is nil while the key works. A revoked key is still in here:
	// a person has to see that it was there and when it stopped working.
	RevokedAt *time.Time `json:"revoked_at"`
}

// Revoked says whether the key has been taken away.
func (k APIKey) Revoked() bool { return k.RevokedAt != nil }

// CreateAPIKey writes a key down.
func (s *Store) CreateAPIKey(ctx context.Context, k APIKey) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO api_keys (id, name, key_hash, prefix, created_at, revoked_at)
		VALUES (?,?,?,?,?,NULL)`,
		k.ID, k.Name, k.KeyHash, k.Prefix, ms(k.CreatedAt))
	if err != nil {
		return fmt.Errorf("create api key: %w", err)
	}
	return nil
}

// ListAPIKeys gives the keys, the newest first.
func (s *Store) ListAPIKeys(ctx context.Context) ([]APIKey, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, key_hash, prefix, created_at, revoked_at
		FROM api_keys ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("list api keys: %w", err)
	}
	defer rows.Close()
	out := []APIKey{}
	for rows.Next() {
		var (
			k         APIKey
			createdAt int64
			revokedAt sql.NullInt64
		)
		if err := rows.Scan(&k.ID, &k.Name, &k.KeyHash, &k.Prefix, &createdAt, &revokedAt); err != nil {
			return nil, fmt.Errorf("list api keys: %w", err)
		}
		k.CreatedAt = fromMS(createdAt)
		if revokedAt.Valid {
			t := fromMS(revokedAt.Int64)
			k.RevokedAt = &t
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list api keys: %w", err)
	}
	return out, nil
}

// RevokeAPIKey takes a key away.
//
// 它是撤销，不是删除：那一行还在，人看得到它曾经有过，也看得到它什么
// 时候没的。删掉的话，人会以为那把 key 从来没有存在过。
func (s *Store) RevokeAPIKey(ctx context.Context, id string, at time.Time) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE api_keys SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`,
		ms(at), id)
	if err != nil {
		return fmt.Errorf("revoke api key: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("revoke api key: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("revoke api key %s: %w", id, ErrNotFound)
	}
	return nil
}

// APIKeyByHash gives the key that hash belongs to.
//
// 撤销过的那一把不算：它已经不能开门了，找它没有意义。
func (s *Store) APIKeyByHash(ctx context.Context, hash string) (APIKey, error) {
	hash = strings.TrimSpace(hash)
	if hash == "" {
		return APIKey{}, fmt.Errorf("get the api key by its hash: %w", ErrNotFound)
	}
	var (
		k         APIKey
		createdAt int64
	)
	row := s.db.QueryRowContext(ctx, `
		SELECT id, name, key_hash, prefix, created_at
		FROM api_keys WHERE key_hash = ? AND revoked_at IS NULL`, hash)
	if err := row.Scan(&k.ID, &k.Name, &k.KeyHash, &k.Prefix, &createdAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return APIKey{}, fmt.Errorf("get the api key by its hash: %w", ErrNotFound)
		}
		return APIKey{}, fmt.Errorf("get the api key by its hash: %w", err)
	}
	k.CreatedAt = fromMS(createdAt)
	return k, nil
}

// HasAPIKeys says whether any key has been made.
//
// 一把都没有的时候网关不鉴权：那时候它只该听在本机。申请过就不一样
// 了：那是有人打算从别的机器用它。
func (s *Store) HasAPIKeys(ctx context.Context) (bool, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM api_keys WHERE revoked_at IS NULL`).Scan(&n); err != nil {
		return false, fmt.Errorf("count api keys: %w", err)
	}
	return n > 0, nil
}
```

- [ ] **Step 5: 运行测试确认通过**

```
go test ./internal/store/
```

- [ ] **Step 6: 提交**

```
git add internal/store
git commit -m "feat(store): 存 key 的哈希，不存 key"
```

### Task 3: 网关的两扇门

**Files:**
- Modify: `internal/gateway/auth.go`
- Modify: `internal/gateway/gateway.go`
- Modify: `internal/gateway/gateway_test.go`

- [ ] **Step 1: 写失败的测试**

在 `internal/gateway/gateway_test.go` 里加：

```go
// TestAKeyMadeInTheConsoleOpensTheChatAPI says a key made in the
// console opens the chat API.
//
// 后台申请的那把 key 是给别的机器用的：它得能聊天。
func TestAKeyMadeInTheConsoleOpensTheChatAPI(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	})
	// 配置里有 key，所以门是关着的：这时候进来的那把才真是靠它开的门。
	h.gw.Cfg.Server.APIKeys = []string{"sk-console"}
	plain := makeAKey(t, h.store)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	req.Header.Set("x-api-key", plain)
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code == http.StatusUnauthorized {
		t.Fatal("the key made in the console was refused")
	}
}

// TestARevokedKeyDoesNotOpenTheChatAPI says a revoked key opens
// nothing.
//
// 撤销是把门关上：撤了的那一把不该再能进门。
func TestARevokedKeyDoesNotOpenTheChatAPI(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	})
	h.gw.Cfg.Server.APIKeys = []string{"sk-console"}
	plain := makeAKey(t, h.store)
	if err := h.store.RevokeAPIKey(context.Background(), "key_1", time.Now()); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	req.Header.Set("x-api-key", plain)
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401 for a revoked key", w.Code)
	}
}

// TestAKeyMadeInTheConsoleDoesNotOpenTheConsole says a chat key is not
// a console key.
//
// 后台登录用的是另一把 key（配置里那把）：聊天用的 key 不该也能打开
// 管理界面。
func TestAKeyMadeInTheConsoleDoesNotOpenTheConsole(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	})
	h.gw.Cfg.Server.APIKeys = []string{"sk-console"}
	plain := makeAKey(t, h.store)

	req := httptest.NewRequest(http.MethodGet, "/api/usage", nil)
	req.Header.Set("x-api-key", plain)
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401: a chat key opened the console", w.Code)
	}

	// 后台那把 key 还是能进后台。
	req = httptest.NewRequest(http.MethodGet, "/api/usage", nil)
	req.Header.Set("x-api-key", "sk-console")
	w = httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code == http.StatusUnauthorized {
		t.Fatal("the console key was refused")
	}
}

// makeAKey writes a key down and gives it back in the clear.
func makeAKey(t *testing.T, s *store.Store) string {
	t.Helper()
	plain, err := apikey.New()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateAPIKey(context.Background(), store.APIKey{
		ID: "key_1", Name: "给我的笔记本", KeyHash: apikey.Hash(plain),
		Prefix: apikey.Display(plain), CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	return plain
}
```

- [ ] **Step 2: 运行测试确认失败**

```
go test ./internal/gateway/ -run "TestAKeyMadeInTheConsole|TestARevokedKeyDoesNotOpen"
```

失败：`the key made in the console was refused`。

- [ ] **Step 3: 实现鉴权**

`internal/gateway/auth.go` 里改 `auth`，把它拆成两半：认 key 的那半（`authWith`）和聊天那扇门（`authChat`）。

```go
// auth checks the request's key.
//
// The key comes in the header, the way it always did, or in the cookie
// the browser carries.
//
// 没有配 key 的时候网关不查：那时候它只该听在本机，serve 那一条命令
// 管这件事。
func (s *Server) auth(r *http.Request) bool {
	keys := s.Cfg.Server.APIKeys
	if len(keys) == 0 {
		return true
	}
	return s.authWith(r, keys)
}

// authWith checks the request's key against the given keys.
//
// 头里的 key 和浏览器带的 cookie 都认。
func (s *Server) authWith(r *http.Request, keys []string) bool {
	if _, ok := matchingKey(givenKey(r), keys); ok {
		return true
	}
	// 浏览器带来的 cookie：值是某一把配好的 key 算出来的
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	for _, k := range keys {
		if strings.TrimSpace(k) == "" {
			// 空的 key 算不出票来：它不该是谁的票。
			continue
		}
		if subtle.ConstantTimeCompare([]byte(c.Value), []byte(sessionValue(k))) == 1 {
			return true
		}
	}
	return false
}

// givenKey gives the key the request carries.
//
// 两个头都认：x-api-key，和 Authorization: Bearer。
func givenKey(r *http.Request) string {
	given := strings.TrimSpace(r.Header.Get("x-api-key"))
	if given != "" {
		return given
	}
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	}
	return ""
}

// authChat checks the request's key for the chat API.
//
// 两把 key 都认：配置里那把（后台登录用的），和在后台申请的那把（给
// 别的机器聊天用的）。
//
// 一把都没有的时候不鉴权：那时候网关只该听本机。申请过就不一样了：
// 那是有人打算从别的机器用它。
func (s *Server) authChat(r *http.Request) bool {
	keys := s.Cfg.Server.APIKeys
	if len(keys) == 0 && !s.hasMadeKeys(r.Context()) {
		return true
	}
	// 配置里那把，和浏览器带的 cookie。
	if s.authWith(r, keys) {
		return true
	}
	given := givenKey(r)
	if given == "" || s.Store == nil {
		return false
	}
	// 库里存的是哈希：请求带来的 key 算一次哈希，对上了就是那一把。
	// 撤销过的不算——它已经不能开门了。
	_, err := s.Store.APIKeyByHash(r.Context(), apikey.Hash(given))
	return err == nil
}

// hasMadeKeys says whether any key has been made in the console.
func (s *Server) hasMadeKeys(ctx context.Context) bool {
	if s.Store == nil {
		return false
	}
	have, err := s.Store.HasAPIKeys(ctx)
	return err == nil && have
}

// isChatPath says the path is one of the chat API's.
//
// 聊天那扇门和管理页那扇门不一样：聊天认两把 key，管理页只认一把。
func isChatPath(path string) bool {
	return path == "/v1" || strings.HasPrefix(path, "/v1/")
}
```

- [ ] **Step 4: 实现路由**

`internal/gateway/gateway.go` 的 `ServeHTTP`：路径是聊天接口的走 `authChat`，别的走 `auth`。

```go
// ServeHTTP routes a request.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/signin":
		// 登录页不用会话：没有会话的人正要来这里登录。
		s.signInPage(w, r, http.StatusOK, "")
		return
	case "/api/session":
		// 发 cookie 和销 cookie 都不用会话。
		s.session(w, r)
		return
	}
	// 两扇门：聊天那扇认两把 key（配置里的和后台申请的），管理页那扇只
	// 认配置里的那把。
	gate := s.auth
	if isChatPath(r.URL.Path) {
		gate = s.authChat
	}
	if !gate(r) {
		if isAPIPath(r.URL.Path) {
			writeError(w, protocolOf(r.URL.Path), http.StatusUnauthorized, "the key was refused")
			return
		}
		// 浏览器来的是页面：给它登录页，不是一段 JSON。
		s.signInPage(w, r, http.StatusOK, "")
		return
	}
	switch r.URL.Path {
	case "/v1/messages":
		s.handleChat(w, r, protocolAnthropic)
		return
	case "/v1/chat/completions":
		s.handleChat(w, r, protocolOpenAI)
		return
	case "/v1/responses":
		s.handleChat(w, r, protocolCodex)
		return
	case "/v1/models":
		s.handleModels(w, r)
		return
	}
	if s.WebUI != nil && strings.HasPrefix(r.URL.Path, "/api/") {
		s.WebUI.ServeHTTP(w, r)
		return
	}
	if s.WebUI != nil {
		s.WebUI.ServeHTTP(w, r)
		return
	}
	writeError(w, protocolOf(r.URL.Path), http.StatusNotFound, "no such endpoint")
}
```

- [ ] **Step 5: 运行测试确认通过**

```
go test ./internal/gateway/
```

- [ ] **Step 6: 提交**

```
git add internal/gateway
git commit -m "feat(gateway): 聊天那扇门认两把 key"
```

### Task 4: 后台的密钥页

**Files:**
- Create: `internal/webui/keys.go`
- Create: `internal/webui/keys_test.go`
- Create: `internal/webui/templates/keys.html`
- Modify: `internal/webui/webui.go`（路由）
- Modify: `internal/webui/templates/layout.html`（导航里加一页）
- Modify: `internal/webui/static/app.js`

- [ ] **Step 1: 写失败的测试**

创建 `internal/webui/keys_test.go`：

```go
package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mmxcode/internal/apikey"
)

// TestAKeyMadeInTheConsoleIsGivenOnce says the console hands the key back
// once, when it is made.
//
// 那一把 key 只在这一刻出现：库里存的是哈希，别的时刻它拿不出来。所以
// 这一次的回答里必须有它，而页面上别的时刻没有。
func TestAKeyMadeInTheConsoleIsGivenOnce(t *testing.T) {
	w, _ := newTestWebUI(t)
	req := httptest.NewRequest(http.MethodPost, "/api/keys", strings.NewReader(`{"name":"我的机器"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out struct {
		ID  string `json:"id"`
		Key string `json:"key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Key == "" {
		t.Fatal("the answer did not carry the key")
	}
	if out.ID == "" {
		t.Fatal("the answer did not carry the id")
	}
	// 再要一次：这一回只有列表，列表里没有 key 本身。
	list := httptest.NewRecorder()
	w.ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/api/keys", nil))
	if list.Code != 200 {
		t.Fatalf("code = %d", list.Code)
	}
	if strings.Contains(list.Body.String(), out.Key) {
		t.Fatal("the list carries the key itself")
	}
}

// TestTheKeyItselfIsNotInTheList says the list shows the prefix, not the
// key.
//
// 列表是给眼睛看的：认得出哪一把是哪一把就行，能开门的那个不给。
func TestTheKeyItselfIsNotInTheList(t *testing.T) {
	w, _ := newTestWebUI(t)
	req := httptest.NewRequest(http.MethodPost, "/api/keys", strings.NewReader(`{"name":"机器一"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	var out struct {
		Key string `json:"key"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)

	list := httptest.NewRecorder()
	w.ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/api/keys", nil))
	var listing struct {
		Keys []struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			Prefix string `json:"prefix"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Keys) != 1 {
		t.Fatalf("keys = %d, want 1", len(listing.Keys))
	}
	if listing.Keys[0].Name != "机器一" {
		t.Errorf("name = %q, want %q", listing.Keys[0].Name, "机器一")
	}
	if listing.Keys[0].Prefix == "" {
		t.Error("the list has no prefix")
	}
	if strings.Contains(list.Body.String(), out.Key) {
		t.Error("the list carries the key itself")
	}
}

// TestARevokedKeyIsRefused says a revoked key does not open the gate any
// more.
//
// 撤销之后它还在列表里：得让人看得见这把 key 曾经有过、什么时候不灵了。
// 只是它不灵了。
func TestARevokedKeyIsRefused(t *testing.T) {
	w, s := newTestWebUI(t)
	req := httptest.NewRequest(http.MethodPost, "/api/keys", strings.NewReader(`{"name":"机器二"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	var made struct {
		ID  string `json:"id"`
		Key string `json:"key"`
	}
	json.Unmarshal(rec.Body.Bytes(), &made)

	del := httptest.NewRecorder()
	w.ServeHTTP(del, httptest.NewRequest(http.MethodDelete, "/api/keys/"+made.ID, nil))
	if del.Code != 200 {
		t.Fatalf("code = %d, body = %s", del.Code, del.Body.String())
	}

	// 网关不认它了。
	if _, err := s.APIKeyByHash(t.Context(), apikey.Hash(made.Key)); err == nil {
		t.Fatal("the revoked key is still found")
	}
	// 再撤一次：没有了。
	again := httptest.NewRecorder()
	w.ServeHTTP(again, httptest.NewRequest(http.MethodDelete, "/api/keys/"+made.ID, nil))
	if again.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404", again.Code)
	}
}

// TestTheKeysPage says there is a page to ask for keys on.
func TestTheKeysPage(t *testing.T) {
	w, _ := newTestWebUI(t)
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/keys", nil))
	if rec.Code != 200 {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "密钥") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

```
go test ./internal/webui/ -run "Key"
```

失败：404。

- [ ] **Step 3: 实现密钥的接口**

创建 `internal/webui/keys.go`：

```go
package webui

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"mmxcode/internal/apikey"
	"mmxcode/internal/store"
)

// keysData is the keys page's data.
//
// 页面上摆的是列表：每一把 key 认得出是哪一把（名字和开头），也摆得下
// 它什么时候不再管用了。key 本身不在这里——它在申请的那一次就交出去了。
func (w *WebUI) keysData(ctx context.Context) (map[string]any, error) {
	keys, err := w.Store.ListAPIKeys(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]keyView, 0, len(keys))
	for _, k := range keys {
		out = append(out, keyView{APIKey: k, Revoked: k.Revoked()})
	}
	return map[string]any{"Keys": out}, nil
}

// keyView is one key as the page shows it.
type keyView struct {
	store.APIKey
	// Revoked says whether the key has been taken away.
	Revoked bool
}

// apiKeys lists the keys, or makes one.
//
// POST 的回答里有 key 本身：那是它唯一一次出现。库里存的是哈希，别的
// 时刻拿不出来，也就不该拿得出来。
func (w *WebUI) apiKeys(rw http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		// 列表给的是库里的那些行：id、名字、开头、时间。key 本身不在
		// 里面 —— 它只交过一次。
		keys, err := w.Store.ListAPIKeys(r.Context())
		if err != nil {
			http.Error(rw, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(rw, map[string]any{"keys": keys})
	case http.MethodPost:
		w.makeKey(rw, r)
	default:
		rw.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// makeKey makes one key and gives it back.
func (w *WebUI) makeKey(rw http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	name := strings.TrimSpace(body.Name)
	if name == "" {
		// 没起名字的 key 也得有个名字：列表里「（没名字）」比一列空白
		// 好认。
		name = "（没名字）"
	}
	key, err := apikey.New()
	if err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}
	now := time.Now()
	k := store.APIKey{
		ID:        newKeyID(),
		Name:      name,
		KeyHash:   apikey.Hash(key),
		Prefix:    apikey.Display(key),
		CreatedAt: now,
	}
	if err := w.Store.CreateAPIKey(r.Context(), k); err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}
	// 这是它唯一一次出现：往后页面上只有它的开头。
	writeJSON(rw, map[string]any{
		"id": k.ID, "name": k.Name, "key": key, "prefix": k.Prefix,
	})
}

// apiKey takes one key away.
//
// DELETE /api/keys/{id}。
func (w *WebUI) apiKey(rw http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/keys/")
	if id == "" {
		http.NotFound(rw, r)
		return
	}
	switch r.Method {
	case http.MethodDelete:
		if err := w.Store.RevokeAPIKey(r.Context(), id, time.Now()); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				http.Error(rw, "没有这一把 key", http.StatusNotFound)
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

// newKeyID makes the id of one key.
//
// 时间还不够：两次申请可能落在同一毫秒里，那样的第二个会被主键挡下。
func newKeyID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("key_%d", time.Now().UnixMilli())
	}
	return fmt.Sprintf("key_%d_%s", time.Now().UnixMilli(), hex.EncodeToString(b))
}
```

- [ ] **Step 4: 写页面**

`internal/webui/templates/keys.html`：

```html
{{define "content"}}
<h2>密钥</h2>
<p>聊天用的 key 在这里申请。申请出来的那一把只在这一页出现一次：抄走收好，之后就再也拿不出来了（库里存的是它的哈希，不是它）。</p>
<form id="key-form">
  <input id="key-name" placeholder="这把 key 给谁用（比如：我的笔记本）">
  <button type="submit">申请一把</button>
  <span class="muted" id="key-status"></span>
</form>
<div id="key-new" hidden>
  <p>这一把 key 就是它了，抄走收好：</p>
  <p><code id="key-plain"></code> <button type="button" id="key-copy">复制</button></p>
</div>
<table>
<tr><th>名字</th><th>开头</th><th>申请时间</th><th>状态</th><th></th></tr>
<tbody id="key-rows">
{{range .Keys}}
<tr>
  <td>{{.Name}}</td>
  <td><code>{{.Prefix}}</code></td>
  <td class="muted">{{.CreatedAt.Format "2006-01-02 15:04"}}</td>
  <td>{{if .Revoked}}<span class="muted">已撤销</span>{{else}}<span class="good">能用</span>{{end}}</td>
  <td>{{if not .Revoked}}<button type="button" data-revoke="{{.ID}}">撤销</button>{{end}}</td>
</tr>
{{end}}
</tbody>
</table>
{{end}}
```

`internal/webui/templates/layout.html` 的导航里加一行：

```html
	<a href="/keys">密钥</a>
```

`internal/webui/static/app.js` 里加：

```js
// 密钥：申请一把新的，撤销一把旧的。
//
// 申请出来的那一把 key 只在这一刻给得出：往后页面上只有它的开头。所以
// 它得摆在人眼前，还得能复制走。
document.addEventListener('submit', async (ev) => {
  const form = ev.target.closest('#key-form');
  if (!form) return;
  ev.preventDefault();
  const status = document.getElementById('key-status');
  const name = document.getElementById('key-name');
  if (status) status.textContent = '正在申请……';
  try {
    const out = await post('/api/keys', { name: name ? name.value : '' });
    const plain = document.getElementById('key-plain');
    if (plain) plain.textContent = out.key;
    const box = document.getElementById('key-new');
    if (box) box.hidden = false;
    if (status) status.textContent = '申请好了。抄走收好：过了这一会儿就拿不出来了。';
    if (name) name.value = '';
    await renderKeys();
  } catch (e) {
    if (status) status.textContent = '失败：' + e.message;
  }
});

document.addEventListener('click', async (ev) => {
  const copy = ev.target.closest('#key-copy');
  if (copy) {
    const plain = document.getElementById('key-plain');
    if (!plain) return;
    try {
      await navigator.clipboard.writeText(plain.textContent);
      copy.textContent = '复制好了';
    } catch (e) {
      // 剪贴板不给用的时候，人自己选中那段字也一样。
      copy.textContent = '选中最下面那段字自己复制';
    }
    return;
  }
  const revoke = ev.target.closest('[data-revoke]');
  if (revoke) {
    revoke.disabled = true;
    try {
      await del('/api/keys/' + encodeURIComponent(revoke.dataset.revoke));
      await renderKeys();
    } catch (e) {
      revoke.disabled = false;
      const status = document.getElementById('key-status');
      if (status) status.textContent = '失败：' + e.message;
    }
  }
});

// renderKeys 把钥匙列表重新摆一遍。
//
// 页面是服务端渲染的，可这一页的列表是刚刚才变的：重新取一遍整页会把
// 刚拿到手的那把 key 弄没，所以只把表格重摆一次。
async function renderKeys() {
  const body = document.getElementById('key-rows');
  if (!body) return;
  const res = await fetch('/api/keys');
  const answer = await readAnswer(res);
  if (!answer.ok) return;
  const keys = Array.isArray(answer.json.keys) ? answer.json.keys : [];
  body.textContent = '';
  for (const k of keys) {
    const tr = document.createElement('tr');
    const revoked = !!k.revoked_at;
    const cells = [
      k.name || '（没名字）',
      k.prefix || '',
      (k.created_at || '').replace('T', ' ').slice(0, 16),
    ];
    for (const c of cells) {
      const td = document.createElement('td');
      td.textContent = c;
      tr.appendChild(td);
    }
    const state = document.createElement('td');
    const label = document.createElement('span');
    if (revoked) {
      label.textContent = '已撤销';
      label.className = 'muted';
    } else {
      label.textContent = '能用';
      label.className = 'good';
    }
    state.appendChild(label);
    tr.appendChild(state);
    const last = document.createElement('td');
    if (!revoked) {
      const button = document.createElement('button');
      button.type = 'button';
      button.textContent = '撤销';
      button.dataset.revoke = k.id;
      last.appendChild(button);
    }
    tr.appendChild(last);
    body.appendChild(tr);
  }
}
```

- [ ] **Step 5: 挂路由**

`internal/webui/webui.go` 的 `ServeHTTP`：

```go
// ServeHTTP routes a request.
func (w *WebUI) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/static/app.js":
		w.serveStatic(rw, r)
		return
	case "/api/accounts":
		w.apiAccounts(rw, r)
		return
	case "/api/accounts/poll":
		w.apiPoll(rw, r)
		return
	case "/api/usage":
		w.apiUsage(rw, r)
		return
	case "/api/usage/refresh":
		w.apiUsageRefresh(rw, r)
		return
	case "/api/egress":
		w.apiEgress(rw, r)
		return
	case "/api/egress/test":
		w.apiEgressTest(rw, r)
		return
	case "/api/sessions":
		w.apiSessions(rw, r)
		return
	case "/api/events":
		w.apiEvents(rw, r)
		return
	case "/api/import":
		w.apiImport(rw, r)
		return
	case "/api/relogin":
		w.apiRelogin(rw, r)
		return
	case "/api/keys":
		w.apiKeys(rw, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/keys/") {
		w.apiKey(rw, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/accounts/") {
		w.apiAccount(rw, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/import/") {
		w.apiImportRun(rw, r)
		return
	}
	switch r.URL.Path {
	case "/":
		w.page(rw, r, "accounts.html", w.accountsData)
	case "/egress":
		w.page(rw, r, "egress.html", w.egressData)
	case "/usage":
		w.page(rw, r, "usage.html", w.usageData)
	case "/login":
		w.page(rw, r, "login.html", w.loginData)
	case "/import":
		// 只有没配 key 的网关才开放上传：配了 key 就说明别的机器能
		// 进来，明文密码不该从那儿过。
		if len(w.Cfg.Server.APIKeys) > 0 {
			http.NotFound(rw, r)
			return
		}
		w.page(rw, r, "import.html", w.importData)
	case "/relogin":
		// 重登页不受上传那个闸限制：它不接触明文密码。
		w.page(rw, r, "relogin.html", w.reloginData)
	case "/keys":
		w.page(rw, r, "keys.html", w.keysData)
	default:
		http.NotFound(rw, r)
	}
}
```

- [ ] **Step 6: 运行测试确认通过**

```
go test ./internal/webui/
```

- [ ] **Step 7: 提交**

```
git add internal/webui
git commit -m "feat(webui): 后台能申请和撤销聊天用的 key"
```

### Task 5: 文档

**Files:**
- Modify: `README.md`

- [ ] **Step 1: 写清楚两把 key 的分工**

README 的「鉴权」一节改成两把 key 的表：后台登录 key（配置里的）开管理界面，聊天 key（后台申请的）开聊天接口。聊天接口两把都认，管理界面只认第一把。

- [ ] **Step 2: 写清楚 key 只给得出一次**

README 写明：库里存的是哈希，申请的那一次回答里有 key 本身，往后拿不回来；撤销是撤销不是删除，那一行还在列表里。

- [ ] **Step 3: 提交**

```
git add README.md
git commit -m "docs(readme): 两把 key 的分工和申请的那一次"
```

