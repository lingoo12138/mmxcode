package rotator

import (
	"bytes"
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
//
// The upstream id in here is the derived one: the client's own session
// id when it sent one, and a hash of the first message when it did not.
// It is the fallback, used when the pool keeps no id of its own.
type Session struct {
	// Key is the account affinity key. Always a hash.
	Key string
	// UpstreamID is the derived session id, used when the pool keeps
	// none.
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
// The derived upstream id is the client's own when it sent one: it
// names the conversation, not the account, so it stays the same when
// the conversation moves to another account. It is what the upstream
// is told when the pool keeps no id of its own.
//
// When there is neither a header nor a message to hash, the session is
// empty and the request is handled by round robin.
//
// The derived id of a conversation that brought no header of its own is
// the hash of its first message. Two users whose first messages
// happen to be the same are one session by this design's own
// definition -- the session key is a hash of the same two things -- so
// they share one upstream id, and one upstream conversation, and one
// user's messages can turn up in another user's conversation. A kept
// id does not change that: it is kept per session key, and the two
// conversations have one key. Nothing in the request tells them
// apart, so no server can: the client has to send X-Session-Id. What
// a kept id does fix is that the upstream stops seeing the client's
// own string and a hash of the request's words, and that the mapping
// from the session to the upstream's conversation survives a restart.
func SessionOf(headers map[string][]string, system, firstUser string) Session {
	return SessionOfBytes(headers, []byte(system), []byte(firstUser))
}

// SessionOfBytes is SessionOf with the words as bytes.
//
// 字节是身体的原来那一段，指着就行：抄一份是几十兆的白抄 —— 一句几十兆
// 的话，抄两份就是白涨一百多兆。
func SessionOfBytes(headers map[string][]string, system, firstUser []byte) Session {
	for _, h := range sessionHeaders {
		for _, v := range headers[h] {
			if s := strings.TrimSpace(v); s != "" {
				return Session{Key: "h:" + digest("hdr:"+s), UpstreamID: s}
			}
		}
	}
	if len(bytes.TrimSpace(system)) == 0 && len(bytes.TrimSpace(firstUser)) == 0 {
		return Session{}
	}
	d := digestBytes(system, []byte{0}, firstUser)
	return Session{Key: "m:" + d, UpstreamID: d}
}

// digest is the short hash a session key is made of.
func digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:32]
}

// digestBytes is the short hash of several pieces put together.
//
// 分开写进去，不先拼起来：拼起来是一份，转成字节又是一份 —— 一句几十兆
// 的第一句话，光是这个哈希就白涨两倍。分块写进去，多的那点地方是固定
// 的。
//
// 哈希出来的东西跟拼起来算的**一个字节都不差**：写进去的还是同样的那
// 些字节，同样的顺序。
func digestBytes(parts ...[]byte) string {
	h := sha256.New()
	buf := make([]byte, 32<<10)
	for _, p := range parts {
		for len(p) > 0 {
			n := copy(buf, p)
			h.Write(buf[:n])
			p = p[n:]
		}
	}
	sum := h.Sum(nil)
	return hex.EncodeToString(sum)[:32]
}
