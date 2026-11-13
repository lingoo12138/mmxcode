package rotator

import "mmxcode/internal/upstream"

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
}

// Decide says what to do about a failure.
//
// The rules come from what the site actually does:
//
//   - a refusal that names the credits or the quota means the account
//     is spent: another account is tried;
//   - a 429 that does not name the credits is a rate limit, and the
//     same account is asked again after a pause;
//   - a 5xx or a network error is the same: the same account, again;
//   - a 401 means the token is stale: it is renewed first;
//   - a redirect to the other site is neither of these: the account is
//     fine and the network is not, so nothing is changed and no
//     refresh token is spent.
func Decide(f Failure) Action {
	if f.OtherSite {
		return ActionGiveUp
	}
	if f.Network {
		return ActionRetrySame
	}
	if f.Status == 200 {
		// 一个 200 里装着业务错误。只有确认了那是错误才会走到
		// 这里，所以按错误来判。
		if upstream.IsQuotaRefusal(f.Body) {
			return ActionFailover
		}
		return ActionGiveUp
	}
	switch f.Status {
	case 401:
		return ActionRenew
	case 402, 403:
		if upstream.IsQuotaRefusal(f.Body) {
			return ActionFailover
		}
		return ActionRetrySame
	case 429:
		if upstream.IsQuotaRefusal(f.Body) {
			return ActionFailover
		}
		return ActionRetrySame
	}
	if f.Status >= 500 {
		return ActionRetrySame
	}
	return ActionGiveUp
}
