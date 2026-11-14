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
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

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

// Store is the database.
type Store struct {
	db *sql.DB

	// Clock is the store's clock.
	//
	// It is there so that a test can say what time it is. A store
	// with no clock uses the wall clock.
	Clock func() time.Time
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
	ID         string
	Name       string
	UserID     string
	Email      string
	Status     string
	ProxyGroup string
	ProxyURL   string
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
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create data dir: %w", err)
		}
	}
	db, err := sql.Open("sqlite", dsn(path))
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
	return &Store{db: db, Clock: time.Now}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

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
func dsn(path string) string {
	p := filepath.ToSlash(path)
	prefix := "file:"
	if strings.HasPrefix(p, "//") {
		// A path that begins with two slashes is a network path, and
		// the two slashes after "file:" are the empty authority that
		// says so.
		prefix = "file://"
	}
	return prefix + escapePath(p) +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(1)&_txlock=immediate"
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
const schemaVersion = 4

// migrations are the steps, in order.
//
// The step at index n brings a file at version n to version n+1.
var migrations = []func(ctx context.Context, conn *sql.Conn) error{
	migrateMake,
	migrateConform,
	migratePlanError,
	migrateBatchImport,
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
		INSERT INTO accounts (id, name, user_id, email, status, proxy_group, proxy_url,
			last_error, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)`,
		a.ID, a.Name, a.UserID, a.Email, a.Status, a.ProxyGroup, a.ProxyURL,
		a.LastError, ms(s.stamp(a.CreatedAt)), ms(s.stamp(a.UpdatedAt)))
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
		UPDATE accounts SET name=?, user_id=?, email=?, status=?, proxy_group=?, proxy_url=?,
			last_error=?, updated_at=?
		WHERE id=?`,
		a.Name, a.UserID, a.Email, a.Status, a.ProxyGroup, a.ProxyURL,
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

// SetAccountProxy writes an account's outlet.
//
// It is narrow like UpdateAccountStatus: the group and the address
// are the only things it writes, so a caller that changes the outlet
// does not have to read the account first.
func (s *Store) SetAccountProxy(ctx context.Context, id, group, url string, updatedAt time.Time) error {
	return s.setAccountProxy(ctx, s.db, id, group, url, updatedAt)
}

func (s *Store) setAccountProxy(ctx context.Context, db execer, id, group, url string, updatedAt time.Time) error {
	res, err := db.ExecContext(ctx, `
		UPDATE accounts SET proxy_group=?, proxy_url=?, updated_at=? WHERE id=?`,
		group, url, ms(s.stamp(updatedAt)), id)
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
// It is narrow like SetAccountProxy: the name is the only thing it
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
func (s *Store) SetAccountIdentity(ctx context.Context, id, email, userID string, updatedAt time.Time) error {
	return s.setAccountIdentity(ctx, s.db, id, email, userID, updatedAt)
}

func (s *Store) setAccountIdentity(ctx context.Context, db execer, id, email, userID string, updatedAt time.Time) error {
	res, err := db.ExecContext(ctx, `
		UPDATE accounts SET email=?, user_id=?, updated_at=? WHERE id=?`,
		email, userID, ms(s.stamp(updatedAt)), id)
	if err != nil {
		return fmt.Errorf("set the identity of the account %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("set the identity of the account %s: %w", id, ErrNotFound)
	}
	return nil
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
	// CreateAccount inserts an account.
	CreateAccount(ctx context.Context, a Account) error
	// SaveToken writes an account's token.
	SaveToken(ctx context.Context, t Token) error
	// SetAccountIdentity writes an account's address and user id.
	SetAccountIdentity(ctx context.Context, id, email, userID string, updatedAt time.Time) error
	// SetAccountName writes the account's name.
	SetAccountName(ctx context.Context, id, name string, updatedAt time.Time) error
	// SetAccountProxy writes the account's outlet.
	SetAccountProxy(ctx context.Context, id, group, url string, updatedAt time.Time) error
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin a transaction: %w", err)
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

func (t *sqlTx) SetAccountProxy(ctx context.Context, id, group, url string, updatedAt time.Time) error {
	return t.s.setAccountProxy(ctx, t.tx, id, group, url, updatedAt)
}

func (t *sqlTx) UpdateAccountStatus(ctx context.Context, id, status, lastError string, updatedAt time.Time) error {
	return t.s.updateAccountStatus(ctx, t.tx, id, status, lastError, updatedAt)
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
		SELECT id, name, user_id, email, status, proxy_group, proxy_url,
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
		SELECT id, name, user_id, email, status, proxy_group, proxy_url,
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
		SELECT id, name, user_id, email, status, proxy_group, proxy_url,
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
		SELECT a.id, a.name, a.user_id, a.email, a.status, a.proxy_group, a.proxy_url,
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

func scanAccount(sc scanner) (Account, error) {
	var (
		a                     Account
		userID, email, pg, pu sql.NullString
		lastErr               sql.NullString
		createdAt, updatedAt  int64
	)
	if err := sc.Scan(&a.ID, &a.Name, &userID, &email, &a.Status, &pg, &pu,
		&lastErr, &createdAt, &updatedAt); err != nil {
		return Account{}, err
	}
	a.UserID = userID.String
	a.Email = email.String
	a.ProxyGroup = pg.String
	a.ProxyURL = pu.String
	a.LastError = lastErr.String
	a.CreatedAt = fromMS(createdAt)
	a.UpdatedAt = fromMS(updatedAt)
	return a, nil
}

// SaveToken writes an account's token.
func (s *Store) SaveToken(ctx context.Context, t Token) error {
	return s.saveToken(ctx, s.db, t)
}

func (s *Store) saveToken(ctx context.Context, db execer, t Token) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO tokens (account_id, access, refresh, expires_at, subject, updated_at)
		VALUES (?,?,?,?,?,?)
		ON CONFLICT(account_id) DO UPDATE SET
			access=excluded.access, refresh=excluded.refresh,
			expires_at=excluded.expires_at, subject=excluded.subject,
			updated_at=excluded.updated_at`,
		t.AccountID, t.Access, t.Refresh, ms(t.ExpiresAt), t.Subject, ms(s.stamp(t.UpdatedAt)))
	if err != nil {
		return fmt.Errorf("save token: %w", err)
	}
	return nil
}

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

// LatestUsageSnapshots gives each account's newest usage reading.
//
// The newest is the row with the greatest id. Two readings of one
// account can share a millisecond, and the timestamp alone would then
// name two rows -- and both would be summed, so the account's credits
// would be counted twice. The id grows with every row written, so it
// tells the readings apart, and there is one row per account, always.
func (s *Store) LatestUsageSnapshots(ctx context.Context) ([]UsageSnapshot, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+usageSnapshotColumns("s")+`
		FROM usage_snapshots s
		JOIN (SELECT account_id, MAX(id) AS mid FROM usage_snapshots GROUP BY account_id)
			m ON m.account_id = s.account_id AND m.mid = s.id
		ORDER BY s.account_id`)
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
// It is the row with the greatest id, for the same reason as in
// LatestUsageSnapshots: two readings of one account can share a
// millisecond.
//
// An account that was never read has no reading and no error.
func (s *Store) LatestUsageSnapshot(ctx context.Context, accountID string) (UsageSnapshot, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+usageSnapshotColumns("")+`
		FROM usage_snapshots WHERE account_id=? ORDER BY id DESC LIMIT 1`, accountID)
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
					ROW_NUMBER() OVER (PARTITION BY account_id ORDER BY id DESC) AS rn
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
func (s *Store) ClearExhausted(ctx context.Context, accountID, windowKey string) error {
	_, err := s.db.ExecContext(ctx,
		"DELETE FROM exhausted_windows WHERE account_id=? AND window_key=?", accountID, windowKey)
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
func (s *Store) PruneExpiredWindows(ctx context.Context, now time.Time) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM exhausted_windows WHERE until <= ?", ms(now))
	if err != nil {
		return fmt.Errorf("prune the expired windows: %w", err)
	}
	return nil
}

// BindSession points a session at an account.
//
// The session's upstream id is left alone: it belongs to the
// conversation and not to the account it is bound to.
func (s *Store) BindSession(ctx context.Context, sessionKey, accountID string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sessions (session_key, account_id, updated_at) VALUES (?,?,?)
		ON CONFLICT(session_key) DO UPDATE SET
			account_id=excluded.account_id, updated_at=excluded.updated_at`,
		sessionKey, accountID, ms(s.now()))
	if err != nil {
		return fmt.Errorf("bind session: %w", err)
	}
	return nil
}

// SessionAccount gives the account a session is bound to.
//
// A session that was never bound has no account and no error.
func (s *Store) SessionAccount(ctx context.Context, sessionKey string) (string, bool, error) {
	var id sql.NullString
	err := s.db.QueryRowContext(ctx,
		"SELECT account_id FROM sessions WHERE session_key=?", sessionKey).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("session account: %w", err)
	}
	if !id.Valid || id.String == "" {
		return "", false, nil
	}
	return id.String, true, nil
}

// UpstreamID gives the session's upstream id, making one if the
// session has none.
//
// The id is random: it is not derived from the session key, so the
// upstream never sees a hash of the words in the request. It is also
// kept: the same session has the same upstream id every time it is
// asked for.
//
// The key is what decides which id a conversation gets, and two
// conversations that arrive under the same key are one conversation as
// far as this store can tell. Telling them apart is the caller's job:
// it has to give them different keys.
//
// The session does not have to be bound to an account yet: a
// conversation has an upstream id before it has an account.
func (s *Store) UpstreamID(ctx context.Context, key string) (string, error) {
	id, ok, err := s.upstreamIDOf(ctx, key)
	if err != nil {
		return "", err
	}
	if ok {
		if id != "" {
			return id, nil
		}
		// The session is there but has no id yet: make one and write
		// it down.
		newID, err := randomID()
		if err != nil {
			return "", err
		}
		res, err := s.db.ExecContext(ctx, `
			UPDATE sessions SET upstream_id=?
			WHERE session_key=? AND (upstream_id IS NULL OR upstream_id='')`, newID, key)
		if err != nil {
			return "", fmt.Errorf("set the upstream id of the session %s: %w", key, err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			return newID, nil
		}
		// Somebody else wrote one first, and theirs is the one.
		id, _, err := s.upstreamIDOf(ctx, key)
		if err != nil {
			return "", err
		}
		if id == "" {
			return "", fmt.Errorf("the session %s has no upstream id", key)
		}
		return id, nil
	}
	// The session is not there yet: make it with an id of its own.
	newID, err := randomID()
	if err != nil {
		return "", err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO sessions (session_key, account_id, upstream_id, updated_at)
		VALUES (?, NULL, ?, ?)
		ON CONFLICT(session_key) DO UPDATE SET
			upstream_id=COALESCE(sessions.upstream_id, excluded.upstream_id)`,
		key, newID, ms(s.now()))
	if err != nil {
		return "", fmt.Errorf("make the session %s: %w", key, err)
	}
	id, _, err = s.upstreamIDOf(ctx, key)
	if err != nil {
		return "", err
	}
	if id == "" {
		return "", fmt.Errorf("the session %s has no upstream id", key)
	}
	return id, nil
}

// upstreamIDOf gives the upstream id of a session.
//
// The second value says whether the session is there at all.
func (s *Store) upstreamIDOf(ctx context.Context, key string) (string, bool, error) {
	var id sql.NullString
	err := s.db.QueryRowContext(ctx,
		"SELECT upstream_id FROM sessions WHERE session_key=?", key).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read the upstream id of the session %s: %w", key, err)
	}
	return id.String, true, nil
}

// randomID gives an id that is not like any other.
//
// The id is random and not made from the session's key: two
// conversations that begin with the same words must not end up with
// the same upstream id.
func randomID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("make an id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
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

// DayKey is the day a check-in is counted for.
//
// The day is the site's day, so that the day rolls over at the same
// hour for every account, whatever the machine's own timezone is.
func DayKey(t time.Time) string {
	return t.In(checkinZone).Format("2006-01-02")
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
