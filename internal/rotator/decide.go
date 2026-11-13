package rotator

import (
	"encoding/json"
	"errors"
	"strings"

	"mmxcode/internal/upstream"
)

// Action is what to do about a failed upstream answer.
type Action int

// The actions.
const (
	// ActionRetrySame waits a little and asks the same account again.
	ActionRetrySame Action = iota
	// ActionFailover marks the account's window spent and asks another
	// account.
	ActionFailover
	// ActionRenew renews the token and asks the same account again.
	ActionRenew
	// ActionGiveUp gives up: the answer goes to the client as it is.
	ActionGiveUp
)

// String is the action's name.
func (a Action) String() string {
	switch a {
	case ActionRetrySame:
		return "retry-same"
	case ActionFailover:
		return "failover"
	case ActionRenew:
		return "renew"
	default:
		return "give-up"
	}
}

// Failure is what went wrong upstream.
type Failure struct {
	// Status is the HTTP status, when there was one.
	Status int
	// Body is the answer's body.
	Body []byte
	// Network says the request never got an answer.
	Network bool
	// OtherSite says the request was redirected to the other site.
	OtherSite bool
	// Refused says the sign-in was refused: the token is stale and
	// the account itself is fine. It is what upstream.ErrSignInRefused
	// says.
	//
	// When it is not set, the body is read to find out: MiniMax's own
	// code 1000048 in the body of a 200 means the same thing.
	Refused bool
	// Message is the message the upstream's business error carried.
	//
	// The words that say "the account is spent" are looked for in it
	// and not in the raw body: a body that says anything about a
	// limit is not a refusal to spend. When it is empty the body is
	// read to find the message.
	Message string
	// Code is the business code the answer carried.
	//
	// When it is zero the body is read to find the code.
	Code int
}

// Decide says what to do about a failure.
//
// The rules come from what the site actually does:
//
//   - a refused sign-in means the token is stale: it is renewed first,
//     and the same account is asked again;
//   - a refusal that names the credits or the quota means the account
//     is spent: another account is tried;
//   - a 429 that does not name the credits is a rate limit, and the
//     same account is asked again after a pause;
//   - a 5xx or a network error is the same: the same account, again;
//   - a 401 means the token is stale: it is renewed first;
//   - a redirect to the other site is neither of these: the account is
//     fine and the network is not, so nothing is changed and no
//     refresh token is spent.
//
// The words that say the account is spent are looked for in the
// upstream's own message and not in the raw body: a body that says
// anything about a limit is not a refusal to spend, and marking the
// account's window spent on one is a mistake that lasts until the
// window comes back.
//
// Decide reads the body when the caller has not filled in the
// message, the code or the refusal. It does no I/O: the body is read,
// not the network.
func Decide(f Failure) Action {
	if f.OtherSite {
		return ActionGiveUp
	}
	if f.Network {
		return ActionRetrySame
	}
	_, msg, refused := f.business()
	if f.Refused || refused {
		// 签到被拒：令牌旧了，续签之后还是这个账号。
		return ActionRenew
	}
	if f.Status == 401 {
		return ActionRenew
	}
	spent := isQuotaMessage(msg)
	switch f.Status {
	case 200:
		// 一个 200 里装着业务错误。只有确认了那是错误才会走到
		// 这里，所以按错误来判。
		if spent {
			return ActionFailover
		}
		return ActionGiveUp
	case 402, 403, 429:
		if spent {
			return ActionFailover
		}
		return ActionRetrySame
	}
	if f.Status >= 500 {
		return ActionRetrySame
	}
	return ActionGiveUp
}

// business gives the business error the answer carried: its code, its
// message, and whether the sign-in was refused.
//
// What the caller filled in comes first; the body is read when it is
// not there, so that a caller that has the parsed error does not have
// to hand it over twice.
func (f Failure) business() (code int, message string, refused bool) {
	code, message, refused = f.Code, f.Message, f.Refused
	if code != 0 && message != "" {
		return code, message, refused
	}
	if len(f.Body) == 0 {
		return code, message, refused
	}
	c, m, r := parseBusiness(f.Body)
	if code == 0 {
		code = c
	}
	if message == "" {
		message = m
	}
	if !refused {
		refused = r
	}
	return code, message, refused
}

// parseBusiness reads the business error out of a body.
//
// MiniMax's own shape is read first: statusInfo and base_resp carry
// the code and the message. A body that carries a plain message is
// read next, for the shape the site uses when it answers with a plain
// error.
//
// A body that says nothing at all — not JSON, or JSON without a
// message — says nothing here either: there is no message to read,
// and the words are not looked for in the raw bytes.
func parseBusiness(body []byte) (code int, message string, refused bool) {
	if err := upstream.CheckEnvelope("chat", body); err != nil {
		var be *upstream.BusinessError
		if errors.As(err, &be) {
			return be.Code, be.Message, errors.Is(err, upstream.ErrSignInRefused)
		}
	}
	return 0, plainMessage(body), false
}

// plainMessage reads the message out of a body that is not in
// MiniMax's own shape.
//
// The message is looked for at the top level and inside an error
// object, which are the shapes the site uses. A field that is not a
// string says nothing.
func plainMessage(body []byte) string {
	var obj map[string]json.RawMessage
	if json.Unmarshal(body, &obj) != nil {
		return ""
	}
	if msg := messageOf(obj); msg != "" {
		return msg
	}
	for _, key := range []string{"error", "err"} {
		raw, ok := obj[key]
		if !ok {
			continue
		}
		var sub map[string]json.RawMessage
		if json.Unmarshal(raw, &sub) != nil {
			continue
		}
		if msg := messageOf(sub); msg != "" {
			return msg
		}
	}
	return ""
}

// messageOf reads the message out of a JSON object.
func messageOf(obj map[string]json.RawMessage) string {
	for _, key := range []string{"message", "msg"} {
		if s := rawString(obj[key]); s != "" {
			return s
		}
	}
	return ""
}

// rawString reads a JSON string. A field that is not a string says
// nothing.
func rawString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// isQuotaMessage says whether the upstream's message is a refusal for
// running out of credits or plan.
//
// A rate limit is not: the account is fine and the answer comes when
// the caller slows down.
func isQuotaMessage(message string) bool {
	if strings.TrimSpace(message) == "" {
		return false
	}
	return upstream.IsQuotaRefusal([]byte(message))
}
