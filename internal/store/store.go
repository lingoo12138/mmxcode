// Package store keeps the accounts, their tokens, the check-in history
// and the usage snapshots in one SQLite file.
//
// The file is opened with WAL on and with more than one connection, so
// a reader (the WebUI) and a writer (the scheduler) do not block each
// other. The pragmas are in the data source name, and a pragma is per
// connection, so every connection of the pool has them.
//
// The file holds the accounts' refresh tokens in clear, so the
// directory it is in is made for its owner only, and the file in it
// is made for its owner only as well.
//
// The sessions table grows by one row for every session key that is
// seen. Nothing prunes it: the rows are small, but they are not
// nothing.
package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	_ "modernc.org/sqlite"
)

// The account states.
const (
	StatusActive    = "active"
	StatusSignedOut = "signed_out"
	StatusError     = "error"
)

// ErrNotFound says the row is not there.
//
// It is the one way of saying so: a caller asks with errors.Is and
// never has to know that the database has a sentinel of its own.
var ErrNotFound = errors.New("store: not found")

// maxConns is how many connections the pool has.
//
// One connection would mean one statement at a time in the whole
// process, and the reader and the writer would take turns. A few
// connections let them work at the same time, which is what the WAL
// journal is for.
const maxConns = 4

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

// Store is the database.
type Store struct {
	db *sql.DB

	// durable is the connection the writes that must not be lost go on.
	//
	// It is the same file as db, opened with synchronous=full: the
	// commit fsyncs the write-ahead log, so the write is on the disk
	// when the commit is done. It is db itself when db already fsyncs
	// every commit, and then there is no second connection.
	//
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

	// Clock is the store's clock.
	//
	// It is there so that a test can say what time it is. A store
	// with no clock uses the wall clock.
	Clock func() time.Time

	// apiKeys is the answer to "has anybody asked for a key yet".
	//
	// 它是「要不要鉴权」：缓一秒就有一秒的窗口 —— 那一秒里后台申请了一把
	// key，不带 key 的请求被放行。所以它不能只靠 TTL 过期：谁改了谁就通
	// 知（见 forgetAPIKeys）。
	apiKeys atomic.Int32
	// apiKeysMu guards the moment when the answer is put back.
	//
	// 查库和把答案写回去要锁在一起：不然一个正在查库的请求会在别人申请了
	// key 之后，把「一把都没有」这个旧答案写回去 —— 那个旧答案就留在缓存
	// 里不动了，门就一直开着（见 forgetAPIKeys）。
	apiKeysMu sync.Mutex
}

// now gives the store's time.
func (s *Store) now() time.Time {
	if s.Clock != nil {
		return s.Clock()
	}
	return time.Now()
}

// stamp gives the time a row is written at.
//
// A caller that does not say when it is writing gets the store's
// clock. A row with no time in it says 1970, and 1970 is not a time
// anybody asked for.
func (s *Store) stamp(t time.Time) time.Time {
	if t.IsZero() {
		return s.now()
	}
	return t
}

// Account is one MiniMax account.
type Account struct {
	ID       string
	Name     string
	UserID   string
	Email    string
	Status   string
	ProxyURL string
	// ProxyGroup is the outlet group the account is pointed at.
	//
	// 组名是空的：这个账号没有指着一个组，它跟着全局组走（或者自己写了
	// 地址，或者直连）。
	ProxyGroup string
	LastError  string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Token is an account's sign-in.
type Token struct {
	AccountID string
	Access    string
	Refresh   string
	ExpiresAt time.Time
	Subject   string
	UpdatedAt time.Time
}

// Checkin is one day's check-in for one account.
type Checkin struct {
	ID        int64
	AccountID string
	Day       string
	Outcome   string
	Credit    float64
	Streak    int
	Message   string
	CreatedAt time.Time
}

// UsageSnapshot is one account's credits and plan at a moment.
type UsageSnapshot struct {
	ID         int64
	AccountID  string
	Granted    float64
	Consumed   float64
	Remaining  float64
	HasAmounts bool
	ExpiresAt  time.Time
	Plan       string
	PlanUntil  time.Time
	// WindowsJSON is the plan's windows as they were read.
	WindowsJSON string
	Balance     string
	// MembershipError says the membership could not be read.
	//
	// It is kept, because a plan that could not be read is not a plan
	// without a group: the windows are asked for under that group.
	MembershipError string
	// WindowsError says the plan's windows could not be read.
	//
	// It is kept, because a plan whose windows could not be read is
	// not a plan without windows.
	WindowsError string
	// PlanError says the plan's title could not be read.
	//
	// It is kept, because a plan whose title could not be read is not
	// a plan without a title: the field below would say "" and read
	// as "this account has no plan".
	PlanError string
	CreatedAt time.Time
}

// Exhausted is one account's spent window.
type Exhausted struct {
	AccountID string
	WindowKey string
	Until     time.Time
	Reason    string
}

// EgressHealth is one proxy's last check.
type EgressHealth struct {
	URL       string
	OK        bool
	Error     string
	CheckedAt time.Time
}

// ImportRun is one batch import and what came of it.
type ImportRun struct {
	ID         string
	Source     string
	StartedAt  time.Time
	FinishedAt time.Time
	Total      int
	OK         int
	Failed     int
	Results    string
}

// Option is one thing about how the database is opened.
type Option func(*options)

// options is what the options add up to.
type options struct {
	// cacheMB is the page cache of one connection, in MB. Zero leaves it
	// to SQLite.
	cacheMB int
	// synchronous is how hard a write hits the disk. Empty leaves it to
	// SQLite.
	synchronous string
	// busyTimeout is how long one write waits for the file's write lock.
	// Zero leaves it at defaultWait.
	busyTimeout time.Duration
	// connector opens the connections. Nil means SQLite's own.
	connector Connector
}

// Connector opens the connections of the database.
//
// 平时是 SQLite 自己的（见 openConn）。量语句条数的时候是套了一层计数的
// （见 counting.go）。
type Connector func(dsn string) (driver.Connector, error)

// WithCacheMB says how many MB of pages one connection may keep.
//
// 页缓存是「读过的页先留在手边」：工作集装得下的时候，同一批查询不用一
// 遍一遍地去磁盘要页。1000 个号、一年的签到：填池那条查询从 11.7 ms 到
// 5.9 ms。
//
// 0 是 SQLite 自己的默认（2 MB）。缓存是用多少占多少：库小的时候它占不
// 到那么多。
func WithCacheMB(n int) Option {
	return func(o *options) { o.cacheMB = n }
}

// WithSynchronous says how hard a write hits the disk.
//
// "normal"：写下去就交给操作系统，断电的时候最近这几笔可能丢（库不会
// 坏 —— SQLite 对 WAL 的推荐就是这个）。"full"：每一笔都 fsync，慢一点
// （同一笔写：normal 64µs 对 full 874µs —— full 那一笔要等盘）。
//
// 它管的是**常用的那些写**。丢不起的那两笔（续签后的 token、清掉的「用完
// 了」标记）走另一条连接，那条永远是 full：断了电它们不会丢。配了 "normal"
// 也是那样 —— full 不是全局的，只有那两笔等得起盘。
//
// 空是 SQLite 自己的默认（"full"）。
func WithSynchronous(s string) Option {
	return func(o *options) { o.synchronous = s }
}

// WithConnector gives the store the connections to use.
//
// 那是量具用的：套一层计数的连接，就知道一个请求打了几条语句。生产的那个
// 连接不用它 —— 那时候还是 SQLite 自己的。
func WithConnector(c Connector) Option {
	return func(o *options) { o.connector = c }
}

// Open opens the database at path, creating it and its directory if
// they are not there.
//
// The directory and the file are made for their owner only: the file
// holds the refresh tokens of every account in clear. Windows has no
// such a mode, and there the file is left as Windows made it.
//
// A directory that is already there is left as it is: the store does
// not tighten the permissions of a directory the user picked. The file
// itself is always tightened, whoever made it.
//
// The file is opened twice when the everyday writes do not fsync: once
// for them, and once with synchronous=full for the writes that must
// not be lost. See the durable field.
func Open(path string, opts ...Option) (*Store, error) {
	o := options{}
	for _, opt := range opts {
		opt(&o)
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create data dir: %w", err)
		}
	}
	db, err := openConn(path, o)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(maxConns)
	db.SetMaxIdleConns(maxConns)
	// The database is opened lazily, so a path that cannot be used
	// would only show up at the first query. Open it now: the caller
	// wants to know now.
	if err := db.PingContext(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if err := protectFile(path); err != nil {
		db.Close()
		return nil, err
	}
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

// openConn opens the connections of the database.
//
// 没有量具的时候是 SQLite 自己的连接 —— 那条路一点没变。有量具的时候是量具
// 给的那条（见 counting.go）。
//
// 出错的时候不包一层「open sqlite」：包了的是 Open 和 openDurable，它们是
// 唯一会把这句话说出口的地方 —— 包两遍的话那句话就重复了。
func openConn(path string, o options) (*sql.DB, error) {
	if o.connector == nil {
		return sql.Open("sqlite", dsn(path, o))
	}
	conn, err := o.connector(dsn(path, o))
	if err != nil {
		return nil, err
	}
	return sql.OpenDB(conn), nil
}

// fsyncsEveryWrite says whether the connection fsyncs every commit.
//
// The pragma is asked of the database, not of the options: SQLite's own
// default (no synchronous in the data source name) is full, and a build
// that says otherwise is the build's business, not ours.
func fsyncsEveryWrite(db *sql.DB) (bool, error) {
	var sync int
	if err := db.QueryRowContext(context.Background(), "PRAGMA synchronous").Scan(&sync); err != nil {
		return false, fmt.Errorf("read the synchronous setting: %w", err)
	}
	// 2 is full, 3 is extra: both of them fsync every commit.
	return sync >= 2, nil
}

// Close closes the database.
func (s *Store) Close() error {
	err := s.db.Close()
	if s.durable != nil && s.durable != s.db {
		if e := s.durable.Close(); err == nil {
			err = e
		}
	}
	return err
}

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

// escapePath makes a path safe to put in a file: URI.
//
// Every character that is not an unreserved one is escaped, so that a
// path with a space, a question mark, an ampersand, a hash or a
// percent sign in it is the path and not the URI's syntax. The
// slashes are left: they separate the parts of the path.
func escapePath(p string) string {
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		c := p[i]
		if c == '/' || unreserved(c) {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteString(strings.ToUpper(hex.EncodeToString([]byte{c})))
	}
	return b.String()
}

// unreserved says whether a character may stand for itself in a URI.
//
// It is the set of RFC 3986: the letters, the digits, and the four
// marks that have no meaning of their own.
func unreserved(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	case c == '-', c == '_', c == '.', c == '~':
		return true
	}
	return false
}

// protectFile makes the database file readable by its owner only.
//
// The file holds the refresh tokens of every account in clear, so any
// other user on the machine could take the accounts over. Windows has
// no such a mode, and there the file is left as Windows made it.
func protectFile(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	if err := os.Chmod(path, 0o600); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("protect the database file: %w", err)
	}
	return nil
}

// Migrate brings the database to the shape the code expects.
//
// The steps are numbered, and the file says which one it is at with
// PRAGMA user_version. A file that was made before the steps were
// numbered says 0, and the steps below bring it forward. A fresh file
// and an old one end up at the same version, with the same tables.
//
// Every step is one transaction: a step that fails leaves the file as
// it was, and the version is written in that same transaction, so a
// file never says it is at a version it did not reach.
func (s *Store) Migrate(ctx context.Context) error {
	// One connection for the whole migration: the pragmas are per
	// connection, and the steps must all see the same database.
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	defer conn.Close()

	// A table is rebuilt by making a new one and copying the rows
	// over, and the old table's rows are still there while the copy
	// runs. The foreign keys would say that those rows belong to an
	// account that is not there, so they are turned off for the
	// steps and turned back on after.
	//
	// The pragmas are put back with a context that is not the caller's:
	// a caller's context that was cancelled would leave the
	// connection without its foreign keys.
	clean := context.WithoutCancel(ctx)
	if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys=off"); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	defer conn.ExecContext(clean, "PRAGMA foreign_keys=on")

	// The old table is renamed out of the way before the new one
	// takes its name, and a rename writes the new name into the
	// tables that point at it. The old way of renaming is asked for
	// here, so that the other tables' names are left alone.
	if _, err := conn.ExecContext(ctx, "PRAGMA legacy_alter_table=on"); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	defer conn.ExecContext(clean, "PRAGMA legacy_alter_table=off")

	version, err := userVersion(ctx, conn)
	if err != nil {
		return err
	}
	if version > schemaVersion {
		return fmt.Errorf("migrate: the database is at version %d and this program knows %d",
			version, schemaVersion)
	}
	for _, step := range migrations[version:] {
		if err := step(ctx, conn); err != nil {
			return err
		}
	}
	return nil
}

// schemaVersion is the version a file is at when the steps are done.
const schemaVersion = 9

// migrations are the steps, in order.
//
// The step at index n brings a file at version n to version n+1.
var migrations = []func(ctx context.Context, conn *sql.Conn) error{
	migrateMake,
	migrateConform,
	migratePlanError,
	migrateBatchImport,
	migrateAPIKeys,
	migrateCheckinIndex,
	migrateIndexes,
	migrateProxyGroups,
	migrateUsageDaily,
}

// migrateMake makes the tables that are not there.
//
// A file that was made by an older version has most of them already,
// and they are left as they are: the next step says what shape they
// should be.
func migrateMake(ctx context.Context, conn *sql.Conn) error {
	return runStep(ctx, conn, 1, func(tx *sql.Tx) error {
		for _, t := range tables {
			if _, err := tx.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS "+t.name+t.body); err != nil {
				return fmt.Errorf("migrate: make the table %s: %w", t.name, err)
			}
		}
		return nil
	})
}

// migrateConform gives the tables the shape the code wants.
//
// A table that is not the one the code has is made again: the rows
// are copied into a new table and the old one is taken away. A file
// that was made by an older version is brought to this shape here.
func migrateConform(ctx context.Context, conn *sql.Conn) error {
	return runStep(ctx, conn, 2, func(tx *sql.Tx) error {
		for _, t := range tables {
			got, err := storedSQL(ctx, tx, t.name)
			if err != nil {
				return err
			}
			if got == "" {
				// The table is not there at all.
				if _, err := tx.ExecContext(ctx, "CREATE TABLE "+t.name+t.body); err != nil {
					return fmt.Errorf("migrate: make the table %s: %w", t.name, err)
				}
				continue
			}
			if sameSQL(got, "CREATE TABLE "+t.name+t.body) {
				continue
			}
			if err := rebuildTable(ctx, tx, t); err != nil {
				return err
			}
		}
		for _, q := range indexStatements {
			if _, err := tx.ExecContext(ctx, q); err != nil {
				return fmt.Errorf("migrate: %w", err)
			}
		}
		// A row whose account is not there is not allowed any more,
		// so the ones that are there are taken away.
		for _, c := range childTables {
			q := fmt.Sprintf("DELETE FROM %s WHERE %s NOT IN (SELECT id FROM accounts)",
				c.name, c.account)
			if _, err := tx.ExecContext(ctx, q); err != nil {
				return fmt.Errorf("migrate: take away the rows of %s without an account: %w",
					c.name, err)
			}
		}
		return nil
	})
}

// migratePlanError gives the usage readings the plan error column.
//
// The column was added after the table was first made, so a file that
// was made before it does not have it. The table is rebuilt when the
// column is not there, and left alone when it is: a file that already
// has it must not have its readings copied about for nothing.
func migratePlanError(ctx context.Context, conn *sql.Conn) error {
	return runStep(ctx, conn, 3, func(tx *sql.Tx) error {
		cols, err := columnsOf(ctx, tx, "usage_snapshots")
		if err != nil {
			return err
		}
		if _, ok := cols["plan_error"]; ok {
			return nil
		}
		for _, t := range tables {
			if t.name == "usage_snapshots" {
				return rebuildTable(ctx, tx, t)
			}
		}
		return nil
	})
}

// migrateBatchImport gives the accounts their password column and the
// file its table of batch imports.
//
// Both were added after the file's shape was settled, so a file that
// was made before them does not have them. The accounts table is
// rebuilt when the column is not there, and the table of imports is
// made when it is not there.
//
// The accounts that were already there keep their rows, and their
// password is left empty: an account that was not brought in by a
// batch import has no password, and saying so with an empty string
// would make it look like there is a password that is not there.
func migrateBatchImport(ctx context.Context, conn *sql.Conn) error {
	return runStep(ctx, conn, 4, func(tx *sql.Tx) error {
		cols, err := columnsOf(ctx, tx, "accounts")
		if err != nil {
			return err
		}
		if _, ok := cols["password"]; !ok {
			for _, t := range tables {
				if t.name == "accounts" {
					if err := rebuildTable(ctx, tx, t); err != nil {
						return err
					}
					break
				}
			}
		}
		for _, t := range tables {
			if t.name != "import_runs" {
				continue
			}
			got, err := storedSQL(ctx, tx, t.name)
			if err != nil {
				return err
			}
			if got != "" {
				continue
			}
			if _, err := tx.ExecContext(ctx, "CREATE TABLE "+t.name+t.body); err != nil {
				return fmt.Errorf("migrate: make the table %s: %w", t.name, err)
			}
		}
		return nil
	})
}

// migrateAPIKeys gives the file its table of api keys.
//
// 这一张表是后来才有的：一个文件在它之前做出来的时候没有它。表不在就
// 造出来，在就不动它——已经有了的 key 不该为了一个迁移被抄一遍。
func migrateAPIKeys(ctx context.Context, conn *sql.Conn) error {
	return runStep(ctx, conn, 5, func(tx *sql.Tx) error {
		for _, t := range tables {
			if t.name != "api_keys" {
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

// migrateCheckinIndex gives the check-ins an index on the day.
//
// 「今天谁签到了」是签到那一趟的名单，也是签到的积分进池的那条路（规格
// §5.3）。一天一个号一行，1000 个号一年就是 36 万行 —— 没有索引就是全表
// 扫，而那一趟是补号那一趟（每 15 分钟一趟）都在问的。
//
// 这一步是单独的：索引那几条在 migrateConform 里，而老库停在版本 5 ——
// 走不到那一步（那一步只把版本 1 的库带到版本 2）。
//
// 建的是新那一条（盖住日子、号、时刻、积分）：旧的 idx_checkins_day 只盖
// 住日子一列，每一行都要回表（规格 §5.3 的那趟就是 1000 次回表）。旧的
// 那一条由版本 7 的那一步删掉 —— 走不到这一步的老库停在版本 6，那一条还
// 在它身上。
func migrateCheckinIndex(ctx context.Context, conn *sql.Conn) error {
	return runStep(ctx, conn, 6, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "CREATE INDEX IF NOT EXISTS idx_checkins_day_account ON checkins(day, account_id, created_at, credit)"); err != nil {
			return fmt.Errorf("migrate: the index on the check-in day: %w", err)
		}
		return nil
	})
}

// migrateIndexes gives the tables the indexes the queries ask for.
//
// 它们是第 7 步建的：老库停在版本 6，走不到 migrateConform 里那几条索引
// （那一步只把版本 1 的库带到版本 2）。
//
// 每建一条索引都要重写一遍表，所以这一步在一个事务里：失败就回滚，老
// 库原样不动。
func migrateIndexes(ctx context.Context, conn *sql.Conn) error {
	return runStep(ctx, conn, 7, func(tx *sql.Tx) error {
		for _, q := range indexStatements {
			if _, err := tx.ExecContext(ctx, q); err != nil {
				return fmt.Errorf("migrate: %w", err)
			}
		}
		return nil
	})
}

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

// migrateUsageDaily gives the file its table of daily consumption.
//
// 这一张表是后来才有的：一个文件在它之前做出来的时候没有它。表不在就造
// 出来，在就不动它 —— 已经有了的账不该为了一个迁移被抄一遍。索引也是一
// 样的：这一张表是照着「按日子问」建的。
func migrateUsageDaily(ctx context.Context, conn *sql.Conn) error {
	return runStep(ctx, conn, 9, func(tx *sql.Tx) error {
		for _, t := range tables {
			if t.name != "usage_daily" {
				continue
			}
			got, err := storedSQL(ctx, tx, t.name)
			if err != nil {
				return err
			}
			if got == "" {
				if _, err := tx.ExecContext(ctx, "CREATE TABLE "+t.name+t.body); err != nil {
					return fmt.Errorf("migrate: make the table %s: %w", t.name, err)
				}
			}
		}
		if _, err := tx.ExecContext(ctx, "CREATE INDEX IF NOT EXISTS idx_usage_daily_day ON usage_daily(day)"); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
		return nil
	})
}

// runStep does one step, in one transaction.
//
// The version is written at the end, in the same transaction, so that
// a file never says it is at a version it did not reach.
func runStep(ctx context.Context, conn *sql.Conn, version int, fn func(tx *sql.Tx) error) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		tx.Rollback()
		return fmt.Errorf("migrate: write the version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

// rebuildTable makes the table again, with the rows of the old one.
//
// The rows are copied into a new table of the wanted shape, and the
// old table is taken away. A column the old table does not have gets
// its default, and a column that could be NULL and cannot any more
// gets the default instead of the NULL.
func rebuildTable(ctx context.Context, tx *sql.Tx, t table) error {
	exprs, err := copyExprs(ctx, tx, t)
	if err != nil {
		return err
	}
	cols := make([]string, 0, len(t.copy))
	for _, c := range t.copy {
		cols = append(cols, c.name)
	}
	old := t.name + "_old"
	for _, q := range []string{
		"CREATE TABLE " + t.name + "_new" + t.body,
		"INSERT INTO " + t.name + "_new (" + strings.Join(cols, ", ") + ") SELECT " +
			strings.Join(exprs, ", ") + " FROM " + t.name,
		"ALTER TABLE " + t.name + " RENAME TO " + old,
		"ALTER TABLE " + t.name + "_new RENAME TO " + t.name,
		"DROP TABLE " + old,
	} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("migrate: rebuild the table %s: %w", t.name, err)
		}
	}
	return nil
}

// copyExprs gives the expressions the rows of the old table are read
// with.
func copyExprs(ctx context.Context, tx *sql.Tx, t table) ([]string, error) {
	old, err := columnsOf(ctx, tx, t.name)
	if err != nil {
		return nil, err
	}
	exprs := make([]string, 0, len(t.copy))
	for _, c := range t.copy {
		prev, ok := old[c.name]
		switch {
		case !ok:
			// The old table does not have the column.
			exprs = append(exprs, c.def)
		case !c.notNull:
			// The new column may be NULL, so a NULL is kept: a
			// reading that could not be made is not a reading of
			// zero.
			exprs = append(exprs, c.name)
		case prev.notNull:
			exprs = append(exprs, c.name)
		default:
			exprs = append(exprs, fmt.Sprintf("COALESCE(%s, %s)", c.name, c.def))
		}
	}
	return exprs, nil
}

// columnInfo says what is known about one column.
type columnInfo struct {
	// notNull says the column cannot be NULL.
	notNull bool
}

// querier is what can ask the database a question.
//
// The migration asks inside a transaction and the tests ask outside of
// one, so both have to do.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// columnsOf gives the columns of a table.
func columnsOf(ctx context.Context, q querier, name string) (map[string]columnInfo, error) {
	rows, err := q.QueryContext(ctx, "PRAGMA table_info("+name+")")
	if err != nil {
		return nil, fmt.Errorf("migrate: read the columns of %s: %w", name, err)
	}
	defer rows.Close()
	out := map[string]columnInfo{}
	for rows.Next() {
		var (
			cid     int
			colName string
			colType string
			notNull int
			dflt    sql.NullString
			pk      int
		)
		if err := rows.Scan(&cid, &colName, &colType, &notNull, &dflt, &pk); err != nil {
			return nil, fmt.Errorf("migrate: read the columns of %s: %w", name, err)
		}
		out[colName] = columnInfo{notNull: notNull != 0}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("migrate: read the columns of %s: %w", name, err)
	}
	return out, nil
}

// storedSQL gives the statement the table in the file was made with.
//
// It is empty when the table is not there.
func storedSQL(ctx context.Context, q querier, name string) (string, error) {
	var sqlText string
	err := q.QueryRowContext(ctx,
		"SELECT sql FROM sqlite_master WHERE type='table' AND name=?", name).Scan(&sqlText)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("migrate: read the table %s: %w", name, err)
	}
	return sqlText, nil
}

// sameSQL says whether two statements are the same table.
//
// The statement in the file is not the one the code has: SQLite
// writes the table's name in quotes when the table is renamed, and
// the spacing is whatever the file was made with. So the two are
// compared with the whitespace put to one space and the quotes taken
// away.
func sameSQL(got, want string) bool {
	return normalizeSQL(got) == normalizeSQL(want)
}

// normalizeSQL puts a statement into the one shape two statements are
// compared in.
func normalizeSQL(s string) string {
	s = strings.ReplaceAll(s, `"`, "")
	return strings.Join(strings.Fields(s), " ")
}

// userVersion gives the version the file is at.
//
// It is 0 in a file that was made before the versions were kept.
func userVersion(ctx context.Context, conn *sql.Conn) (int, error) {
	var v int
	if err := conn.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return 0, fmt.Errorf("migrate: read the version: %w", err)
	}
	return v, nil
}

func ms(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

// fromMS gives the time a millisecond count says.
//
// The time is in UTC: everything that is written in is in UTC, and a
// time that comes back in the machine's own zone is a time that says
// two different things depending on who reads it.
func fromMS(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.UnixMilli(v).UTC()
}

// AtMillisecond gives the time as the store writes it down.
//
// 库里存的时间是毫秒 —— 比毫秒细的那部分存下去就没有了。所以拿一个时刻跟
// 库里的比，要先把它也砍到毫秒：不砍的话，库里那个（毫秒）永远比手里那个
// （毫秒以下）旧那么一点，于是每一次比较都说「要重写」。
func AtMillisecond(t time.Time) time.Time {
	return fromMS(ms(t))
}

// CreateAccount inserts an account.
func (s *Store) CreateAccount(ctx context.Context, a Account) error {
	return s.createAccount(ctx, s.db, a)
}

// createAccount inserts an account, on the pool or inside a
// transaction.
//
// 同一句语句在事务里和事务外都要用：能跑语句的就这三样，所以语句只
// 写一遍。
func (s *Store) createAccount(ctx context.Context, db execer, a Account) error {
	if err := checkStatus(a.Status); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO accounts (id, name, user_id, email, status, proxy_url, proxy_group,
			last_error, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)`,
		a.ID, a.Name, a.UserID, strings.TrimSpace(a.Email), a.Status, a.ProxyURL,
		a.ProxyGroup, a.LastError, ms(s.stamp(a.CreatedAt)), ms(s.stamp(a.UpdatedAt)))
	if err != nil {
		return fmt.Errorf("create account: %w", err)
	}
	return nil
}

// UpdateAccount writes an account's fields.
//
// An account that is not there is an error, not a quiet success: the
// caller believes it wrote something.
//
// It writes every field, so a caller that wants to change one of them
// has to read the account first. The setters below change one field
// without that, and are the ones to use.
func (s *Store) UpdateAccount(ctx context.Context, a Account) error {
	if err := checkStatus(a.Status); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE accounts SET name=?, user_id=?, email=?, status=?, proxy_url=?, proxy_group=?,
			last_error=?, updated_at=?
		WHERE id=?`,
		a.Name, a.UserID, a.Email, a.Status, a.ProxyURL, a.ProxyGroup,
		a.LastError, ms(s.stamp(a.UpdatedAt)), a.ID)
	if err != nil {
		return fmt.Errorf("update account: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("update account %s: %w", a.ID, ErrNotFound)
	}
	return nil
}

// UpdateAccountStatus writes an account's state and its last error.
//
// It is narrower than UpdateAccount on purpose: the caller does not
// have to read the account first, and a change that touches the state
// cannot lose a change that touched something else.
func (s *Store) UpdateAccountStatus(ctx context.Context, id, status, lastError string, updatedAt time.Time) error {
	return s.updateAccountStatus(ctx, s.db, id, status, lastError, updatedAt)
}

func (s *Store) updateAccountStatus(ctx context.Context, db execer, id, status, lastError string, updatedAt time.Time) error {
	if err := checkStatus(status); err != nil {
		return err
	}
	res, err := db.ExecContext(ctx, `
		UPDATE accounts SET status=?, last_error=?, updated_at=? WHERE id=?`,
		status, lastError, ms(s.stamp(updatedAt)), id)
	if err != nil {
		return fmt.Errorf("update the status of the account %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("update the status of the account %s: %w", id, ErrNotFound)
	}
	return nil
}

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

func (s *Store) setAccountOutlet(ctx context.Context, db execer, id, url, group string, updatedAt time.Time) error {
	res, err := db.ExecContext(ctx, `
		UPDATE accounts SET proxy_url=?, proxy_group=?, updated_at=? WHERE id=?`,
		strings.TrimSpace(url), strings.TrimSpace(group), ms(s.stamp(updatedAt)), id)
	if err != nil {
		return fmt.Errorf("set the outlet of the account %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("set the outlet of the account %s: %w", id, ErrNotFound)
	}
	return nil
}

// SetAccountName writes the account's name.
//
// It is narrow like SetAccountOutlet: the name is the only thing it
// writes, so a caller that renames an account does not have to read
// the account first, and cannot lose a change another writer made.
func (s *Store) SetAccountName(ctx context.Context, id, name string, updatedAt time.Time) error {
	return s.setAccountName(ctx, s.db, id, name, updatedAt)
}

func (s *Store) setAccountName(ctx context.Context, db execer, id, name string, updatedAt time.Time) error {
	res, err := db.ExecContext(ctx, `
		UPDATE accounts SET name=?, updated_at=? WHERE id=?`,
		name, ms(s.stamp(updatedAt)), id)
	if err != nil {
		return fmt.Errorf("set the name of the account %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("set the name of the account %s: %w", id, ErrNotFound)
	}
	return nil
}

// SetAccountIdentity writes an account's address and user id.
//
// 它是窄的：只写这两样。站点那边报出来的真邮箱和真用户 id 知道了就写
// 上去——不去重的时候邮箱是键之一，键停在旧的地方，下一次导入就会把
// 这个账号当成新账号，再插一行。
//
// 地址写进去之前是要去空格的：地址是去重的键之一，同一个地址写成两种
// 样子，就看成两个账号。
func (s *Store) SetAccountIdentity(ctx context.Context, id, email, userID string, updatedAt time.Time) error {
	return s.setAccountIdentity(ctx, s.db, id, email, userID, updatedAt)
}

func (s *Store) setAccountIdentity(ctx context.Context, db execer, id, email, userID string, updatedAt time.Time) error {
	res, err := db.ExecContext(ctx, `
		UPDATE accounts SET email=?, user_id=?, updated_at=? WHERE id=?`,
		strings.TrimSpace(email), userID, ms(s.stamp(updatedAt)), id)
	if err != nil {
		return fmt.Errorf("set the identity of the account %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("set the identity of the account %s: %w", id, ErrNotFound)
	}
	return nil
}

// SignIn records a sign-in: the account and its token.
//
// 一个站点账号在库里只能有一行。库里已经有这一行的时候改那一行，不另
// 起一行——同一个站点账号两行都是活的，轮转会挑到两次，页面把它列两遍，
// 而且其中一个的令牌迟早被另一个的重登作废。
//
// 找的是令牌里带的那个 sub：那是站点账号自己的 id，一个站点账号一个，
// 重登多少次都是它。清单上的邮箱可能是别名，站点那边报的邮箱又可能一
// 时问不出来，只有它是一直在的。邮箱是第二条路：有的登录那条路上，令牌
// 里的 sub 是空的。
//
// a 是这个账号该有的样子：它的 id 只在没有那一行的时候才用得上。a.Email
// 是站点那边说的地址：它是空的，说明这一次没问出身份来——那就不动那一
// 行上的地址和用户 id，把空的写上去等于把知道的东西换成不知道的。
//
// 名字是人自己写的标签：给了就写上去，没给就把原来那一行上的留着。
func (s *Store) SignIn(ctx context.Context, a Account, tok Token) (Account, error) {
	var out Account
	err := s.Tx(ctx, func(tx Tx) error {
		row, err := findForSignIn(ctx, tx, a, tok)
		if err != nil {
			return err
		}
		now := a.UpdatedAt
		if row.ID == "" {
			// 没有那一行：这就是一个新账号。名字没有给定的时候，用知道
			// 的东西起：地址、用户 id、令牌里带的那个站点账号 id。
			acc := a
			acc.Name = firstNonEmpty(a.Name, a.Email, a.UserID, tok.Subject)
			if err := tx.CreateAccount(ctx, acc); err != nil {
				return err
			}
			row = acc
		} else {
			// 已经有这一行了：改它，不另起一行。
			if a.Email != "" && (a.Email != row.Email || a.UserID != row.UserID) {
				if err := tx.SetAccountIdentity(ctx, row.ID, a.Email, a.UserID, now); err != nil {
					return err
				}
				row.Email = a.Email
				row.UserID = a.UserID
			}
			if a.Name != "" && a.Name != row.Name {
				if err := tx.SetAccountName(ctx, row.ID, a.Name, now); err != nil {
					return err
				}
				row.Name = a.Name
			}
			if row.Status != StatusActive {
				// 它刚登进来：状态回到 active。
				if err := tx.UpdateAccountStatus(ctx, row.ID, StatusActive, "", now); err != nil {
					return err
				}
				row.Status = StatusActive
			}
		}
		if err := tx.SaveToken(ctx, Token{
			AccountID: row.ID, Access: tok.Access, Refresh: tok.Refresh,
			ExpiresAt: tok.ExpiresAt, Subject: tok.Subject, UpdatedAt: now,
		}); err != nil {
			return err
		}
		out = row
		return nil
	})
	if err != nil {
		return Account{}, err
	}
	return out, nil
}

// findForSignIn gives the row that belongs to this site account.
//
// It gives an empty account when there is none: that is a new account,
// not an error.
func findForSignIn(ctx context.Context, tx Tx, a Account, tok Token) (Account, error) {
	if tok.Subject != "" {
		row, err := tx.AccountBySubject(ctx, tok.Subject)
		if err == nil {
			return row, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return Account{}, err
		}
	}
	if a.Email != "" {
		row, err := tx.AccountByEmail(ctx, a.Email)
		if err == nil {
			return row, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return Account{}, err
		}
	}
	// 站点给的那个真的 user id：有的账号没有邮箱，令牌里也没有 sub，
	// 只有它认得出是哪一个站点账号。少了这一步，这样的账号每登一次就
	// 多一行活的。
	if a.UserID != "" {
		row, err := tx.AccountByUserID(ctx, a.UserID)
		if err == nil {
			return row, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return Account{}, err
		}
	}
	return Account{}, nil
}

// firstNonEmpty gives the first value that is not empty.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// MarkSignedOut writes the reason a sign-in failed, but only while the
// account is still signed out.
//
// 它是有条件的：另一个进程可能刚把这个账号登回去了，那一行现在是
// active 了，这时候把它写回 signed_out，就是把别人刚换好的令牌作废掉，
// 还让这个账号显示成登出的状态——手里却握着一份能用的凭据。轮转池不
// 会用它，下一轮又要登一次，白白多登一次。
//
// 它说有没有写下去：没有写，说明这一行的状态已经不是签出的了。
func (s *Store) MarkSignedOut(ctx context.Context, id, lastError string, updatedAt time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE accounts SET status=?, last_error=?, updated_at=? WHERE id=? AND status=?`,
		StatusSignedOut, lastError, ms(s.stamp(updatedAt)), id, StatusSignedOut)
	if err != nil {
		return false, fmt.Errorf("mark the account %s as signed out: %w", id, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SignOutIfTheTokenIsStillThere signs the account out and takes its
// token away, but only while the token row still holds that access
// token.
//
// 签出是**有条件**地写的：另一个进程可能刚把这个账号登回去了。手里这
// 份令牌就是这次失败说的那份；库里换成了别的，就是有人替它登好了，这
// 时候把它写回签出，就是把别人刚换好的令牌作废掉，还让这个账号显示成
// 登出的状态——手里却握着一份能用的凭据，轮转池不用它，下一轮又要登
// 一次。
//
// 它说有没有写下去。没有写，说明库里那一份令牌已经不是手里这一份了：
// 有人在这中间把账号救回来了，这次的失败说的已经不是它了。
//
// 写状态和收走令牌是一起写的。中间插进来一个写，账号就会停在「状态是
// 签出、令牌还在」的样子：状态说它登出了，令牌却还在，每次请求都要
// 去续一次，续不回来。
//
// 令牌行里的 subject 留着。那是站点那边账号自己的 id，不是凭据——它是
// 「这个站点账号在库里是哪一行」的键。抹掉它，下次导入这个账号就会当
// 成新账号再插一行，而那一行是活的。
func (s *Store) SignOutIfTheTokenIsStillThere(ctx context.Context, id, access, reason string, updatedAt time.Time) (bool, error) {
	written := false
	err := s.tx(ctx, func(tx *sqlTx) error {
		n, err := tx.setStatusIfTokenUnchanged(ctx, id, access, StatusSignedOut, reason, updatedAt)
		if err != nil || !n {
			return err
		}
		written = true
		return tx.blankToken(ctx, id, updatedAt)
	})
	if err != nil {
		return false, err
	}
	return written, nil
}

// MarkErrorIfTheTokenIsStillThere writes the error state down, but only
// while the token row still holds that access token.
//
// 它和 SignOutIfTheTokenIsStillThere 是同一个条件、同一件事：这个错误
// 说的是手里这份令牌的事，库里换成了别的令牌，说的就不是这个账号了。
// 把别人刚换好的状态改回出错，页面就会说一个好好在用的账号出错了。
func (s *Store) MarkErrorIfTheTokenIsStillThere(ctx context.Context, id, access, reason string, updatedAt time.Time) (bool, error) {
	return s.setStatusIfTokenUnchanged(ctx, s.db, id, access, StatusError, reason, updatedAt)
}

// setStatusIfTokenUnchanged writes the state and the reason, but only
// while the token row still holds that access token.
//
// The access token is what says the chain in hand is still the chain in
// the store: every renewal hands out a new access token, so a different
// one means somebody else has moved the account on since.
//
// 手里没有令牌可比的时候不写：这次失败说的不是库里那一份令牌，写下去
// 只能靠猜。
func (s *Store) setStatusIfTokenUnchanged(ctx context.Context, db execer, id, access, status, reason string, updatedAt time.Time) (bool, error) {
	if strings.TrimSpace(access) == "" {
		return false, nil
	}
	res, err := db.ExecContext(ctx, `
		UPDATE accounts SET status=?, last_error=?, updated_at=?
		WHERE id=? AND EXISTS (
			SELECT 1 FROM tokens t WHERE t.account_id=accounts.id AND t.access=?
		)`, status, reason, ms(s.stamp(updatedAt)), id, access)
	if err != nil {
		return false, fmt.Errorf("set the state of the account %s: %w", id, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// blankToken takes an account's token away.
//
// The subject stays: it is the site account's own id, not a credential,
// and it is what says which site account the row is. Wiping it would
// make the next import of that account make a second row — a live one.
func (s *Store) blankToken(ctx context.Context, db execer, id string, updatedAt time.Time) error {
	_, err := db.ExecContext(ctx, `
		UPDATE tokens SET access='', refresh='', expires_at=0, updated_at=? WHERE account_id=?`,
		ms(s.stamp(updatedAt)), id)
	if err != nil {
		return fmt.Errorf("take the token of the account %s away: %w", id, err)
	}
	return nil
}

// SetAccountLastError writes the account's last error.
//
// It writes the error and nothing else. It does not write the status
// either: an account that is healthy and an account whose last reading
// failed are two different things, and writing the status here would
// mean reading it first and writing back a value that could be stale.
func (s *Store) SetAccountLastError(ctx context.Context, id, lastError string, updatedAt time.Time) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE accounts SET last_error=?, updated_at=? WHERE id=?`,
		lastError, ms(s.stamp(updatedAt)), id)
	if err != nil {
		return fmt.Errorf("set the last error of the account %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("set the last error of the account %s: %w", id, ErrNotFound)
	}
	return nil
}

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

// SetPassword writes an account's encrypted password.
//
// The cipher text is written on its own, so that the other setters --
// which write one column each -- do not have to know about it.
//
// An account that is not there is ErrNotFound: the caller believes it
// wrote a password, and writing nothing is not writing one.
func (s *Store) SetPassword(ctx context.Context, id, cipher string, updatedAt time.Time) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE accounts SET password=?, updated_at=? WHERE id=?`,
		cipher, ms(s.stamp(updatedAt)), id)
	if err != nil {
		return fmt.Errorf("set the password of the account %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("set the password of the account %s: %w", id, ErrNotFound)
	}
	return nil
}

// Password gives an account's encrypted password.
//
// An account that was not brought in by a batch import has none, and
// that is not an error: the text is empty.
//
// An account that is not there at all has no password either, and that
// is not an error either: the caller asked about a password, and there
// is none. A caller that cares whether the account is there asks for
// the account.
func (s *Store) Password(ctx context.Context, id string) (string, error) {
	var cipher sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT password FROM accounts WHERE id=?`, id).Scan(&cipher)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get the password of the account %s: %w", id, err)
	}
	return cipher.String, nil
}

// CreateImportRun records the start of a batch import.
//
// The run is written down before the accounts are made, so that an
// import that is still going can be seen.
func (s *Store) CreateImportRun(ctx context.Context, r ImportRun) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO import_runs (id, source, started_at, total, ok, failed)
		VALUES (?,?,?,?,?,?)`,
		r.ID, r.Source, ms(s.stamp(r.StartedAt)), r.Total, r.OK, r.Failed)
	if err != nil {
		return fmt.Errorf("create import run %s: %w", r.ID, err)
	}
	return nil
}

// FinishImportRun records what a batch import came to.
//
// The results are kept as they were given: they are the reader's to
// read, and the store does not have to know what is in them.
func (s *Store) FinishImportRun(ctx context.Context, id string, finishedAt time.Time, ok, failed int, results string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE import_runs SET finished_at=?, ok=?, failed=?, results=? WHERE id=?`,
		ms(s.stamp(finishedAt)), ok, failed, results, id)
	if err != nil {
		return fmt.Errorf("finish import run %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("finish import run %s: %w", id, ErrNotFound)
	}
	return nil
}

// GetImportRun gives one batch import by its name.
//
// A name that is not there is ErrNotFound: the caller asked about one
// particular import, and being handed a different one would be worse
// than being told there is none.
func (s *Store) GetImportRun(ctx context.Context, id string) (ImportRun, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, source, started_at, finished_at, total, ok, failed,
			COALESCE(results, '')
		FROM import_runs WHERE id=?`, id)
	r, err := scanImportRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ImportRun{}, fmt.Errorf("get import run %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return ImportRun{}, fmt.Errorf("get import run %s: %w", id, err)
	}
	return r, nil
}

// ListImportRuns gives the most recent batch imports.
//
// A limit of zero or less means the usual twenty: a limit is a limit,
// and a limit of zero is no limit at all, which is not what a reader
// of the page wants.
func (s *Store) ListImportRuns(ctx context.Context, limit int) ([]ImportRun, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, source, started_at, finished_at, total, ok, failed,
			COALESCE(results, '')
		FROM import_runs ORDER BY started_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list import runs: %w", err)
	}
	defer rows.Close()
	var out []ImportRun
	for rows.Next() {
		r, err := scanImportRun(rows)
		if err != nil {
			return nil, fmt.Errorf("list import runs: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list import runs: %w", err)
	}
	return out, nil
}

// scanImportRun reads one batch import.
func scanImportRun(sc scanner) (ImportRun, error) {
	var (
		r        ImportRun
		started  int64
		finished sql.NullInt64
		results  sql.NullString
	)
	if err := sc.Scan(&r.ID, &r.Source, &started, &finished, &r.Total, &r.OK,
		&r.Failed, &results); err != nil {
		return ImportRun{}, err
	}
	r.StartedAt = fromMS(started)
	r.FinishedAt = fromMS(finished.Int64)
	r.Results = results.String
	return r, nil
}

// checkStatus says whether a status is one of ours.
//
// The rotator looks for the active accounts, so an account with a
// status nobody wrote is an account that is quietly never used.
func checkStatus(status string) error {
	switch status {
	case StatusActive, StatusSignedOut, StatusError:
		return nil
	}
	return fmt.Errorf("unknown account status %q: it is %q, %q or %q",
		status, StatusActive, StatusSignedOut, StatusError)
}

// execer is what can run a statement.
//
// 事务里和事务外跑的是同一批语句：连接池和事务都做得到，所以语句只
// 写一遍，事务里跑的也是同一句。
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Tx is what one transaction can do.
//
// It is the same set of writes the store has, but inside one
// transaction: they all land, or none of them does.
type Tx interface {
	// AccountByEmail gives the account with that email, when there is
	// one. It says ErrNotFound when there is none.
	AccountByEmail(ctx context.Context, email string) (Account, error)
	// AccountBySubject gives the account whose token carries that
	// subject, when there is one. It says ErrNotFound when there is
	// none.
	AccountBySubject(ctx context.Context, subject string) (Account, error)
	// AccountByUserID gives the account with that site user id, when
	// there is one. It says ErrNotFound when there is none.
	AccountByUserID(ctx context.Context, userID string) (Account, error)
	// CreateAccount inserts an account.
	CreateAccount(ctx context.Context, a Account) error
	// SaveToken writes an account's token.
	SaveToken(ctx context.Context, t Token) error
	// SetAccountIdentity writes an account's address and user id.
	SetAccountIdentity(ctx context.Context, id, email, userID string, updatedAt time.Time) error
	// SetAccountName writes the account's name.
	SetAccountName(ctx context.Context, id, name string, updatedAt time.Time) error
	// SetAccountOutlet writes the account's outlet.
	SetAccountOutlet(ctx context.Context, id, url, group string, updatedAt time.Time) error
	// UpdateAccountStatus writes the account's state and its last
	// error.
	UpdateAccountStatus(ctx context.Context, id, status, lastError string, updatedAt time.Time) error
}

// Tx runs fn inside one write transaction.
//
// The write lock is taken when the transaction starts (the data source
// name says _txlock=immediate), so "look the email up, and make a row
// when there is none" cannot be done by two imports at the same time.
// Without that, two concurrent imports of one email leave two live rows
// of one site account: both of them are in the rotation pool, the page
// lists the account twice, and the token of one of them is bound to be
// thrown away by the other one's re-login.
//
// An error from fn rolls the transaction back: a reader never sees a
// row whose token was not written.
//
// The rollback does not need the caller's context: the caller may have
// been cut short, and the half of the transaction that did land has to
// go back all the same.
func (s *Store) Tx(ctx context.Context, fn func(Tx) error) error {
	return s.tx(ctx, func(tx *sqlTx) error { return fn(tx) })
}

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

// sqlTx is one transaction of the store.
//
// 它拿着库，是为了那几样要盖时间戳的语句：时间戳是库自己的时钟盖的
// （东八区），不是调用方的。
type sqlTx struct {
	s  *Store
	tx *sql.Tx
}

func (t *sqlTx) AccountByEmail(ctx context.Context, email string) (Account, error) {
	return t.s.accountByEmail(ctx, t.tx, email)
}

func (t *sqlTx) AccountBySubject(ctx context.Context, subject string) (Account, error) {
	return t.s.accountBySubject(ctx, t.tx, subject)
}

func (t *sqlTx) AccountByUserID(ctx context.Context, userID string) (Account, error) {
	return t.s.accountByUserID(ctx, t.tx, userID)
}

func (t *sqlTx) CreateAccount(ctx context.Context, a Account) error {
	return t.s.createAccount(ctx, t.tx, a)
}

func (t *sqlTx) SaveToken(ctx context.Context, tok Token) error {
	return t.s.saveToken(ctx, t.tx, tok)
}

func (t *sqlTx) SetAccountIdentity(ctx context.Context, id, email, userID string, updatedAt time.Time) error {
	return t.s.setAccountIdentity(ctx, t.tx, id, email, userID, updatedAt)
}

func (t *sqlTx) SetAccountName(ctx context.Context, id, name string, updatedAt time.Time) error {
	return t.s.setAccountName(ctx, t.tx, id, name, updatedAt)
}

func (t *sqlTx) SetAccountOutlet(ctx context.Context, id, url, group string, updatedAt time.Time) error {
	return t.s.setAccountOutlet(ctx, t.tx, id, url, group, updatedAt)
}

func (t *sqlTx) UpdateAccountStatus(ctx context.Context, id, status, lastError string, updatedAt time.Time) error {
	return t.s.updateAccountStatus(ctx, t.tx, id, status, lastError, updatedAt)
}

func (t *sqlTx) setStatusIfTokenUnchanged(ctx context.Context, id, access, status, reason string, updatedAt time.Time) (bool, error) {
	return t.s.setStatusIfTokenUnchanged(ctx, t.tx, id, access, status, reason, updatedAt)
}

func (t *sqlTx) blankToken(ctx context.Context, id string, updatedAt time.Time) error {
	return t.s.blankToken(ctx, t.tx, id, updatedAt)
}

// DeleteAccount removes an account and everything under it.
//
// The children are taken away by the database: the tables say what
// belongs to an account, and ON DELETE CASCADE takes it away with the
// account. So there is no list of children here that can fall out of
// step with the schema.
//
// An account that is not there is an error: the caller believes
// something was removed.
func (s *Store) DeleteAccount(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM accounts WHERE id=?", id)
	if err != nil {
		return fmt.Errorf("delete account %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("delete account %s: %w", id, ErrNotFound)
	}
	return nil
}

// ListAccounts gives every account, oldest first.
func (s *Store) ListAccounts(ctx context.Context) ([]Account, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, user_id, email, status, proxy_url, proxy_group,
			last_error, created_at, updated_at
		FROM accounts ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	return out, nil
}

// GetAccount gives one account.
//
// An account that is not there is ErrNotFound.
func (s *Store) GetAccount(ctx context.Context, id string) (Account, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, name, user_id, email, status, proxy_url, proxy_group,
			last_error, created_at, updated_at
		FROM accounts WHERE id=?`, id)
	a, err := scanAccount(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, fmt.Errorf("get account %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return Account{}, fmt.Errorf("get account %s: %w", id, err)
	}
	return a, nil
}

// AccountByEmail gives the account with that email.
//
// 邮箱是站点那边认的东西：同一个邮箱只该有一行。重跑一次导入是常事
// （上一批有一部分失败了，重来一遍是补救的办法），再来一行就是同一个
// 站点账号有两个活着的行，两个都在轮转池里，页面把它列两遍。
//
// 表不大——几百行——一次全表扫就够了，不值得为它改表的结构或者加个
// 索引。邮箱比的时候两边都折成小写：清单上写的是哪个样子，库里存的
// 是哪个样子，人看着是同一个就是同一个。
//
// 没有这一行是 ErrNotFound。
func (s *Store) AccountByEmail(ctx context.Context, email string) (Account, error) {
	return s.accountByEmail(ctx, s.db, email)
}

func (s *Store) accountByEmail(ctx context.Context, db execer, email string) (Account, error) {
	email = strings.TrimSpace(email)
	if email == "" {
		return Account{}, fmt.Errorf("get the account by email: %w", ErrNotFound)
	}
	row := db.QueryRowContext(ctx, `
		SELECT id, name, user_id, email, status, proxy_url, proxy_group,
			last_error, created_at, updated_at
		FROM accounts WHERE lower(trim(email)) = lower(trim(?))
		ORDER BY created_at, id LIMIT 1`, email)
	a, err := scanAccount(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, fmt.Errorf("get the account by email %s: %w", email, ErrNotFound)
	}
	if err != nil {
		return Account{}, fmt.Errorf("get the account by email %s: %w", email, err)
	}
	return a, nil
}

// AccountByUserID gives the account with that site user id.
//
// 用户 id 是站点那边给的那个真的那个（realUserID）：签到和积分明细要的
// 是它。它是账号最稳当的钥匙——有的账号没有邮箱，令牌里也没有 sub，只
// 有它一直在。
//
// 空的 user id 不是一把钥匙：没有它的账号，谁也不是。
//
// 没有这一行是 ErrNotFound。
func (s *Store) AccountByUserID(ctx context.Context, userID string) (Account, error) {
	return s.accountByUserID(ctx, s.db, userID)
}

func (s *Store) accountByUserID(ctx context.Context, db execer, userID string) (Account, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return Account{}, fmt.Errorf("get the account by user id: %w", ErrNotFound)
	}
	row := db.QueryRowContext(ctx, `
		SELECT id, name, user_id, email, status, proxy_url, proxy_group,
			last_error, created_at, updated_at
		FROM accounts WHERE user_id = ?
		ORDER BY created_at, id LIMIT 1`, userID)
	a, err := scanAccount(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, fmt.Errorf("get the account by user id %s: %w", userID, ErrNotFound)
	}
	if err != nil {
		return Account{}, fmt.Errorf("get the account by user id %s: %w", userID, err)
	}
	return a, nil
}

// AccountBySubject gives the account whose token carries that subject.
//
// 令牌里的那个 sub 是站点账号自己的 id：一个站点账号一个，重登多少次
// 都是它。它比邮箱靠得住——清单上写的邮箱可能是别名，站点那边报的邮箱
// 又可能一时问不出来，只有它是一直在的。
//
// 表不大——几百行——一次全表扫就够了，不值得为它改表的结构或者加个
// 索引。
//
// 没有这一行是 ErrNotFound。
func (s *Store) AccountBySubject(ctx context.Context, subject string) (Account, error) {
	return s.accountBySubject(ctx, s.db, subject)
}

func (s *Store) accountBySubject(ctx context.Context, db execer, subject string) (Account, error) {
	subject = strings.TrimSpace(subject)
	if subject == "" {
		return Account{}, fmt.Errorf("get the account by subject: %w", ErrNotFound)
	}
	row := db.QueryRowContext(ctx, `
		SELECT a.id, a.name, a.user_id, a.email, a.status, a.proxy_url, a.proxy_group,
			a.last_error, a.created_at, a.updated_at
		FROM accounts a JOIN tokens t ON t.account_id = a.id
		WHERE t.subject = ?
		ORDER BY a.created_at, a.id LIMIT 1`, subject)
	a, err := scanAccount(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, fmt.Errorf("get the account by subject %s: %w", subject, ErrNotFound)
	}
	if err != nil {
		return Account{}, fmt.Errorf("get the account by subject %s: %w", subject, err)
	}
	return a, nil
}

type scanner interface{ Scan(dest ...any) error }

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
	// 有人申请了 key：那个「有没有 key」的答案变了，扔掉它（见
	// forgetAPIKeys）。不改这一句的话，下一个不带 key 的请求还会被放行
	// —— 那是鉴权上的窟窿。
	s.forgetAPIKeys()
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
	// 有人撤销了 key：那个「有没有 key」的答案变了，扔掉它（见
	// forgetAPIKeys）。不改这一句的话，门还关着 —— 人已经撤销了，请求
	// 却还进不来。
	s.forgetAPIKeys()
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

// keyState is what the store knows about the keys in the console.
//
// 三态：「没查过」/「一把都没有」/「有」。「一把都没有」跟「没查过」必须分开
// —— 分不开的话缓存永远不命中（每次都当没查过）。
type keyState int32

const (
	// keysUnknown says nobody has asked yet.
	keysUnknown keyState = iota
	// keysNone says there is no key.
	keysNone
	// keysSome says there is at least one key.
	keysSome
)

// HasAPIKeys says whether any key has been made in the console.
//
// 一把都没有的时候网关不鉴权：那时候它只该听在本机。申请过就不一样
// 了：那是有人打算从别的机器用它。
//
// 答案记在内存里：这个数每个请求都要问一次，而它变不变只有一种理由 —— 有
// 人申请了或者撤销了 key。谁改了谁就把这个答案扔掉（见 forgetAPIKeys）。
func (s *Store) HasAPIKeys(ctx context.Context) (bool, error) {
	switch keyState(s.apiKeys.Load()) {
	case keysSome:
		return true, nil
	case keysNone:
		return false, nil
	}
	// 没查过：去查库。查库和写答案锁在一起，不然一个正在查库的请求会在
	// 别人申请了 key 之后，把「一把都没有」这个旧答案写回去 —— 那个答案
	// 留在缓存里不动了，门就一直开着。
	s.apiKeysMu.Lock()
	defer s.apiKeysMu.Unlock()
	// 等锁的这会儿，答案可能已经有人填上了。
	switch keyState(s.apiKeys.Load()) {
	case keysSome:
		return true, nil
	case keysNone:
		return false, nil
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM api_keys WHERE revoked_at IS NULL`).Scan(&n); err != nil {
		return false, fmt.Errorf("count api keys: %w", err)
	}
	state := keysNone
	if n > 0 {
		state = keysSome
	}
	s.apiKeys.Store(int32(state))
	return n > 0, nil
}

// forgetAPIKeys throws the cached answer away.
//
// 谁改了 key 谁就通知：这个答案是「要不要鉴权」，它慢了半秒就是半秒的窗口
// 里不带 key 的请求被放行。
//
// 它拿着那把锁：不然一个正在查库的请求会在这次通知之后把旧答案写回去。
func (s *Store) forgetAPIKeys() {
	s.apiKeysMu.Lock()
	defer s.apiKeysMu.Unlock()
	s.apiKeys.Store(int32(keysUnknown))
}

// SaveToken writes an account's token.
//
// 这笔丢不得：库里那笔 token 丢了，下一次续签拿的就是一个已经被站点用过
// 一次的 refresh token，站点会拒，账号就被记成签入失效。所以它走那条
// fsync 的连接：写完了就在盘上。
func (s *Store) SaveToken(ctx context.Context, t Token) error {
	return s.saveToken(ctx, s.durable, t)
}

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

// GetToken gives an account's token.
//
// A token that is not there is ErrNotFound.
func (s *Store) GetToken(ctx context.Context, accountID string) (Token, error) {
	var (
		t         Token
		expiresAt int64
		updatedAt int64
		subject   sql.NullString
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT account_id, access, refresh, expires_at, subject, updated_at
		FROM tokens WHERE account_id=?`, accountID).
		Scan(&t.AccountID, &t.Access, &t.Refresh, &expiresAt, &subject, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Token{}, fmt.Errorf("get token %s: %w", accountID, ErrNotFound)
	}
	if err != nil {
		return Token{}, fmt.Errorf("get token %s: %w", accountID, err)
	}
	t.Subject = subject.String
	t.ExpiresAt = fromMS(expiresAt)
	t.UpdatedAt = fromMS(updatedAt)
	return t, nil
}

// SaveCheckin writes a day's check-in, replacing an earlier one for the
// same day.
func (s *Store) SaveCheckin(ctx context.Context, c Checkin) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO checkins (account_id, day, outcome, credit, streak, message, created_at)
		VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(account_id, day) DO UPDATE SET
			outcome=excluded.outcome, credit=excluded.credit, streak=excluded.streak,
			message=excluded.message, created_at=excluded.created_at`,
		c.AccountID, c.Day, c.Outcome, c.Credit, c.Streak, c.Message, ms(s.stamp(c.CreatedAt)))
	if err != nil {
		return fmt.Errorf("save checkin: %w", err)
	}
	return nil
}

// SaveCheckins writes several check-ins down in one transaction.
//
// 一趟签到有几百个号：一个号一个事务的话，每个号都要把签到表的那一页和
// 它的几个索引页重写一遍 —— 一千个号是 17 MB 的日志，而合成一个事务写
// 是 1 MB（量出来的：每个号 1133 B，而不是 18004 B）。
//
// 一整批是一个事务：一个写不下去，一整批都回去。
//
// 全批一起回是划得来的：签到的记录是彼此独立的，可是它们写的是同一张表的
// 同一批页 —— 半批写下去比全批写下去更贵，而丢掉的最多是这一批里那些号
// 今天的积分进不了池（规格 §5.3）。丢得起。
func (s *Store) SaveCheckins(ctx context.Context, cs []Checkin) error {
	if len(cs) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("save checkins: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO checkins (account_id, day, outcome, credit, streak, message, created_at)
		VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(account_id, day) DO UPDATE SET
			outcome=excluded.outcome, credit=excluded.credit, streak=excluded.streak,
			message=excluded.message, created_at=excluded.created_at`)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("save checkins: %w", err)
	}
	for _, c := range cs {
		if _, err := stmt.ExecContext(ctx,
			c.AccountID, c.Day, c.Outcome, c.Credit, c.Streak, c.Message, ms(s.stamp(c.CreatedAt))); err != nil {
			_ = stmt.Close()
			_ = tx.Rollback()
			return fmt.Errorf("save checkin: %w", err)
		}
	}
	if err := stmt.Close(); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("save checkins: %w", err)
	}
	if err := tx.Commit(); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("save checkins: %w", err)
	}
	return nil
}

// ListCheckins gives an account's check-ins, newest first.
//
// A limit of zero or less means every check-in: a limit is a limit,
// and a limit of zero is no limit at all.
func (s *Store) ListCheckins(ctx context.Context, accountID string, limit int) ([]Checkin, error) {
	q := `
		SELECT id, account_id, day, outcome, credit, streak, message, created_at
		FROM checkins WHERE account_id=? ORDER BY day DESC`
	args := []any{accountID}
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list checkins: %w", err)
	}
	defer rows.Close()
	var out []Checkin
	for rows.Next() {
		var (
			c         Checkin
			credit    sql.NullFloat64
			streak    sql.NullInt64
			message   sql.NullString
			createdAt int64
		)
		if err := rows.Scan(&c.ID, &c.AccountID, &c.Day, &c.Outcome, &credit, &streak, &message, &createdAt); err != nil {
			return nil, fmt.Errorf("list checkins: %w", err)
		}
		c.Credit = credit.Float64
		c.Streak = int(streak.Int64)
		c.Message = message.String
		c.CreatedAt = fromMS(createdAt)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list checkins: %w", err)
	}
	return out, nil
}

// CheckinTime is when an account was checked in.
type CheckinTime struct {
	// AccountID is the account.
	AccountID string
	// At is when it was checked in.
	At time.Time
}

// CheckinTimes gives, for every account checked in since a moment, the
// time of that check-in.
//
// 它是「签到待补」那一档要的东西：这个号最后一次签到是什么时候（规格
// §5.3）。签到给了积分，卡上还摆着签到之前的旧数 —— 那种号要补读一次
// 额度。
//
// 问的是「最近」，不是「全部」：签到的积分七天就过期了（规格 §5.3），七
// 天以前的签到既不会让一个号变成「待补」，也不会在十分钟的重试窗口里。
// 而问「全部」的那条查询在有历史的库上慢得没法看：一年 36 万行，138 秒
// —— 而这一档每 30 秒问一次。
//
// 每个号一条：它在这段时间里最后一次签到。时刻说了算（MAX 出来的就是
// 最大的时刻），平局的时候是哪一个不影响 —— 这里的时刻就是全部。
func (s *Store) CheckinTimes(ctx context.Context, since time.Time) ([]CheckinTime, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT account_id, MAX(created_at) FROM checkins
		WHERE created_at >= ? GROUP BY account_id`, ms(since))
	if err != nil {
		return nil, fmt.Errorf("the check-in times: %w", err)
	}
	defer rows.Close()
	var out []CheckinTime
	for rows.Next() {
		var (
			t  CheckinTime
			at int64
		)
		if err := rows.Scan(&t.AccountID, &at); err != nil {
			return nil, fmt.Errorf("the check-in times: %w", err)
		}
		t.At = fromMS(at)
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("the check-in times: %w", err)
	}
	return out, nil
}

// PruneCheckins throws away the check-ins of the days before a day.
//
// 签到的历史是唯一会长个没完的东西：一天一个号一行，1000 个号一年 36 万
// 行、40 MB。签到的积分七天就过期了（规格 §5.3），网页上每个号只摆最近
// 5 条 —— 再往前的记录除了占地方没有别的用处。
//
// 留多少天是外面说了算（sqlite.checkin_retention_days）。
func (s *Store) PruneCheckins(ctx context.Context, olderThan time.Time) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM checkins WHERE day < ?", DayKey(olderThan))
	if err != nil {
		return fmt.Errorf("prune the check-ins: %w", err)
	}
	return nil
}

// SaveUsageSnapshot writes a usage reading.
//
// A reading that could not be made is not a reading of zero. Without
// the amounts the three numbers are NULL, so the reader can tell "no
// idea" from "nothing left" -- and an account with no idea is not an
// account with nothing left.
//
// The errors of the parts that could not be read are written down with
// it: a plan whose windows could not be read is not a plan without
// windows, and the reader has to be able to tell them apart.
//
// The amounts and the flag that says there are amounts are two ways
// of saying one thing, and a caller that forgets the flag would lose
// the numbers without a word. So the flag is worked out from the
// numbers: a reading with a number in it has amounts.
func (s *Store) SaveUsageSnapshot(ctx context.Context, u UsageSnapshot) error {
	has := u.HasAmounts
	if !has && (u.Granted != 0 || u.Consumed != 0 || u.Remaining != 0) {
		// There are numbers and nobody said so. The numbers are the
		// truth here: throwing them away would lose them without a
		// word.
		has = true
	}
	var granted, consumed, remaining any
	if has {
		granted, consumed, remaining = u.Granted, u.Consumed, u.Remaining
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO usage_snapshots (account_id, granted, consumed, remaining,
			expires_at, plan, plan_until, windows_json, balance,
			membership_error, windows_error, plan_error, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		u.AccountID, granted, consumed, remaining, ms(u.ExpiresAt),
		u.Plan, ms(u.PlanUntil), u.WindowsJSON, u.Balance,
		u.MembershipError, u.WindowsError, u.PlanError, ms(s.stamp(u.CreatedAt)))
	if err != nil {
		return fmt.Errorf("save usage snapshot: %w", err)
	}
	return nil
}

// usageSnapshotColumns gives the columns of a usage snapshot, in the
// order scanUsageSnapshot reads them.
//
// The prefix names the table the columns come from: a join needs it,
// or the column names are ambiguous.
func usageSnapshotColumns(prefix string) string {
	cols := []string{
		"id", "account_id", "granted", "consumed", "remaining", "expires_at",
		"plan", "plan_until", "windows_json", "balance",
		"membership_error", "windows_error", "plan_error", "created_at",
	}
	if prefix != "" {
		for i := range cols {
			cols[i] = prefix + "." + cols[i]
		}
	}
	return strings.Join(cols, ", ")
}

// latestUsageSnapshotsSQL is the query that gives every account's newest
// usage reading.
//
// 它是从账号那边走的，不是从读数那边走的：读数表比账号多得多（一个号一
// 刻钟一条的话，一天就是几千行），从读数那边走就是「每读一行问一次这个
// 号最新的是哪一行」—— 110 万行的时候是 1.5 秒，而这条路在卡片的路上
// （用户点一次卡片就等 1.5 秒）。从账号那边走是一千次查找：12 毫秒。
//
// 没有读过的号不在里面：它是 JOIN，不是 LEFT JOIN（规格 §3.4：卡上摆的
// 是每个号最新的一条读数，没有读过的号没有那条）。
//
// 排的是 a.id 而不是 u.account_id：这两个是同一个号（u 就是 a 那个号的
// 最新一条），而账号的主键索引本来就是按 id 排的 —— 排 u.account_id 要让
// SQLite 临时排一遍（USE TEMP B-TREE FOR ORDER BY）。
var latestUsageSnapshotsSQL = `
	SELECT ` + usageSnapshotColumns("u") + `
	FROM accounts a JOIN usage_snapshots u ON u.id = (
		SELECT s2.id FROM usage_snapshots s2
		WHERE s2.account_id = a.id
		ORDER BY s2.created_at DESC, s2.id DESC
		LIMIT 1)
	ORDER BY a.id`

// LatestUsageSnapshots gives each account's newest usage reading.
//
// The newest is the one that was read last: the time says when the
// reading was made, and the id only breaks a tie between two readings
// of the same millisecond.
//
// The id alone is not enough. A reading stamps its time when it starts
// and writes it down when it ends, so two readings of one account that
// overlap leave the older reading with the greater id -- and the card
// would then show the older numbers. The check-ins have the same shape
// and the same rule.
func (s *Store) LatestUsageSnapshots(ctx context.Context) ([]UsageSnapshot, error) {
	rows, err := s.db.QueryContext(ctx, latestUsageSnapshotsSQL)
	if err != nil {
		return nil, fmt.Errorf("latest usage snapshots: %w", err)
	}
	defer rows.Close()
	var out []UsageSnapshot
	for rows.Next() {
		u, err := scanUsageSnapshot(rows)
		if err != nil {
			return nil, fmt.Errorf("latest usage snapshots: %w", err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("latest usage snapshots: %w", err)
	}
	return out, nil
}

// LatestUsageSnapshot gives one account's newest usage reading.
//
// It is the reading that was made last, for the same reason as in
// LatestUsageSnapshots: the time says which reading is the newest and
// the id only breaks a tie.
//
// An account that was never read has no reading and no error.
func (s *Store) LatestUsageSnapshot(ctx context.Context, accountID string) (UsageSnapshot, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+usageSnapshotColumns("")+`
		FROM usage_snapshots WHERE account_id=?
		ORDER BY created_at DESC, id DESC LIMIT 1`, accountID)
	u, err := scanUsageSnapshot(row)
	if errors.Is(err, sql.ErrNoRows) {
		return UsageSnapshot{}, nil
	}
	if err != nil {
		return UsageSnapshot{}, fmt.Errorf("latest usage snapshot %s: %w", accountID, err)
	}
	return u, nil
}

// scanUsageSnapshot reads one usage snapshot.
func scanUsageSnapshot(sc scanner) (UsageSnapshot, error) {
	var (
		u                            UsageSnapshot
		granted, consumed, remaining sql.NullFloat64
		expiresAt                    sql.NullInt64
		plan, windowsJSON, balance   sql.NullString
		membershipErr, windowsErr    sql.NullString
		planErr                      sql.NullString
		planUntil                    sql.NullInt64
		createdAt                    int64
	)
	if err := sc.Scan(&u.ID, &u.AccountID, &granted, &consumed, &remaining, &expiresAt,
		&plan, &planUntil, &windowsJSON, &balance, &membershipErr, &windowsErr,
		&planErr, &createdAt); err != nil {
		return UsageSnapshot{}, err
	}
	u.Granted = granted.Float64
	u.Consumed = consumed.Float64
	u.Remaining = remaining.Float64
	u.HasAmounts = granted.Valid && consumed.Valid && remaining.Valid
	u.ExpiresAt = fromMS(expiresAt.Int64)
	u.Plan = plan.String
	u.PlanUntil = fromMS(planUntil.Int64)
	u.WindowsJSON = windowsJSON.String
	u.Balance = balance.String
	u.MembershipError = membershipErr.String
	u.WindowsError = windowsErr.String
	u.PlanError = planErr.String
	u.CreatedAt = fromMS(createdAt)
	return u, nil
}

// PruneUsageSnapshots throws away the readings that are not needed.
//
// The readings that are older than olderThan are thrown away, but the
// newest keepPerAccount readings of every account are kept whatever
// their age: the newest reading is the account's reading, and it is
// what the totals are made of.
//
// The newest is the one that was read last: the time says when the
// reading was made, and the id only breaks a tie between two readings
// of the same millisecond -- the same rule as LatestUsageSnapshots.
// Ranking by the id alone would keep the reading that was written last,
// which is not always the newest one: a reading stamps its time when it
// starts and writes it down when it ends.
//
// A keepPerAccount of zero or less keeps nothing: every reading that
// is older than olderThan goes.
func (s *Store) PruneUsageSnapshots(ctx context.Context, olderThan time.Time, keepPerAccount int) error {
	if keepPerAccount < 0 {
		keepPerAccount = 0
	}
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM usage_snapshots WHERE id IN (
			SELECT id FROM (
				SELECT id, created_at,
					ROW_NUMBER() OVER (PARTITION BY account_id ORDER BY created_at DESC, id DESC) AS rn
				FROM usage_snapshots
			) WHERE rn > ? AND created_at < ?)`,
		keepPerAccount, ms(olderThan))
	if err != nil {
		return fmt.Errorf("prune the usage snapshots: %w", err)
	}
	return nil
}

// SetExhausted marks an account's window as spent until a time.
func (s *Store) SetExhausted(ctx context.Context, accountID, windowKey string, until time.Time, reason string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO exhausted_windows (account_id, window_key, until, reason)
		VALUES (?,?,?,?)
		ON CONFLICT(account_id, window_key) DO UPDATE SET
			until=excluded.until, reason=excluded.reason`,
		accountID, windowKey, ms(until), reason)
	if err != nil {
		return fmt.Errorf("set exhausted: %w", err)
	}
	return nil
}

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

// ListExhausted gives every window that is spent.
//
// The windows that have come back are in here as well: a row is there
// until it is pruned, and the row says when the window comes back.
// The reader decides which of them still count.
func (s *Store) ListExhausted(ctx context.Context) ([]Exhausted, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT account_id, window_key, until, reason FROM exhausted_windows ORDER BY account_id`)
	if err != nil {
		return nil, fmt.Errorf("list exhausted: %w", err)
	}
	defer rows.Close()
	var out []Exhausted
	for rows.Next() {
		var (
			e      Exhausted
			until  int64
			reason sql.NullString
		)
		if err := rows.Scan(&e.AccountID, &e.WindowKey, &until, &reason); err != nil {
			return nil, fmt.Errorf("list exhausted: %w", err)
		}
		e.Until = fromMS(until)
		e.Reason = reason.String
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list exhausted: %w", err)
	}
	return out, nil
}

// PruneExpiredWindows throws away the windows that have come back.
//
// A window that has run out is not a window that is spent, and it is
// of no use to anybody.
//
// 它没有地方调用，而且是有意的：过了时刻的标记不是垃圾，它是「这个账
// 号该去读一次」的信号（规格 §4.5 的恢复点 A）。删掉它，那个账号就再
// 也等不到那一趟读 —— 它的数会一直停在用完的那一刻。
//
// 所以要收拾过期标记的地方，是读额度的名单（`internal/usage` 那一套）：
// 它读完了，标记自然就不欠了。
func (s *Store) PruneExpiredWindows(ctx context.Context, now time.Time) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM exhausted_windows WHERE until <= ?", ms(now))
	if err != nil {
		return fmt.Errorf("prune the expired windows: %w", err)
	}
	return nil
}

// SetEgressHealth writes a proxy's last check.
func (s *Store) SetEgressHealth(ctx context.Context, url string, ok bool, errMsg string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO egress_health (url, ok, error, checked_at) VALUES (?,?,?,?)
		ON CONFLICT(url) DO UPDATE SET
			ok=excluded.ok, error=excluded.error, checked_at=excluded.checked_at`,
		url, boolInt(ok), errMsg, ms(s.now()))
	if err != nil {
		return fmt.Errorf("set egress health: %w", err)
	}
	return nil
}

// EgressHealth gives every proxy's last check.
func (s *Store) EgressHealth(ctx context.Context) ([]EgressHealth, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT url, ok, error, checked_at FROM egress_health ORDER BY url")
	if err != nil {
		return nil, fmt.Errorf("egress health: %w", err)
	}
	defer rows.Close()
	var out []EgressHealth
	for rows.Next() {
		var (
			h         EgressHealth
			ok        int
			errMsg    sql.NullString
			checkedAt int64
		)
		if err := rows.Scan(&h.URL, &ok, &errMsg, &checkedAt); err != nil {
			return nil, fmt.Errorf("egress health: %w", err)
		}
		h.OK = ok != 0
		h.Error = errMsg.String
		h.CheckedAt = fromMS(checkedAt)
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("egress health: %w", err)
	}
	return out, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
