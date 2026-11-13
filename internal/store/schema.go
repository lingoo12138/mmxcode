package store

// schema is the whole database. It is applied in one go, so a fresh
// file and an existing one end up the same.
const schema = `
CREATE TABLE IF NOT EXISTS accounts (
  id               TEXT PRIMARY KEY,
  name             TEXT NOT NULL,
  user_id          TEXT,
  email            TEXT,
  status           TEXT NOT NULL,
  proxy_group      TEXT,
  proxy_url        TEXT,
  pending_persist  INTEGER NOT NULL DEFAULT 0,
  exhausted_until  INTEGER,
  last_error       TEXT,
  created_at       INTEGER NOT NULL,
  updated_at       INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS tokens (
  account_id  TEXT PRIMARY KEY,
  access      TEXT NOT NULL,
  refresh     TEXT NOT NULL,
  expires_at  INTEGER NOT NULL,
  subject     TEXT,
  updated_at  INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS exhausted_windows (
  account_id TEXT NOT NULL,
  window_key TEXT NOT NULL,
  until      INTEGER NOT NULL,
  reason     TEXT,
  PRIMARY KEY (account_id, window_key)
);

CREATE TABLE IF NOT EXISTS sessions (
  session_key TEXT PRIMARY KEY,
  account_id  TEXT NOT NULL,
  updated_at  INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS checkins (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  account_id TEXT NOT NULL,
  day        TEXT NOT NULL,
  outcome    TEXT NOT NULL,
  credit     REAL,
  streak     INTEGER,
  message    TEXT,
  created_at INTEGER NOT NULL,
  UNIQUE (account_id, day)
);

CREATE TABLE IF NOT EXISTS usage_snapshots (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  account_id  TEXT NOT NULL,
  granted     REAL,
  consumed    REAL,
  remaining   REAL,
  expires_at  INTEGER,
  plan        TEXT,
  plan_until  INTEGER,
  windows_json TEXT,
  balance     TEXT,
  created_at  INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS settings (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS egress_health (
  url        TEXT PRIMARY KEY,
  ok         INTEGER NOT NULL,
  error      TEXT,
  checked_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_usage_account ON usage_snapshots(account_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_checkins_account ON checkins(account_id, day DESC);
`
