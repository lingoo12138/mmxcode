package store

// The database's tables.
//
// A table is given as a name and a body, so that the migration can
// both make the table and tell whether the one in the file is the one
// it wants.
//
// The bodies are the shape a file ends up with. A file that was made
// by an older version is brought to this shape by the steps in
// store.go, so that a fresh file and an old one are the same.
//
// The children say which account they belong to, and the account is
// taken with them when it goes: the database does the cascade, so
// there is no list of children anywhere that can fall out of step
// with the schema.
type table struct {
	// name is the table's name.
	name string
	// body is the table: its columns, its constraints and the
	// accounts it belongs to.
	body string
	// copy says how the columns of the new table are filled from
	// the old one.
	copy []copyColumn
}

// copyColumn says how one column of a new table is filled from the
// old one.
type copyColumn struct {
	// name is the column's name.
	name string
	// notNull says the new column cannot be NULL, so a NULL in the
	// old one is replaced by the default.
	notNull bool
	// def is the value a column the old table does not have gets.
	def string
}

// notNull is a column that cannot be NULL.
func notNull(name, def string) copyColumn {
	return copyColumn{name: name, notNull: true, def: def}
}

// nullable is a column that may be NULL.
func nullable(name string) copyColumn {
	return copyColumn{name: name, def: "NULL"}
}

// The tables, in the order they are made.
//
// The accounts come first: the other tables point at them.
var tables = []table{
	{
		name: "accounts",
		body: `(
  id           TEXT PRIMARY KEY,
  name         TEXT NOT NULL,
  user_id      TEXT,
  email        TEXT,
  status       TEXT NOT NULL CHECK (status IN ('active','signed_out','error')),
  proxy_group  TEXT,
  proxy_url    TEXT,
  last_error   TEXT,
  created_at   INTEGER NOT NULL,
  updated_at   INTEGER NOT NULL
)`,
		copy: []copyColumn{
			notNull("id", "''"),
			notNull("name", "''"),
			nullable("user_id"),
			nullable("email"),
			notNull("status", "'active'"),
			nullable("proxy_group"),
			nullable("proxy_url"),
			nullable("last_error"),
			notNull("created_at", "0"),
			notNull("updated_at", "0"),
		},
	},
	{
		name: "tokens",
		body: `(
  account_id  TEXT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
  access      TEXT NOT NULL,
  refresh     TEXT NOT NULL,
  expires_at  INTEGER NOT NULL,
  subject     TEXT NOT NULL DEFAULT '',
  updated_at  INTEGER NOT NULL
)`,
		copy: []copyColumn{
			notNull("account_id", "''"),
			notNull("access", "''"),
			notNull("refresh", "''"),
			notNull("expires_at", "0"),
			notNull("subject", "''"),
			notNull("updated_at", "0"),
		},
	},
	{
		name: "exhausted_windows",
		body: `(
  account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  window_key TEXT NOT NULL,
  until      INTEGER NOT NULL,
  reason     TEXT,
  PRIMARY KEY (account_id, window_key)
)`,
		copy: []copyColumn{
			notNull("account_id", "''"),
			notNull("window_key", "''"),
			notNull("until", "0"),
			nullable("reason"),
		},
	},
	{
		name: "sessions",
		body: `(
  session_key TEXT PRIMARY KEY,
  account_id  TEXT REFERENCES accounts(id) ON DELETE CASCADE,
  upstream_id TEXT UNIQUE,
  updated_at  INTEGER NOT NULL
)`,
		copy: []copyColumn{
			notNull("session_key", "''"),
			nullable("account_id"),
			nullable("upstream_id"),
			notNull("updated_at", "0"),
		},
	},
	{
		name: "checkins",
		body: `(
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  day        TEXT NOT NULL,
  outcome    TEXT NOT NULL,
  credit     REAL NOT NULL DEFAULT 0,
  streak     INTEGER NOT NULL DEFAULT 0,
  message    TEXT,
  created_at INTEGER NOT NULL,
  UNIQUE (account_id, day)
)`,
		copy: []copyColumn{
			notNull("id", "0"),
			notNull("account_id", "''"),
			notNull("day", "''"),
			notNull("outcome", "''"),
			notNull("credit", "0"),
			notNull("streak", "0"),
			nullable("message"),
			notNull("created_at", "0"),
		},
	},
	{
		name: "usage_snapshots",
		body: `(
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  account_id  TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  granted     REAL,
  consumed    REAL,
  remaining   REAL,
  expires_at  INTEGER,
  plan        TEXT,
  plan_until  INTEGER,
  windows_json TEXT,
  balance     TEXT,
  membership_error TEXT,
  windows_error TEXT,
  plan_error TEXT,
  created_at  INTEGER NOT NULL
)`,
		copy: []copyColumn{
			notNull("id", "0"),
			notNull("account_id", "''"),
			nullable("granted"),
			nullable("consumed"),
			nullable("remaining"),
			nullable("expires_at"),
			nullable("plan"),
			nullable("plan_until"),
			nullable("windows_json"),
			nullable("balance"),
			nullable("membership_error"),
			nullable("windows_error"),
			nullable("plan_error"),
			notNull("created_at", "0"),
		},
	},
	{
		name: "egress_health",
		body: `(
  url        TEXT PRIMARY KEY,
  ok         INTEGER NOT NULL,
  error      TEXT,
  checked_at INTEGER NOT NULL
)`,
		copy: []copyColumn{
			notNull("url", "''"),
			notNull("ok", "0"),
			nullable("error"),
			notNull("checked_at", "0"),
		},
	},
}

// The indexes.
//
// The check-ins need none of their own: the unique (account_id, day)
// is already an index over the account and the day.
var indexStatements = []string{
	"CREATE INDEX IF NOT EXISTS idx_usage_account ON usage_snapshots(account_id, created_at DESC)",
	"CREATE INDEX IF NOT EXISTS idx_sessions_account ON sessions(account_id)",
	"DROP INDEX IF EXISTS idx_checkins_account",
}

// The children, with the column that says which account they belong
// to.
//
// A row whose account is not there is refused, and the row goes when
// the account goes.
var childTables = []struct {
	name    string
	account string
}{
	{"tokens", "account_id"},
	{"exhausted_windows", "account_id"},
	{"sessions", "account_id"},
	{"checkins", "account_id"},
	{"usage_snapshots", "account_id"},
}
