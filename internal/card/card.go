// Package card says the gateway's usage in magpie's card shape.
//
// magpie 的 provider 插件有一个 usage() 钩子：它把「这个 provider 还剩
// 多少额度」摆成一张卡片，magpie 把它画出来。参考实现（magpie 社区插件
// 的 packages/minimax/index.mjs）摆的是这样一张卡片：
//
//	{ plan, until, balance, windows: [{ name, used, display, resetsAt,
//	  amount, limit, unit, aside, runsOutAt, runsOut }] }
//
// 这个包照着那张卡片做：网关的积分是所有账号加起来，计划是账号里最重
// 的那个，窗口每一个都说清是哪个账号的。
package card

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"time"

	"mmxcode/internal/account"
	"mmxcode/internal/store"
)

// Card is the gateway's usage as magpie reads it.
//
// 上面四个字段是 magpie 的：它照着这些名字读。mmxcode 那块是我们自己
// 的（读数是什么时候的、几个账号在后面），magpie 不认识它，也不会把它
// 当成自己的字段。
type Card struct {
	// Plan is the plan the accounts are on.
	//
	// 它是空的：计划读不到的时候。读不到不是一个「没有计划」——说 Free
	// 是说它没有，说都不说才是真的不知道。
	Plan string `json:"plan,omitempty"`
	// Until is when the plan ends.
	Until *time.Time `json:"until,omitempty"`
	// Balance is what is left, as a count of credits.
	//
	// "437.5 credits"：magpie 的卡片就是这么写的。
	Balance string `json:"balance,omitempty"`
	// Windows are the windows of the accounts: the credits of each one
	// and the windows of its plan.
	Windows []Window `json:"windows"`
	// Extra is what mmxcode knows about the card.
	Extra *Extra `json:"mmxcode,omitempty"`

	// remaining is what is left over the accounts whose credits could
	// be read.
	remaining float64
	// readable says whether any account's credits could be read at all.
	readable bool
}

// Extra is what mmxcode knows about its own card.
//
// 它收在一个名字下面，是为了让上面那几个字段就是 magpie 的字段：一个
// 多出来的字段不会让 magpie 认错，可是放在外头的多出来的字段会。
type Extra struct {
	// ReadAt is when the numbers were read.
	ReadAt *time.Time `json:"read_at,omitempty"`
	// Accounts is how many accounts are behind the card.
	Accounts int `json:"accounts"`
	// Readings is how many of them have a reading.
	Readings int `json:"readings"`
	// Unread is how many have never been read.
	Unread int `json:"unread"`
	// Unreadable is how many had a reading that failed.
	Unreadable int `json:"unreadable"`
}

// Window is one window of the card.
//
// magpie 的窗口：名字、用掉的份额（百分比）、还有它数的是什么东西。
type Window struct {
	// Name is the window's name.
	Name string `json:"name"`
	// Used is the share used, in percent.
	Used float64 `json:"used"`
	// Display is what the window says in words: "62.5 / 500".
	Display string `json:"display,omitempty"`
	// ResetsAt is when the window resets.
	ResetsAt *time.Time `json:"resetsAt,omitempty"`
	// Amount is what was used, when the window counts.
	Amount *float64 `json:"amount,omitempty"`
	// Limit is what was allowed, when the window counts.
	Limit *float64 `json:"limit,omitempty"`
	// Unit is what it counts, when it counts.
	Unit string `json:"unit,omitempty"`
	// Aside says the window is beside the plan, not part of it.
	Aside bool `json:"aside,omitempty"`
	// RunsOutAt is when what is left runs out.
	RunsOutAt *time.Time `json:"runsOutAt,omitempty"`
	// RunsOut is how much is left when it runs out.
	RunsOut *float64 `json:"runsOut,omitempty"`
}

// Of builds the card from the accounts' latest readings.
//
// The readings are the ones that were saved: the card says what was read
// and when, not what would be read now. A reading that failed is left
// out of the totals -- it is not a zero.
func Of(snaps []store.UsageSnapshot, accounts []store.Account) Card {
	byAccount := map[string]store.Account{}
	for _, a := range accounts {
		byAccount[a.ID] = a
	}
	c := Card{Windows: []Window{}}
	read := map[string]bool{}
	for _, s := range snaps {
		read[s.AccountID] = true
		name := accountName(byAccount[s.AccountID], s.AccountID)
		// 一个账号的窗口上带它的名字：网关是轮转的，不说清是谁的，
		// 卡片上就分不出哪个窗口是谁的。
		prefix := ""
		if len(snaps) > 1 {
			prefix = name + " · "
		}
		c.Windows = append(c.Windows, creditWindow(s, prefix)...)
		c.Windows = append(c.Windows, planWindows(s, prefix)...)
		if s.HasAmounts {
			c.remaining += s.Remaining
			c.readable = true
		}
		if p, ok := planOf(s); ok {
			if c.Plan == "" || planRank(p) > planRank(c.Plan) {
				c.Plan = p
			}
			// 计划什么时候没：这一批里最早的那个。一个账号的计划先
			// 没了，那张卡片就不该再拿它说事。
			if !s.PlanUntil.IsZero() && (c.Until == nil || s.PlanUntil.Before(*c.Until)) {
				t := s.PlanUntil
				c.Until = &t
			}
		}
	}
	unread := 0
	for _, a := range accounts {
		if !read[a.ID] {
			unread++
		}
	}
	c.Extra = &Extra{
		Accounts:   len(accounts),
		Readings:   len(snaps),
		Unread:     unread,
		Unreadable: unreadable(snaps),
		ReadAt:     readAt(snaps),
	}
	if c.readable {
		c.Balance = creditsText(c.remaining)
	}
	return c
}

// planOf gives the account's plan, and whether it is known.
//
// 三种：有一个计划（标题或者「M Plan」）、没有计划（Free）、读不到
// （不知道）。
func planOf(s store.UsageSnapshot) (string, bool) {
	if s.Plan != "" {
		return s.Plan, true
	}
	if s.MembershipError != "" || s.PlanError != "" {
		return "", false
	}
	// 名字读不到，可窗口读到了：那是有一个计划的，只是名字不知道。
	// 这种时候说 M Plan（参考实现也是这么说的），不说 Free。
	if s.WindowsJSON != "" {
		return "M Plan", true
	}
	return "Free", true
}

// planRank says how much a plan is worth.
//
// 一个有名字的计划最重，然后是没有名字的 M Plan，最后是 Free：卡片上
// 要摆的是账号里最重的那一个。
func planRank(plan string) int {
	switch {
	case plan == "":
		return 0
	case plan == "Free":
		return 1
	case plan == "M Plan":
		return 2
	default:
		return 3
	}
}

// creditWindow gives the credits as a window.
//
// magpie 的卡片上，积分是一个窗口：共给了多少、用了多少、什么时候用完。
func creditWindow(s store.UsageSnapshot, prefix string) []Window {
	if !s.HasAmounts || s.Granted <= 0 {
		return nil
	}
	// 账本给的是十进制的小数，加起来会带出一串小数点后头的零头。
	// 四舍五入到两位：卡片上的数是给人看的。
	used := round2(math.Max(0, s.Consumed))
	limit := round2(s.Granted)
	left := round2(s.Remaining)
	w := Window{
		Name:    prefix + "Credits",
		Used:    round2(100 * used / limit),
		Display: strconv.FormatFloat(used, 'f', -1, 64) + " / " + strconv.FormatFloat(limit, 'f', -1, 64),
		Amount:  &used,
		Limit:   &limit,
		Unit:    "credits",
	}
	if !s.ExpiresAt.IsZero() {
		t := s.ExpiresAt
		w.RunsOutAt = &t
		w.RunsOut = &left
	}
	return []Window{w}
}

// planWindows gives the plan's windows.
func planWindows(s store.UsageSnapshot, prefix string) []Window {
	if strings.TrimSpace(s.WindowsJSON) == "" {
		return nil
	}
	var ws []account.Window
	if err := json.Unmarshal([]byte(s.WindowsJSON), &ws); err != nil {
		// 读不出来的窗口就是没有窗口：这里不能因为一个存坏的读数
		// 就把整张卡片弄坏。
		return nil
	}
	out := make([]Window, 0, len(ws))
	for _, w := range ws {
		cw := Window{Name: prefix + w.Name, Used: w.Used}
		if w.Unlimited {
			cw.Display = "Unlimited"
			cw.Aside = true
			out = append(out, cw)
			continue
		}
		if !w.ResetsAt.IsZero() {
			t := w.ResetsAt
			cw.ResetsAt = &t
		}
		if w.Amount != 0 {
			v := w.Amount
			cw.Amount = &v
		}
		if w.Limit != 0 {
			v := w.Limit
			cw.Limit = &v
		}
		cw.Unit = w.Unit
		out = append(out, cw)
	}
	return out
}

// accountName gives the account's name for the card.
func accountName(a store.Account, id string) string {
	if a.Name != "" {
		return a.Name
	}
	if a.Email != "" {
		return a.Email
	}
	return id
}

// unreadable counts the readings that failed.
func unreadable(snaps []store.UsageSnapshot) int {
	n := 0
	for _, s := range snaps {
		if !s.HasAmounts {
			n++
		}
	}
	return n
}

// readAt gives when the numbers were read: the newest reading's time.
func readAt(snaps []store.UsageSnapshot) *time.Time {
	var out *time.Time
	for _, s := range snaps {
		if s.CreatedAt.IsZero() {
			continue
		}
		if out == nil || s.CreatedAt.After(*out) {
			t := s.CreatedAt
			out = &t
		}
	}
	return out
}

// creditsText says a balance as a count of credits.
//
// 跟参考实现一样："1234.50" 是 "1234.5 credits"，整数是 "500 credits"。
func creditsText(n float64) string {
	if math.IsNaN(n) || math.IsInf(n, 0) {
		return ""
	}
	if n == math.Trunc(n) {
		return strconv.FormatInt(int64(n), 10) + " credits"
	}
	s := strconv.FormatFloat(n, 'f', 2, 64)
	s = strings.TrimRight(s, "0")
	s = strings.TrimSuffix(s, ".")
	return s + " credits"
}

// round2 rounds to two decimals, the way the reference does.
func round2(n float64) float64 {
	return math.Round(n*100) / 100
}
