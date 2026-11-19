package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
)

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
// 一条语句：写完把这一行最后的上游 id 拿回来（RETURNING）。从前是「读一
// 遍、写一遍、再读一遍」—— 那最后一遍是为了知道别人有没有先写一个 id。
// RETURNING 是同一件事，而且省掉两趟。
//
// 那一行已经有了 id 的时候：写的还是那一行，id 不换（先到先得）。
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
	newID, err := randomID()
	if err != nil {
		return "", err
	}
	var id string
	err = s.db.QueryRowContext(ctx, `
		INSERT INTO sessions (session_key, account_id, upstream_id, updated_at)
		VALUES (?, NULL, ?, ?)
		ON CONFLICT(session_key) DO UPDATE SET
			upstream_id=CASE WHEN sessions.upstream_id IS NULL OR sessions.upstream_id=''
			                  THEN excluded.upstream_id ELSE sessions.upstream_id END
		RETURNING upstream_id`, key, newID, ms(s.now())).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("make the session %s: %w", key, err)
	}
	if id == "" {
		return "", fmt.Errorf("the session %s has no upstream id", key)
	}
	return id, nil
}

// SessionState is what the store knows about a session's row.
//
// 「没读过」跟「读过、那一行是空的」是两件事：分不开的话，轮转会以为每一段
// 对话都没有绑定的号，每一次都重新绑。
type SessionState struct {
	// Known says whether the row was read at all.
	Known bool
	// AccountID is the account the session is bound to. Empty when it is
	// bound to nobody.
	AccountID string
	// UpstreamID is the id the upstream knows this conversation by.
	UpstreamID string
}

// SessionState gives the session's row in one read.
//
// 那一行的两列（钉在哪个号上、上游的 id）本来是分开读的：分开读就是两次往
// 返，而每个请求都要问这一行。
func (s *Store) SessionState(ctx context.Context, sessionKey string) (SessionState, error) {
	var (
		account  sql.NullString
		upstream sql.NullString
	)
	err := s.db.QueryRowContext(ctx,
		"SELECT account_id, upstream_id FROM sessions WHERE session_key=?", sessionKey).
		Scan(&account, &upstream)
	if errors.Is(err, sql.ErrNoRows) {
		// 那一行还没有：读过了，是空的。
		return SessionState{Known: true}, nil
	}
	if err != nil {
		return SessionState{}, fmt.Errorf("the session row: %w", err)
	}
	return SessionState{
		Known:      true,
		AccountID:  account.String,
		UpstreamID: upstream.String,
	}, nil
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
