# 网关的 SQLite：写锁、丢不得的那两笔、和那条擦不掉的记号 —— 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 那条记号（旧的版本写的）擦掉；事务里写的令牌落盘；库说忙的时候再试一次。

**Architecture:** store 里多一个 `busy.go`：按**结果码**认忙（驱动给的
`SQLITE_BUSY`/`SQLITE_LOCKED`），丢不得的那两笔（续签后的令牌、清掉的「用完
了」标记）和事务在库说忙的时候再试一遍 —— 一遍等一秒，一共五遍，五秒那个顶
跟从前一样。事务改走那条会 fsync 的连接，所以事务里写的令牌是落盘的。那条
fsync 的连接从一条变成两条：两个写不排在 Go 这边（从前是 5 秒和 7.8 秒）。
manager 那头：库里那份令牌追上来了，那条记号就擦（旧版本写的那一条也认）；
开机的时候把旧版本写的记号擦一遍。

**Tech Stack:** Go 1.26、`modernc.org/sqlite v1.62.0`（纯 Go，不用 CGO）、
标准库（`context`、`time`、`unicode/utf8`）。

**规格：** `docs/superpowers/specs/2026-11-21-mmxcode-sqlite-write-lock-design.md`

## 文件一览

| 文件 | 干什么 |
|---|---|
| `internal/store/busy.go`（新） | 按结果码认忙；丢不得的那两笔再试一次 |
| `internal/store/busy_test.go`（新） | 这两件事的测试 |
| `internal/store/notes_test.go`（新） | 擦记号的测试 |
| `internal/store/store.go` | 等锁的工夫；事务走 fsync 那条连接；擦记号的原语 |
| `internal/store/store_test.go` | 事务里的令牌走哪条连接 |
| `internal/manager/manager.go` | 库追上了记号就擦；擦旧版本写的记号 |
| `internal/manager/manager_test.go` | 这两件事的测试 |
| `internal/provision/chain.go` | 按结果码认忙（不再靠字眼） |
| `internal/provision/chain_test.go` | 假库说的那句话照着驱动说的写 |
| `cmd/mmxcode/root.go` | 开机时擦一次 |

---

### Task 1: 库说忙的时候，再试一次

**Files:**
- Create: `internal/store/busy.go`
- Create: `internal/store/busy_test.go`
- Modify: `internal/store/store.go`

**为什么先看规格 §4 的洞三：** 库说忙的时候没人再试一次（store 里一处也没有），
而整个仓库里唯一认忙的地方（`internal/provision`）靠字眼认 —— 别的毛病也会
被认成忙。

**这一件事是一起的：** 等锁等多久（那条会 fsync 的连接等一秒）和「再来一遍」
是同一件事的两半 —— 等得短，再来一遍才划算（有那几遍）；不来一遍，等得短的
那个连接就比从前还差（等一秒就放弃了）。所以这两样在同一个任务里改，中间不
留那个更差的状态。

- [ ] **Step 1: 写失败的测试**

`internal/store/busy_test.go`（新文件，`package store`；import：
`context`、`database/sql`、`errors`、`path/filepath`、`testing`、`time`）：

```go
package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// withBusyTimeout says how long one write waits for the file's write lock.
//
// 测试用的：等锁的工夫在测试里等不起（一遍一秒多，一趟测试五秒，谁都受
// 不了）。
func withBusyTimeout(d time.Duration) Option {
	return func(o *options) { o.busyTimeout = d }
}

// openBusyTest opens a store whose writes wait a short time for the write
// lock, and gives the path of the file it is on.
//
// 另一个连接要占着那个锁，所以路径是给它的。
func openBusyTest(t *testing.T, wait time.Duration) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "busy.db")
	s, err := Open(path, withBusyTimeout(wait))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s, path
}

// holdTheWriteLock takes the file's write lock and keeps it until the
// returned function is called.
//
// 真库里另一个进程占着写锁就是这个样子：一个写事务开着，什么也不写。
func holdTheWriteLock(t *testing.T, path string) func() {
	t.Helper()
	db, err := sql.Open("sqlite", dsn(path, options{}))
	if err != nil {
		t.Fatal(err)
	}
	// The data source name says _txlock=immediate, so the transaction
	// takes the write lock when it starts.
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	return func() {
		_ = tx.Rollback()
		db.Close()
	}
}

// TestTheWriteThatMustNotBeLostIsTriedAgainWhenTheFileIsBusy says the
// write that must not be lost is tried again when the library says the
// file is busy.
//
// 写锁多半只被人占着几毫秒（一次写、一个事务），等一下就拿到了 —— 可是
// 「等一下」是库那个忙处理器的事：它越等越粗（一开始 1 毫秒，后来 100 毫
// 秒），占着锁的那件事中间让出来的空当（导入的时候，一笔账一个事务）它
// 就错过了。再来一遍就是新的忙处理器，它又从 1 毫秒开始等。
func TestTheWriteThatMustNotBeLostIsTriedAgainWhenTheFileIsBusy(t *testing.T) {
	ctx := context.Background()
	s, path := openBusyTest(t, 50*time.Millisecond)
	makeAccount(t, s, "acc_01")
	release := holdTheWriteLock(t, path)

	done := make(chan error, 1)
	go func() {
		done <- s.SaveToken(ctx, Token{
			AccountID: "acc_01", Access: "a", Refresh: "r",
			ExpiresAt: time.Now().Add(time.Hour), UpdatedAt: time.Now(),
		})
	}()
	// 让头一遍等满（它会失败），后面那几遍还在等的时候把锁放开。
	time.Sleep(200 * time.Millisecond)
	release()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the token was not written: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the write did not finish")
	}
	tok, err := s.GetToken(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if tok.Access != "a" {
		t.Errorf("the token in the store = %q, want %q", tok.Access, "a")
	}
}

// TestTheWriteFailsWhenTheFileStaysBusy says the write gives up when the
// file stays busy, and that the error says so.
//
// 锁一直被占着的时候，再试也是白等：那个错还是那个错，只是说得清楚。
func TestTheWriteFailsWhenTheFileStaysBusy(t *testing.T) {
	ctx := context.Background()
	s, path := openBusyTest(t, 50*time.Millisecond)
	makeAccount(t, s, "acc_01")
	release := holdTheWriteLock(t, path)
	defer release()

	err := s.SaveToken(ctx, Token{
		AccountID: "acc_01", Access: "a", Refresh: "r",
		ExpiresAt: time.Now().Add(time.Hour), UpdatedAt: time.Now(),
	})
	if err == nil {
		t.Fatal("the token was written while the file was busy")
	}
	if !Busy(err) {
		t.Errorf("the error = %v, want the one that says the file is busy", err)
	}
}

// TestBusyIsNotReadFromTheWords says the library's result code says the
// file is busy, not the words in the message.
//
// 「locked」「busy」这两个词别的毛病也会带着（"the server is busy"）。按
// 结果码认才不会把别的毛病当成忙。
func TestBusyIsNotReadFromTheWords(t *testing.T) {
	if Busy(errors.New("the server is busy")) {
		t.Error("an error that only says busy is not the file being busy")
	}
	if Busy(errors.New("the table is locked by the reader")) {
		t.Error("an error that only says locked is not the file being busy")
	}
	if !Busy(errors.New("database is locked (5) (SQLITE_BUSY)")) {
		t.Error("the library's own words say the file is busy")
	}
}

// TestTheDurablePoolHasRoomForTwoBlockedWrites says two writes do not
// queue up behind each other.
//
// 一条连接的时候，第二个写是在 Go 这边排队的：等第一条放弃了（等满了
// busy_timeout）它才拿到连接，然后自己再等一遍 —— 量出来是 5 秒和 7.8
// 秒。两条连接的时候，两个写一起在库的忙处理器里等，一起结束。
func TestTheDurablePoolHasRoomForTwoBlockedWrites(t *testing.T) {
	s := openTestOpts(t, WithSynchronous("normal"))
	if got := s.durable.Stats().MaxOpenConnections; got != 2 {
		t.Errorf("the durable pool has %d connections, want 2", got)
	}
}
```

- [ ] **Step 2: 跑测试，看它失败**

```bash
go test ./internal/store/ -run "TestTheWriteThatMustNotBeLost|TestBusyIsNotRead" -v
```

失败：编译不过（`Busy`、`withBusyTimeout` 都还没有）。

- [ ] **Step 3: 实现 `internal/store/busy.go`**

`internal/store/busy.go`（新文件）：

```go
package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"modernc.org/sqlite"
)

// The result codes SQLite says when the file is busy.
//
// 5 是 SQLITE_BUSY（别人占着写锁），6 是 SQLITE_LOCKED。延长的结果码
// （SQLITE_BUSY_SNAPSHOT 之类）低八位还是这两个。
const (
	sqliteBusy   = 5
	sqliteLocked = 6
)

// The waits of a write that must not be lost.
//
// 写锁多半只被人占着几毫秒（一次写、一个事务），等一下就拿到了。占得久
// 的是导入和迁移 —— 那不是等得久就能等到的，所以等的工夫有个顶，超了顶就
// 再来一遍：占着锁的那件事中间会让出来。
const (
	// busyBudget is how long the store keeps trying, in all.
	busyBudget = 5 * time.Second
	// busyPause is how long the store waits between two tries.
	busyPause = 50 * time.Millisecond
	// busyTriesMax is how many times at most a write is tried.
	busyTriesMax = 5
)

// Busy says the failure is the file being busy.
//
// 它按结果码认：驱动说的忙是 *sqlite.Error，结果码 5（SQLITE_BUSY）或 6
// （SQLITE_LOCKED）。延长的结果码（517 之类）低八位还是这两个。
//
// 不是驱动说的错（错被包过一层，类型丢了）就看那句话里有没有 SQLite 自己
// 的名字 —— "SQLITE_BUSY"、"SQLITE_LOCKED"。不看「locked」「busy」这两个
// 词：别的毛病也会带着它们（"the server is busy"）。
func Busy(err error) bool {
	if err == nil {
		return false
	}
	var se *sqlite.Error
	if errors.As(err, &se) {
		code := se.Code() & 0xff
		return code == sqliteBusy || code == sqliteLocked
	}
	msg := err.Error()
	return strings.Contains(msg, "SQLITE_BUSY") || strings.Contains(msg, "SQLITE_LOCKED")
}

// writeBusy does a write that must not be lost, and does it again when the
// library says the file was busy.
//
// 一遍等的是连接上那个 busy_timeout（丢不得的那两笔走的那条连接上是
// 一秒）。占着写锁的那件事要是短命（导入的时候，一笔账一个事务，中间的
// 空当只有几毫秒），库的忙处理器越等越粗（一开始 1 毫秒，后来 100 毫秒），
// 那个空当它就错过了 —— 再来一遍是新的忙处理器，它又从 1 毫秒开始等。
//
// 一共 busyTries 遍，几遍加起来是 busyBudget 那个顶。超了顶就还是那个错，
// 只是说得清楚：库忙着呢。
//
// 调用方的 context 到了就停：再试也是白试。
func (s *Store) writeBusy(ctx context.Context, db execer, query string, args ...any) error {
	var err error
	for i := 0; i < s.busyTries(); i++ {
		if i > 0 && !sleepCtx(ctx, busyPause) {
			break
		}
		_, err = db.ExecContext(ctx, query, args...)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil || !Busy(err) {
			return err
		}
	}
	return err
}

// busyTries is how many times a write that must not be lost is tried.
//
// 一遍等的是连接上那个 busy_timeout：一遍等满了还没拿到锁，占着锁的那件
// 事不是等一秒就能做完的。总共的工夫有顶（busyBudget），所以遍数是顶除
// 以一遍的工夫 —— 而且有个上限（最多 busyTriesMax 遍）。
func (s *Store) busyTries() int {
	if s.busyWait <= 0 {
		// The wait is not known: one try and no more.
		return 1
	}
	n := int(busyBudget / s.busyWait)
	if n > busyTriesMax {
		n = busyTriesMax
	}
	if n < 1 {
		n = 1
	}
	return n
}

// sleepCtx waits, and says whether the wait happened.
//
// The context ending stops the wait.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
```

- [ ] **Step 4: 改 `internal/store/store.go` —— 等锁的工夫**

`internal/store/store.go` —— 两条连接，等锁的工夫：

```go
// durableConns is how many connections the durable pool has.
//
// 丢不得的那两笔很少写（一次续签、一次签到救活了一个用完的号），写的工夫
// 比等的工夫长得多。
//
// 两条连接不是为了写得多快，是为了**不排队**：写锁只有一把，可是等着的人
// 不用在 Go 这边排。一条连接的时候，第二个写等的是一条空连接 —— 等第一条
// 放弃了（等满了 busy_timeout）它才拿到连接，然后自己再等一遍：量出来是
// 5 秒和 7.8 秒。两条连接的时候，两个写一起在库的忙处理器里等，一起结束。
const durableConns = 2

// The waits for the file's write lock.
//
// 写锁多半只被人占着几毫秒（一次写、一个事务），等一下就拿到了。占得久
// 的是导入和迁移 —— 那不是等得久就能等到的，所以等的工夫有个顶。
const (
	// defaultWait is how long the everyday writes wait for the write
	// lock.
	//
	// 平时那些写丢了都能自己好起来（额度快照旧一点、标记没写上撞一次空
	// 换号重试），所以它们等得久一点没关系。
	defaultWait = 5 * time.Second
	// durableWait is the longest the writes that must not be lost wait
	// for the write lock.
	//
	// 那两笔等得短一点，是为了让「再来一遍」有意义：库的忙处理器越等越
	// 粗（一开始 1 毫秒，后来 100 毫秒），占着锁的那件事中间让出来的空
	// 当（导入的时候，一笔账一个事务）它就错过了。再来一遍是新的忙处理
	// 器，它又从 1 毫秒开始等。
	//
	// 等的工夫短了，再来的遍数就多：几遍加起来还是那个顶（busyBudget）。
	durableWait = time.Second
)

```

`internal/store/store.go` —— `Store` 多一个字段：

```go
	// 丢不得的那两笔（续签后的 token、清掉的「用完了」标记）走它。常用的
	// 那些写还是走 db，按配的速度写（normal）—— 那些写丢了都能自己好起来
	// （额度快照旧一点、标记没写上撞一次空换号重试）。
	durable *sql.DB

	// busyWait is how long one try waits for the file's write lock.
	//
	// 它是丢不得的那两笔走的那条连接上的那个数（那边等一秒，平时那些写
	// 等五秒）。一遍等多久，决定了能试几遍：几遍加起来是那个顶
	// （busyBudget）。
	busyWait time.Duration
```

`internal/store/store.go` —— `options` 多一个字段：

```go
	// synchronous is how hard a write hits the disk. Empty leaves it to
	// SQLite.
	synchronous string
	// busyTimeout is how long one write waits for the file's write lock.
	// Zero leaves it at defaultWait.
	busyTimeout time.Duration
	// connector opens the connections. Nil means SQLite's own.
	connector Connector
}
```

`internal/store/store.go` —— 两个小函数，和 `dsn` 里那个数：

```go
// busyWaitOf is how long one write waits for the file's write lock.
//
// 平时那些写等得久一点没关系：它们丢了都能自己好起来。
func busyWaitOf(o options) time.Duration {
	if o.busyTimeout > 0 {
		return o.busyTimeout
	}
	return defaultWait
}

// durableWaitOf is how long the writes that must not be lost wait for the
// write lock.
//
// 那两笔等得短一点，是为了让「再来一遍」有意义（见 durableWait）。
func durableWaitOf(o options) time.Duration {
	if w := busyWaitOf(o); w < durableWait {
		return w
	}
	return durableWait
}

// dsn gives the data source name of a database file.
//
// The path is made a file: URI, so that a path with a question mark
// in it is the path and not the end of the file name. The pragmas
// come after the question mark; a pragma is per connection, so every
// connection of the pool gets them.
//
// The write lock is taken when a transaction starts and not in the
// middle of it. A transaction that finds the lock taken fails at once
// instead of failing at the end, after it has done its work.
func dsn(path string, o options) string {
	p := filepath.ToSlash(path)
	prefix := "file:"
	if strings.HasPrefix(p, "//") {
		// A path that begins with two slashes is a network path, and
		// the two slashes after "file:" are the empty authority that
		// says so.
		prefix = "file://"
	}
	q := prefix + escapePath(p) +
		"?_pragma=busy_timeout(" + fmt.Sprint(int(busyWaitOf(o)/time.Millisecond)) + ")" +
		"&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_txlock=immediate"
	if o.cacheMB > 0 {
		// The page cache is in KB, and a negative number means "that many
		// KB" (a positive one would mean "that many pages").
		q += fmt.Sprintf("&_pragma=cache_size(%d)", -o.cacheMB*1024)
	}
	if o.synchronous != "" {
		q += "&_pragma=synchronous(" + o.synchronous + ")"
	}
	return q
}

```

`internal/store/store.go` —— `Open` 算出来那个数，`openDurable` 用它：

```go
	durable, err := openDurable(path, o, db)
	if err != nil {
		db.Close()
		return nil, err
	}
	// 丢不得的那两笔等锁等多久，是它们走的那条连接上的那个数：平时那条
	// 连接自己就 fsync 的时候（配了 full），没有第二条连接，那两笔走的就是
	// 它。
	wait := busyWaitOf(o)
	if durable != db {
		wait = durableWaitOf(o)
	}
	return &Store{db: db, durable: durable, Clock: time.Now, busyWait: wait}, nil
}
```

```go
// openDurable opens the connection the writes that must not be lost go
// on.
//
// It is the same file, opened with synchronous=full: the commit fsyncs
// the write-ahead log, so the write is on the disk when the commit is
// done. The everyday connection keeps the speed it was given (normal):
// most writes can be lost and recover by themselves.
//
// If the everyday connection fsyncs every write already, it is the
// durable one and there is no second connection.
//
// 丢不得的那两笔（续签后的 token、清掉的「用完了」标记）走这条连接。它们的
// 提交是一次 fsync：几毫秒。它们很少发生 —— 一次续签，和一次签到救活了
// 一个用完的号 —— 所以等得起。
func openDurable(path string, o options, db *sql.DB) (*sql.DB, error) {
	fsyncs, err := fsyncsEveryWrite(db)
	if err != nil {
		return nil, err
	}
	if fsyncs {
		return db, nil
	}
	d := o
	d.synchronous = "full"
	d.busyTimeout = durableWaitOf(o)
	durable, err := openConn(path, d)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	durable.SetMaxOpenConns(durableConns)
	durable.SetMaxIdleConns(durableConns)
	if err := durable.PingContext(context.Background()); err != nil {
		durable.Close()
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	return durable, nil
}

```

- [ ] **Step 5: 改 `internal/store/store.go` —— 那两笔走 `writeBusy`**

```go
func (s *Store) saveToken(ctx context.Context, db execer, t Token) error {
	err := s.writeBusy(ctx, db, tokenInsert,
		t.AccountID, t.Access, t.Refresh, ms(t.ExpiresAt), t.Subject, ms(s.stamp(t.UpdatedAt)))
	if err != nil {
		return fmt.Errorf("save token: %w", err)
	}
	return nil
}

// tokenInsert is the statement that writes a token.
//
// 写进去的是哪一个令牌，由库里那份说了算：saveToken 只在手里那份更新的
// 时候才写（见 internal/manager 的 saveToken）。
const tokenInsert = `
	INSERT INTO tokens (account_id, access, refresh, expires_at, subject, updated_at)
	VALUES (?,?,?,?,?,?)
	ON CONFLICT(account_id) DO UPDATE SET
		access=excluded.access, refresh=excluded.refresh,
		expires_at=excluded.expires_at, subject=excluded.subject,
		updated_at=excluded.updated_at`

```

```go
// ClearExhausted clears an account's window.
//
// 清除的那笔要落盘：这个标记是「这个号的额度用完了」，而签到给的积分能
// 救活它 —— 标记没了它才回到轮转里。断了电把这一笔丢了，它就还被关在
// 外面，最多关到签到的积分过期（七天）。
//
// 它走那条 fsync 的连接：写不进去就是没删掉，写进去了就是在盘上的 ——
// 没有「删掉了但没落盘」这种半个状态，所以只有 DELETE 本身失败才报错。
func (s *Store) ClearExhausted(ctx context.Context, accountID, windowKey string) error {
	err := s.writeBusy(ctx, s.durable,
		"DELETE FROM exhausted_windows WHERE account_id=? AND window_key=?",
		accountID, windowKey)
	if err != nil {
		return fmt.Errorf("clear exhausted: %w", err)
	}
	return nil
}

```

- [ ] **Step 6: 跑测试，看它过了**

```bash
gofmt -l .
go build ./...
go vet ./...
go test ./internal/store/ -count=1
```

**过了** —— 四条都过了。

- [ ] **Step 7: 自查有没有牙**

- 把 `writeBusy` 里的「再来一遍」去掉（`s.busyTries()` 改成 1）→
  `TestTheWriteThatMustNotBeLostIsTriedAgainWhenTheFileIsBusy` 要**红**（写不进
  去）；改回来。

- [ ] **Step 8: 提交**

```bash
git add internal/store/busy.go internal/store/busy_test.go internal/store/store.go
git commit -F .git/commit_msg.txt
```

---

### Task 2: 事务走 fsync 那条连接

**Files:**
- Modify: `internal/store/store.go`
- Modify: `internal/store/store_test.go`

**为什么先看规格 §4 的洞二：** 事务里写的令牌走的是**平时**那条连接
（不 fsync）—— 那一笔也是丢不得的：库里留着的是一个已经被站点用掉一次的
refresh token，下一次续签站点会拒，账号就被记成签入失效。

- [ ] **Step 1: 写失败的测试**

`internal/store/store_test.go` —— 挨着别的「走哪条连接」那几条加一条：

```go
// TestTheTokenWrittenInATransactionIsOnTheDurableConnection says the
// token written inside a transaction is written on the connection that
// fsyncs.
//
// 签入和导入都是走事务写令牌的（internal/store 的 SignIn 和
// internal/provision 的链）。那一笔也是丢不得的：库里留着的是一个已经被
// 站点用掉一次的 refresh token，下一次续签站点会拒，账号就被记成签入失效。
//
// 把那条连接关掉，事务就起不来了 —— 这一条守的是「走哪条连接」。
func TestTheTokenWrittenInATransactionIsOnTheDurableConnection(t *testing.T) {
	ctx := context.Background()
	s := openTestOpts(t, WithSynchronous("normal"))
	makeAccount(t, s, "one")

	if err := s.durable.Close(); err != nil {
		t.Fatal(err)
	}
	err := s.Tx(ctx, func(tx Tx) error {
		return tx.SaveToken(ctx, Token{
			AccountID: "one", Access: "a", Refresh: "r",
			ExpiresAt: time.Now().Add(time.Hour), UpdatedAt: time.Now(),
		})
	})
	if err == nil {
		t.Fatal("the token was written in a transaction without the connection that fsyncs")
	}
}

```

- [ ] **Step 2: 跑测试，看它失败**

```bash
go test ./internal/store/ -run TestTheTokenWrittenInATransaction -v
```

失败：那个事务写进去了（事务走的是平时那条连接，不是 fsync 那条）。

- [ ] **Step 3: 改 `internal/store/store.go` —— 事务走 fsync 那条连接**

```go
// tx runs fn inside one write transaction.
//
// It is what Tx hands out, and what the store's own two-step writes use
// when the two steps have to land together.
//
// 事务走那条会 fsync 的连接：签入和导入都是走事务写令牌的（见 SignIn 和
// internal/provision 的链），那一笔也是丢不得的 —— 库里留着的是一个已经被
// 站点用掉一次的 refresh token，下一次续签站点会拒，账号就被记成签入失效。
// 走那条连接的时候，账号行和令牌行是一起落盘的：读者不会看见一行没有
// 令牌的账号。
func (s *Store) tx(ctx context.Context, fn func(*sqlTx) error) error {
	tx, err := s.begin(ctx)
	if err != nil {
		return err
	}
	if err := fn(&sqlTx{s: s, tx: tx}); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("commit the transaction: %w", err)
	}
	return nil
}

// begin starts a write transaction, and starts it again when the library
// says the file was busy.
//
// 事务里的写是拿不到写锁才起不来的：起来了，锁就是它的了，里面的语句再也
// 不会碰到「忙」（数据源名里写着 _txlock=immediate）。所以要再试的只有开
// 头那一件事 —— 而它没起来的话，里面那个函数一次也没有跑过，再来一遍是
// 安全的。
func (s *Store) begin(ctx context.Context) (*sql.Tx, error) {
	var err error
	for i := 0; i < s.busyTries(); i++ {
		if i > 0 && !sleepCtx(ctx, busyPause) {
			break
		}
		var tx *sql.Tx
		tx, err = s.durable.BeginTx(ctx, nil)
		if err == nil {
			return tx, nil
		}
		if ctx.Err() != nil || !Busy(err) {
			return nil, fmt.Errorf("begin a transaction: %w", err)
		}
	}
	return nil, fmt.Errorf("begin a transaction: %w", err)
}

```

- [ ] **Step 4: 跑测试，看它过了**

```bash
gofmt -l .
go build ./...
go vet ./...
go test ./internal/store/ -count=1
```

**过了** —— Task 1 那四条还在过，那条新写的也过了。

- [ ] **Step 5: 自查有没有牙**

- 把 `begin` 里的 `s.durable` 改回 `s.db` → `TestTheTokenWrittenInATransactionIsOnTheDurableConnection`
  要**红**（事务写进去了）；改回来。

- [ ] **Step 6: 提交**

```bash
git add internal/store/store.go internal/store/store_test.go
git commit -F .git/commit_msg.txt
```

---

### Task 3: 库追上了，记号就没了

**Files:**
- Modify: `internal/manager/manager.go`
- Modify: `internal/manager/manager_test.go`

**为什么先看规格 §4 的洞一：** 记号说的是「库里那份比手里那份旧」。库里那份
追上来了，那句话就不成立了 —— 可是从前只有「写下去了」那条路擦记号，跳过写入
的那条路不擦。

- [ ] **Step 1: 写失败的测试**

`internal/manager/manager_test.go` —— 加一条：

```go
// TestTheTokenNoteGoesAwayWhenTheStoreHasCaughtUp says the note about a
// token that could not be written down goes away when the store catches
// up — even when nothing was written this time.
//
// 那条记号说的是「库里那份比手里那份旧」。库里那份追上了，那句话就不成立
// 了 —— 记号该擦。写下去的那条路从前就擦（saveToken 的最后一行），跳过
// 写入的那条路（库里那份已经不比手里那份旧）从前不擦：记号就一直挂着，一
// 直挂到下一次改状态为止。
func TestTheTokenNoteGoesAwayWhenTheStoreHasCaughtUp(t *testing.T) {
	ctx := context.Background()
	m, _ := newTestManagerWithPath(t, nil)
	now := time.Now()
	m.Store.CreateAccount(ctx, store.Account{
		ID: "acc_01", Name: "a", Status: store.StatusActive, CreatedAt: now, UpdatedAt: now,
	})
	// 库里那份已经是新的了（别的路写进去的）。
	if err := m.Store.SaveToken(ctx, store.Token{
		AccountID: "acc_01", Access: "a", Refresh: "r",
		ExpiresAt: now.Add(time.Hour), UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	// 写不下去的那一次留下的记号。
	m.note(ctx, "acc_01", tokenNotWrittenReason+": the disk is full")
	// 手里那份不比库里那份旧：不写，可是记号该擦。
	if err := m.saveToken(ctx, "acc_01", &oauth.Token{
		Access: "a", Refresh: "r", Expires: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	a, err := m.Store.GetAccount(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if a.LastError != "" {
		t.Errorf("the note is still on the account: %q", a.LastError)
	}
}

```

- [ ] **Step 2: 跑测试，看它失败**

```bash
go test ./internal/manager/ -run TestTheTokenNoteGoesAwayWhenTheStoreHasCaughtUp -v
```

失败：记号还在（`the note is still on the account`）。

- [ ] **Step 3: 改 `internal/manager/manager.go`**

那两条前缀和两个名单：

```go
// The notes that are about a token that could not be written down.
//
// 前一个是现在写的那一条。后一个是旧版本写的那一条 —— 旧的那一条说的
// 是另一句话，可是说的是同一件事（库里那份比手里那份旧），所以擦的时候
// 要认得它。
var tokenNotePrefixes = []string{tokenNotWrittenReason, retiredDurableReason}

// retiredNotePrefixes are the notes that the current version cannot write
// any more.
//
// 旧版本写的那些记号说的是一句今天已经不存在的话，挂着它是骗人的。开机
// 的时候擦一遍（见 ClearRetiredNotes）。
var retiredNotePrefixes = []string{retiredDurableReason}

```

`saveToken` 的那道守卫：

```go
// saveToken writes an account's token to the store.
//
// The token is written only when it is newer than the one the store
// holds. The chain advances on its own, so two renewals can answer
// with two different tokens, and the older one can arrive second: the
// older token's refresh has been spent already — the newer one was
// bought with it — and writing it down would leave the store with a
// refresh token the site refuses, which signs the account out at the
// next renewal.
//
// The expiry says which token is newer: the site hands out access
// tokens that live an hour, so the token of the later renewal ends
// later. Two tokens that end at the same time are the same step of the
// chain, and the one in the store is kept: writing the other one down
// buys nothing and risks a refresh token that is spent already.
//
// 库里存的是毫秒，而续签那一刻拿到的是毫秒以下的精度。所以比的时候**两边
// 都要砍到毫秒**：不砍的话，库里那个 = fromMS(ms(手里那个))，永远比手里
// 那个「旧」那么一点，于是这个守卫永远是「库里那个旧，写吧」—— 每个请求
// 都重写一遍 token 行，每一遍都是一个写事务加一次 fsync，写锁排长队，排满
// busy_timeout 就是 SQLITE_BUSY。
func (m *Manager) saveToken(ctx context.Context, accountID string, tok *oauth.Token) error {
	stored, err := m.Store.GetToken(ctx, accountID)
	if err == nil && !stored.ExpiresAt.Before(store.AtMillisecond(tok.Expires)) {
		// The store already holds a token that is at least as new.
		//
		// 库里那份不比手里那份旧 —— 那条「库里那份比手里那份旧」就不成立
		// 了，记号该擦。这一遍没有写下去（写下去的那条路本来就在擦），
		// 可是记号说的不是「这一遍写了没有」，说的是「库里那份旧不旧」。
		m.clearTokenNote(ctx, accountID)
		return nil
	}
	// A token that could not be read is written over: the store is
	// the only place the token lives, and a renewal that is not
	// written down is a renewal that is lost.
	if err := m.Store.SaveToken(ctx, store.Token{
		AccountID: accountID,
		Access:    tok.Access,
		Refresh:   tok.Refresh,
		ExpiresAt: tok.Expires,
		Subject:   tok.Subject,
		UpdatedAt: time.Now(),
	}); err != nil {
		return err
	}
	// 写下去了，库里那份就追上了手里这份 —— 那条「写不下去」的记号不再
	// 成立，把它擦掉。
	m.clearTokenNote(ctx, accountID)
	return nil
}

```

擦记号的那一段，和认前缀的那个函数：

```go
// clearTokenNote takes away the note about a token that could not be
// written down.
//
// 那条记号说的是「库里那份比手里那份旧」。token 写成功了，库里那份就追上
// 了手里那份 —— 记号不成立了，可它还挂着：last_error 只有在改状态的时候
// 才会被清掉，所以一次暂时的忙会在一个好好的账号上留一条「这个号有问题」，
// 一直留到下一次改状态为止。
//
// 所以 token 写成功之后要把它擦掉。只擦关于令牌的那一条（现在写的那条，
// 和旧版本写的那条）：别的理由（续签失败但手里那份还好之类）说的是别的事，
// 擦了就是把真话也藏起来。
//
// 它是尽力而为的：擦不掉就算了，下一次写成功还会再擦一次。
func (m *Manager) clearTokenNote(ctx context.Context, accountID string) {
	a, err := m.Store.GetAccount(ctx, accountID)
	if err != nil {
		return
	}
	if a.Status == store.StatusSignedOut {
		// The sign-out has its own reason, and that is the one that
		// matters: it says why the site does not take this account
		// any more.
		return
	}
	if !isTokenNote(a.LastError) {
		// Some other reason is on the account, and it is still true.
		return
	}
	_ = m.Store.SetAccountLastError(ctx, accountID, "", time.Now())
}

// isTokenNote says the note is about a token that could not be written down.
//
// 两条前缀都认：现在写的那一条，和旧版本写的那一条 —— 旧的那一条说的是
// 另一句话，可是说的是同一件事，擦它的时候得认得它。
func isTokenNote(note string) bool {
	for _, p := range tokenNotePrefixes {
		if strings.HasPrefix(note, p) {
			return true
		}
	}
	return false
}

```

- [ ] **Step 4: 跑测试，看它过了**

```bash
go test ./internal/manager/ -count=1
```

- [ ] **Step 5: 提交**

```bash
git add internal/manager/manager.go internal/manager/manager_test.go
git commit -F .git/commit_msg.txt
```

---

### Task 4: 旧版本写的记号，开机时擦掉

**Files:**
- Modify: `internal/store/store.go`
- Create: `internal/store/notes_test.go`
- Modify: `internal/manager/manager.go`
- Modify: `internal/manager/manager_test.go`
- Modify: `cmd/mmxcode/root.go`

**为什么：** 旧版本写的那条记号说的是一句今天已经不存在的话，挂着它是骗人的。
而且从前**没有任何一条代码路径**能把它擦掉（擦的那段只认现在写的那一条前
缀）—— 库里 46 个账号就这么挂着。

- [ ] **Step 1: 写失败的测试**

`internal/store/notes_test.go`（新文件，`package store`；import：`context`、
`testing`、`time`）：

```go
package store

import (
	"context"
	"testing"
	"time"
)

// TestTheNotesWithAPrefixAreCleared says the notes that start with a
// prefix are cleared, and the others are left alone.
//
// 旧版本写在账号上的记号说的是一句今天已经不存在的话，挂着它是骗人的
// （页面上显示着「这个号有问题」，而那个号其实好得很）。擦的时候只擦自
// 己认得的那一条前缀：别的理由说的是别的事，擦了就是把真话也藏起来。
func TestTheNotesWithAPrefixAreCleared(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	now := time.Now()
	for _, id := range []string{"one", "two", "three"} {
		makeAccount(t, s, id)
	}
	if err := s.SetAccountLastError(ctx, "one",
		"the token was written down but it was not made durable: a power cut could lose it", now); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAccountLastError(ctx, "two",
		"the token was written down but it was not made durable: flush the database: it is busy", now); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAccountLastError(ctx, "three",
		"the renewal failed but the token in hand is still good", now); err != nil {
		t.Fatal(err)
	}

	n, err := s.ClearAccountNotes(ctx, "the token was written down but it was not made durable")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("the number of notes cleared = %d, want 2", n)
	}

	for _, id := range []string{"one", "two"} {
		a, err := s.GetAccount(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if a.LastError != "" {
			t.Errorf("the note on %s is still there: %q", id, a.LastError)
		}
	}
	other, err := s.GetAccount(ctx, "three")
	if err != nil {
		t.Fatal(err)
	}
	if other.LastError == "" {
		t.Error("the note of another reason was cleared")
	}
}

// TestTheNotesWithAPrefixAreClearedOnlyOnce says the second time there is
// nothing to clear.
//
// 擦过一遍就干净了：开机的时候擦一遍就够，擦第二遍是白擦（也是白拿一
// 把写锁）。
func TestTheNotesWithAPrefixAreClearedOnlyOnce(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	now := time.Now()
	makeAccount(t, s, "one")
	if err := s.SetAccountLastError(ctx, "one",
		"the token was written down but it was not made durable: a power cut could lose it", now); err != nil {
		t.Fatal(err)
	}
	n, err := s.ClearAccountNotes(ctx, "the token was written down but it was not made durable")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("the number of notes cleared = %d, want 1", n)
	}
	n, err = s.ClearAccountNotes(ctx, "the token was written down but it was not made durable")
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("the number of notes cleared the second time = %d, want 0", n)
	}
}
```

`internal/manager/manager_test.go` —— 再加一条：

```go
// TestTheNoteOfAnOlderVersionIsCleared says the note an older version
// wrote is cleared, and the note of another reason is left alone.
//
// 旧版本写的记号说的是一句今天已经不存在的话（那一版有一个 Flush，去把预
// 写日志搬回主库，搬不动的时候就把那句话记在账号上）。现在那两笔丢不得的
// 写走的是会 fsync 的连接 —— 落盘了 —— 所以那句话已经不成立了，挂着它是
// 骗人的。
//
// 别的理由（续签失败但手里那份还好）说的是别的事，那件事还是真的：擦了就
// 是把真话也藏起来。
func TestTheNoteOfAnOlderVersionIsCleared(t *testing.T) {
	ctx := context.Background()
	m, _ := newTestManagerWithPath(t, nil)
	now := time.Now()
	for _, id := range []string{"acc_01", "acc_02"} {
		m.Store.CreateAccount(ctx, store.Account{
			ID: id, Name: id, Status: store.StatusActive, CreatedAt: now, UpdatedAt: now,
		})
	}
	m.note(ctx, "acc_01", retiredDurableReason+": flush the database: it is busy")
	m.note(ctx, "acc_02", renewalFailedReason+": the site is having a bad moment")

	if err := m.ClearRetiredNotes(ctx); err != nil {
		t.Fatal(err)
	}

	old, err := m.Store.GetAccount(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if old.LastError != "" {
		t.Errorf("the note of the older version is still on the account: %q", old.LastError)
	}
	other, err := m.Store.GetAccount(ctx, "acc_02")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(other.LastError, renewalFailedReason) {
		t.Errorf("the note on the account = %q, want the one that was there", other.LastError)
	}
}

```

- [ ] **Step 2: 跑测试，看它失败**

```bash
go test ./internal/store/ -run TestTheNotesWithAPrefix -v
go test ./internal/manager/ -run TestTheNoteOfAnOlderVersionIsCleared -v
```

失败：编译不过（`ClearAccountNotes`、`ClearRetiredNotes` 都还没有）。

- [ ] **Step 3: store 给一个原语**

`internal/store/store.go` —— `SetAccountLastError` 后面加一条：

```go
// ClearAccountNotes takes away the notes that start with prefix, and says
// how many it took away.
//
// 它是给「旧版本写的记号」用的：那些记号说的是一句今天已经不存在的话，挂
// 着它是骗人的 —— 页面上显示着「这个号有问题」，而那个号其实好得很。
//
// 只擦前缀一样的：别的理由说的是别的事，擦了就是把真话也藏起来。
//
// 前缀是拿 substr 比的，不是 LIKE —— LIKE 里的 "%" 和 "_" 是通配符，一
// 个记号的前缀里要是有它们，擦掉的就是别的记号。
func (s *Store) ClearAccountNotes(ctx context.Context, prefix string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		"UPDATE accounts SET last_error='', updated_at=? WHERE substr(last_error,1,?)=?",
		ms(s.now()), utf8.RuneCountInString(prefix), prefix)
	if err != nil {
		return 0, fmt.Errorf("clear the notes: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

```

（`unicode/utf8` 要 import。）

- [ ] **Step 4: manager 擦**

`internal/manager/manager.go` —— `isTokenNote` 后面加一条：

```go
// ClearRetiredNotes takes away the notes that an older version wrote.
//
// 旧版本写的那些记号说的是一句今天已经不存在的话 —— 那一版有一个 Flush，
// 去把预写日志搬回主库，搬不动的时候就把那句话记在账号上。现在那两笔丢
// 不得的写走的是会 fsync 的连接（落盘了），所以那句话已经不成立了，挂着
// 它是骗人的：页面上显示着「这个号有问题」，而那个号其实好得很。
//
// 它是开机时做一次的事。擦的是自己认得的那一条前缀：别的理由说的是别的
// 事，擦了就是把真话也藏起来。
func (m *Manager) ClearRetiredNotes(ctx context.Context) error {
	for _, p := range retiredNotePrefixes {
		if _, err := m.Store.ClearAccountNotes(ctx, p); err != nil {
			return err
		}
	}
	return nil
}

```

- [ ] **Step 5: 开机的时候擦一次**

`cmd/mmxcode/root.go` —— 建好 manager 之后：

```go
		e.mgr = manager.New(s, e.up, e.acc, e.egr, oauth.NewRenewer())
		// 旧版本写在账号上的记号擦掉：那句话说的事今天已经不存在了，挂
		// 着它是骗人的（页面上显示着「这个号有问题」，而那个号其实得好
		// 得很）。擦不掉不是说一声就好 —— 网关照样起：那是一次收拾，不
		// 是网关的命。
		if err := e.mgr.ClearRetiredNotes(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "warning: the notes of an older version could not be cleared: %v\n", err)
		}
```

- [ ] **Step 6: 跑测试，看它过了**

```bash
gofmt -l .
go build ./...
go vet ./...
go test ./... -count=1
```

- [ ] **Step 7: 拿真库的一份拷贝试一遍**

**不动真的那个库** —— 拷贝一份，命令行跑一遍，看那 46 条记号没了：

```bash
cp mmxcode.db /tmp/mmxcode-copy.db
go run ./cmd/mmxcode accounts list --db /tmp/mmxcode-copy.db
```

（拷的是拷贝，跑完记号就没了；真的那个库要等换了这个版本启动的时候才擦。）

- [ ] **Step 8: 提交**

```bash
git add internal/store/store.go internal/store/notes_test.go internal/manager/manager.go internal/manager/manager_test.go cmd/mmxcode/root.go
git commit -F .git/commit_msg.txt
```

---

### Task 5: 导入那条路也按结果码认忙

**Files:**
- Modify: `internal/provision/chain.go`
- Modify: `internal/provision/chain_test.go`

**为什么：** 那是整个仓库里唯一认忙的地方，而它是靠字眼认的 —— 别的毛病也会
被认成忙（"the server is busy"），真的忙反倒可能不被认出来。

- [ ] **Step 1: 改 `internal/provision/chain.go`**

```go
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

```

那个靠字眼认的 `busyLibrary` 整个删掉。

- [ ] **Step 2: 改 `internal/provision/chain_test.go` —— 假库说的那句话**

假库说「库忙」的时候说的那句话，要照着驱动说的写（驱动说的就是
`database is locked (5) (SQLITE_BUSY)`）：

```go
	// 库忙：真库上两个写碰上了就是这样（SQLITE_BUSY）。这一趟没写进去
	// 任何东西，等一下再来一趟就行。
	//
	// 那句话是照着驱动说的写的：库说忙，是它的结果码（store.Busy 认的
	// 就是那个），不是句子里有没有「locked」两个字。
	if s.lockFailures > 0 {
		s.lockFailures--
		return errors.New("database is locked (5) (SQLITE_BUSY)")
	}
```

- [ ] **Step 3: 跑测试**

```bash
go test ./internal/provision/ -count=1
```

- [ ] **Step 4: 提交**

```bash
git add internal/provision/chain.go internal/provision/chain_test.go
git commit -F .git/commit_msg.txt
```

---

### Task 6: 全查一遍

- [ ] **Step 1: 全部的测试**

```bash
gofmt -l .
go build ./...
go vet ./...
go test ./... -count=1
```

- [ ] **Step 2: 自查有没有牙**

- 把 `begin` 里的 `s.durable` 改回 `s.db` → 事务那条测试要**红**；改回来。
- 把 `writeBusy` 的遍数改成 1 → 「再来一遍」那条测试要**红**；改回来。
- 把 `clearTokenNote` 认的前缀列表改回只有一条 → 「旧版本写的那一条」那条
  测试要**红**；改回来。
- 把 `saveToken` 那道守卫里的 `m.clearTokenNote` 去掉 → 「库追上了」那条测试
  要**红**；改回来。

- [ ] **Step 3: 提交**

```bash
git add -A
git commit -F .git/commit_msg.txt
```

## 前后两遍

| | 改之前 | 改之后 |
|---|---|---|
| 旧版本写的那条记号 | **永远擦不掉**（46 个账号挂着） | 开机时擦掉（拿真库的拷贝验过：46 → 0） |
| 库里那份追上了 | 记号还挂着 | 记号擦掉 |
| 事务里写的令牌 | **不落盘**（走的是不 fsync 的那条连接） | 落盘（走 fsync 那条连接） |
| 库说忙的时候 | 等 5 秒，然后失败 | 再试几遍，一共还是那个 5 秒的顶 |
| 两个写一起被堵住 | 5 秒和 **7.8 秒**（排队） | 一起在 5 秒左右结束 |
| 认「忙」 | 靠字眼（`internal/provision`） | 靠结果码（`SQLITE_BUSY`/`SQLITE_LOCKED`） |
