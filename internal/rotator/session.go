package rotator

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// The headers a client can name its session with, in the order they
// are looked for.
var sessionHeaders = []string{
	"X-Session-Id",
	"X-Conversation-Id",
	"X-Session-Id-Codex",
	"Session-Id",
}

// Session is what a request's session is made of.
//
// The two things it holds are different on purpose. The key says which
// account the conversation belongs to, and it is ours alone. The
// upstream id says which conversation it is, and MiniMax sees it.
type Session struct {
	// Key is the account affinity key. Always a hash.
	Key string
	// UpstreamID is the session id sent to MiniMax.
	UpstreamID string
}

// SessionOf reads the session out of a request.
//
// The client's own session id is used when it has one. Otherwise the
// session is a hash of the system prompt and the first user message:
// the first message of a conversation does not change while the
// conversation goes on, so the hash holds the conversation to one
// account.
//
// The key is a hash either way. A client's own string is not ours to
// keep, and it has no business becoming a database key as it came.
//
// The upstream id is the client's own when it sent one: it names the
// conversation, not the account, so it stays the same when the
// conversation moves to another account.
//
// When there is neither a header nor a message to hash, the session is
// empty and the request is handled by round robin.
//
// The upstream id of a conversation that brought no header of its own
// is the hash of its first message. Two users whose first messages
// happen to be the same therefore share one upstream id, and one
// account: the upstream sees one conversation as two, and one user's
// messages can turn up in another user's conversation. A random id per
// conversation would not, but the id has to be kept from one turn to
// the next and there is nowhere to keep it: the sessions table holds
// the key and the account and nothing else. It needs a column of its
// own before this can be done.
func SessionOf(headers map[string][]string, system, firstUser string) Session {
	for _, h := range sessionHeaders {
		for _, v := range headers[h] {
			if s := strings.TrimSpace(v); s != "" {
				return Session{Key: "h:" + digest("hdr:"+s), UpstreamID: s}
			}
		}
	}
	if strings.TrimSpace(system) == "" && strings.TrimSpace(firstUser) == "" {
		return Session{}
	}
	d := digest(system + "\x00" + firstUser)
	return Session{Key: "m:" + d, UpstreamID: d}
}

// digest is the short hash a session key is made of.
func digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:32]
}
