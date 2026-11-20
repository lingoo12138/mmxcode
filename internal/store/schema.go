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
//
// accounts.proxy_group 是账号指着的出口组：组名（空 = 跟着全局组走）。
// 这一列一直都在库里 —— 删列要重建整张表，而那张表上有触发器、有一百多
// 个号在用。
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
  updated_at   INTEGER NOT NULL,
  password    TEXT
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
			nullable("password"),
		},
	},
	{
		name: "import_runs",
		body: `(
  id          TEXT PRIMARY KEY,
  source      TEXT NOT NULL,
  started_at  INTEGER NOT NULL,
  finished_at INTEGER,
  total       INTEGER NOT NULL DEFAULT 0,
  ok          INTEGER NOT NULL DEFAULT 0,
  failed      INTEGER NOT NULL DEFAULT 0,
  results     TEXT
)`,
		copy: []copyColumn{
			notNull("id", "''"),
			notNull("source", "''"),
			notNull("started_at", "0"),
			nullable("finished_at"),
			notNull("total", "0"),
			notNull("ok", "0"),
			notNull("failed", "0"),
			nullable("results"),
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
}

// The indexes.
//
// 索引是照着「怎么问」建的，不是照着「表长什么样」建的：每一条都对应
// 一条被量出来的热查询。
//
//   - checkins(day, account_id, created_at, credit)：今天谁签到、给了多
//     少积分（DayCheckins）、今天谁还没签（AccountsWithoutCheckin）。
//     旧的那条 idx_checkins_day 只盖住 day 一列，每一行都要回表。
//   - checkins(account_id, created_at DESC, id DESC)：这个号最近签到的
//     时候（CheckinTimes）。旧的 UNIQUE(account_id, day) 按日子排，不按
//     时刻排。
//   - usage_snapshots(account_id, created_at DESC, id DESC)：每个号最新
//     的一条读数（LatestUsageSnapshots）。旧的 idx_usage_account 少了
//     id，排到最后一条的时候还要再排一次。
//   - sessions(account_id)：这个号绑着哪些对话（PoolCandidates 的排序）。
//
// 别的都试过，没有好处（1000 个号 / 36 万行签到的库上量的，每条量三遍
// 取最快的那一遍）：
//
//   - accounts(status, created_at, id)：填池那条查询 6.15 ms 到 6.79 ms
//     （顺序扫变成了按索引随机取行）。
//   - sessions(account_id, updated_at)：写会话（一批 100 个 key）0.77 ms 到
//     0.82 ms —— 请求的路上一分都不能慢。
//   - sessions(updated_at)：数活跃会话 0.196 ms 到 0.247 ms，而它省下的
//     那点时间在一条每 20 秒才跑一次的路上。
//
// 每一条都要在每次写的时候多维护一份，所以每一条索引都对应一条具体的、
// 被量出来的查询。
//
// 被新索引完全盖住的旧索引在这里删掉：留着就是每次写都多维护一份。
var indexStatements = []string{
	"CREATE INDEX IF NOT EXISTS idx_checkins_day_account ON checkins(day, account_id, created_at, credit)",
	"CREATE INDEX IF NOT EXISTS idx_checkins_account_created ON checkins(account_id, created_at DESC, id DESC)",
	"CREATE INDEX IF NOT EXISTS idx_usage_account_created ON usage_snapshots(account_id, created_at DESC, id DESC)",
	"CREATE INDEX IF NOT EXISTS idx_sessions_account ON sessions(account_id)",
	"DROP INDEX IF EXISTS idx_checkins_day",
	"DROP INDEX IF EXISTS idx_usage_account",
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
