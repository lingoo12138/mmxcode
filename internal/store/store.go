// Package store keeps the accounts, their tokens, the check-in history
// and the usage snapshots in one SQLite file.
//
// The file is opened with WAL on, so a reader (the WebUI) and a writer
// (the scheduler) do not block each other.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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

// Store is the database.
type Store struct {
	db *sql.DB
}

// Account is one MiniMax account.
type Account struct {
	ID             string
	Name           string
	UserID         string
	Email          string
	Status         string
	ProxyGroup     string
	ProxyURL       string
	PendingPersist bool
	ExhaustedUntil time.Time
	LastError      string
	CreatedAt      time.Time
	UpdatedAt      time.Time
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
	ID          int64
	AccountID   string
	Granted     float64
	Consumed    float64
	Remaining   float64
	HasAmounts  bool
	ExpiresAt   time.Time
	Plan        string
	PlanUntil   time.Time
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
	CreatedAt    time.Time
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

// Open opens the database at path, creating it and its directory if
// they are not there.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create data dir: %w", err)
		}
	}
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// One writer at a time; SQLite serializes them anyway and this keeps
	// the Go side from piling up.
	db.SetMaxOpenConns(1)
	// The database is opened lazily, so a path that cannot be used
	// would only show up at the first query. Open it now: the caller
	// wants to know now.
	if err := db.PingContext(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Migrate applies the schema.
func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	// A file that was made before these columns were added does not
	// get them from CREATE TABLE IF NOT EXISTS, so they are added
	// here.
	for _, col := range []struct{ table, name, decl string }{
		{"usage_snapshots", "membership_error", "TEXT"},
		{"usage_snapshots", "windows_error", "TEXT"},
	} {
		if err := s.addColumn(ctx, col.table, col.name, col.decl); err != nil {
			return err
		}
	}
	return nil
}

// addColumn adds a column to a table, unless the table already has it.
func (s *Store) addColumn(ctx context.Context, table, name, decl string) error {
	have, err := s.columnExists(ctx, table, name)
	if err != nil {
		return err
	}
	if have {
		return nil
	}
	if _, err := s.db.ExecContext(ctx,
		fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, name, decl)); err != nil {
		return fmt.Errorf("migrate: add column %s.%s: %w", table, name, err)
	}
	return nil
}

// columnExists says whether a table has a column.
func (s *Store) columnExists(ctx context.Context, table, name string) (bool, error) {
	rows, err := s.db.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return false, fmt.Errorf("migrate: %w", err)
	}
	defer rows.Close()
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
			return false, fmt.Errorf("migrate: %w", err)
		}
		if strings.EqualFold(colName, name) {
			return true, nil
		}
	}
	return false, rows.Err()
}

func ms(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func fromMS(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.UnixMilli(v)
}

// CreateAccount inserts an account.
func (s *Store) CreateAccount(ctx context.Context, a Account) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO accounts (id, name, user_id, email, status, proxy_group, proxy_url,
			pending_persist, exhausted_until, last_error, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		a.ID, a.Name, a.UserID, a.Email, a.Status, a.ProxyGroup, a.ProxyURL,
		boolInt(a.PendingPersist), ms(a.ExhaustedUntil), a.LastError, ms(a.CreatedAt), ms(a.UpdatedAt))
	if err != nil {
		return fmt.Errorf("create account: %w", err)
	}
	return nil
}

// UpdateAccount writes an account's fields.
//
// An account that is not there is an error, not a quiet success: the
// caller believes it wrote something.
func (s *Store) UpdateAccount(ctx context.Context, a Account) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE accounts SET name=?, user_id=?, email=?, status=?, proxy_group=?, proxy_url=?,
			pending_persist=?, exhausted_until=?, last_error=?, updated_at=?
		WHERE id=?`,
		a.Name, a.UserID, a.Email, a.Status, a.ProxyGroup, a.ProxyURL,
		boolInt(a.PendingPersist), ms(a.ExhaustedUntil), a.LastError, ms(a.UpdatedAt), a.ID)
	if err != nil {
		return fmt.Errorf("update account: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("update account %s: no such account", a.ID)
	}
	return nil
}

// DeleteAccount removes an account and everything under it.
//
// The deletes belong together: half of them done is an account with
// its tokens gone and its history still there. So they are one
// transaction.
func (s *Store) DeleteAccount(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("delete account: %w", err)
	}
	for _, q := range []string{
		"DELETE FROM tokens WHERE account_id=?",
		"DELETE FROM exhausted_windows WHERE account_id=?",
		"DELETE FROM checkins WHERE account_id=?",
		"DELETE FROM usage_snapshots WHERE account_id=?",
		"DELETE FROM sessions WHERE account_id=?",
		"DELETE FROM accounts WHERE id=?",
	} {
		if _, err := tx.ExecContext(ctx, q, id); err != nil {
			tx.Rollback()
			return fmt.Errorf("delete account: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("delete account: %w", err)
	}
	return nil
}

// ListAccounts gives every account, oldest first.
func (s *Store) ListAccounts(ctx context.Context) ([]Account, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, user_id, email, status, proxy_group, proxy_url,
			pending_persist, exhausted_until, last_error, created_at, updated_at
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
	return out, rows.Err()
}

// GetAccount gives one account.
func (s *Store) GetAccount(ctx context.Context, id string) (Account, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, name, user_id, email, status, proxy_group, proxy_url,
			pending_persist, exhausted_until, last_error, created_at, updated_at
		FROM accounts WHERE id=?`, id)
	a, err := scanAccount(row)
	if err != nil {
		return Account{}, fmt.Errorf("get account %s: %w", id, err)
	}
	return a, nil
}

type scanner interface{ Scan(dest ...any) error }

func scanAccount(sc scanner) (Account, error) {
	var (
		a                     Account
		userID, email, pg, pu sql.NullString
		lastErr               sql.NullString
		exhausted             sql.NullInt64
		pending               int
		createdAt, updatedAt  int64
	)
	if err := sc.Scan(&a.ID, &a.Name, &userID, &email, &a.Status, &pg, &pu,
		&pending, &exhausted, &lastErr, &createdAt, &updatedAt); err != nil {
		return Account{}, err
	}
	a.UserID = userID.String
	a.Email = email.String
	a.ProxyGroup = pg.String
	a.ProxyURL = pu.String
	a.LastError = lastErr.String
	a.PendingPersist = pending != 0
	a.ExhaustedUntil = fromMS(exhausted.Int64)
	a.CreatedAt = fromMS(createdAt)
	a.UpdatedAt = fromMS(updatedAt)
	return a, nil
}

// SaveToken writes an account's token.
func (s *Store) SaveToken(ctx context.Context, t Token) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO tokens (account_id, access, refresh, expires_at, subject, updated_at)
		VALUES (?,?,?,?,?,?)
		ON CONFLICT(account_id) DO UPDATE SET
			access=excluded.access, refresh=excluded.refresh,
			expires_at=excluded.expires_at, subject=excluded.subject,
			updated_at=excluded.updated_at`,
		t.AccountID, t.Access, t.Refresh, ms(t.ExpiresAt), t.Subject, ms(t.UpdatedAt))
	if err != nil {
		return fmt.Errorf("save token: %w", err)
	}
	return nil
}

// GetToken gives an account's token.
func (s *Store) GetToken(ctx context.Context, accountID string) (Token, error) {
	var (
		t         Token
		expiresAt int64
		updatedAt int64
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT account_id, access, refresh, expires_at, subject, updated_at
		FROM tokens WHERE account_id=?`, accountID).
		Scan(&t.AccountID, &t.Access, &t.Refresh, &expiresAt, &t.Subject, &updatedAt)
	if err != nil {
		return Token{}, fmt.Errorf("get token %s: %w", accountID, err)
	}
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
		c.AccountID, c.Day, c.Outcome, c.Credit, c.Streak, c.Message, ms(c.CreatedAt))
	if err != nil {
		return fmt.Errorf("save checkin: %w", err)
	}
	return nil
}

// ListCheckins gives an account's check-ins, newest first.
func (s *Store) ListCheckins(ctx context.Context, accountID string, limit int) ([]Checkin, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, account_id, day, outcome, credit, streak, message, created_at
		FROM checkins WHERE account_id=? ORDER BY day DESC LIMIT ?`, accountID, limit)
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
			return nil, err
		}
		c.Credit = credit.Float64
		c.Streak = int(streak.Int64)
		c.Message = message.String
		c.CreatedAt = fromMS(createdAt)
		out = append(out, c)
	}
	return out, rows.Err()
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
func (s *Store) SaveUsageSnapshot(ctx context.Context, u UsageSnapshot) error {
	var granted, consumed, remaining any
	if u.HasAmounts {
		granted, consumed, remaining = u.Granted, u.Consumed, u.Remaining
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO usage_snapshots (account_id, granted, consumed, remaining,
			expires_at, plan, plan_until, windows_json, balance,
			membership_error, windows_error, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		u.AccountID, granted, consumed, remaining, ms(u.ExpiresAt),
		u.Plan, ms(u.PlanUntil), u.WindowsJSON, u.Balance,
		u.MembershipError, u.WindowsError, ms(u.CreatedAt))
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
		"membership_error", "windows_error", "created_at",
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
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
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
	if err == sql.ErrNoRows {
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
		planUntil                    sql.NullInt64
		createdAt                    int64
	)
	if err := sc.Scan(&u.ID, &u.AccountID, &granted, &consumed, &remaining, &expiresAt,
		&plan, &planUntil, &windowsJSON, &balance, &membershipErr, &windowsErr,
		&createdAt); err != nil {
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
	u.CreatedAt = fromMS(createdAt)
	return u, nil
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
			return nil, err
		}
		e.Until = fromMS(until)
		e.Reason = reason.String
		out = append(out, e)
	}
	return out, rows.Err()
}

// BindSession points a session at an account.
func (s *Store) BindSession(ctx context.Context, sessionKey, accountID string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sessions (session_key, account_id, updated_at) VALUES (?,?,?)
		ON CONFLICT(session_key) DO UPDATE SET
			account_id=excluded.account_id, updated_at=excluded.updated_at`,
		sessionKey, accountID, time.Now().UnixMilli())
	if err != nil {
		return fmt.Errorf("bind session: %w", err)
	}
	return nil
}

// SessionAccount gives the account a session is bound to.
func (s *Store) SessionAccount(ctx context.Context, sessionKey string) (string, bool, error) {
	var id string
	err := s.db.QueryRowContext(ctx,
		"SELECT account_id FROM sessions WHERE session_key=?", sessionKey).Scan(&id)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("session account: %w", err)
	}
	return id, true, nil
}

// SetEgressHealth writes a proxy's last check.
func (s *Store) SetEgressHealth(ctx context.Context, url string, ok bool, errMsg string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO egress_health (url, ok, error, checked_at) VALUES (?,?,?,?)
		ON CONFLICT(url) DO UPDATE SET
			ok=excluded.ok, error=excluded.error, checked_at=excluded.checked_at`,
		url, boolInt(ok), errMsg, time.Now().UnixMilli())
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
			return nil, err
		}
		h.OK = ok != 0
		h.Error = errMsg.String
		h.CheckedAt = fromMS(checkedAt)
		out = append(out, h)
	}
	return out, rows.Err()
}

// SetSetting writes a setting.
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO settings (key, value) VALUES (?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	if err != nil {
		return fmt.Errorf("set setting: %w", err)
	}
	return nil
}

// Setting reads a setting.
func (s *Store) Setting(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=?", key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("setting: %w", err)
	}
	return v, true, nil
}

// MarshalWindows saves a window list as JSON.
func MarshalWindows(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
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
