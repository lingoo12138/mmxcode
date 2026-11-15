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
//
// 卡上摆的行是有限的。网关后面有 98 个号，每个号摆一行「xxx · Credits
// 0 / 400」，卡片就没法看了 —— 一片一样的行，什么话也没说。所以：
//
//   - 全池的积分摆一行（Credits）；
//   - 账号有几个、有几个读到、协议是哪几个，各摆一行（账号、协议）；
//   - 只有「有话说」的账号才各有自己的窗口（用过积分的、用完的），
//     用得最多的排前面，最多十个；
//   - 账号的计划窗口照旧摆着（有计划的账号才有）。
package card

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"mmxcode/internal/account"
	"mmxcode/internal/store"
)

// shownAccounts is how many accounts get a window of their own.
//
// 卡上摆的账号是有限的：98 个号各摆一行，卡片就没法看了。只摆「有话说」
// 的 —— 用过积分的、用完的 —— 用得最多的排前面。
const shownAccounts = 10

// protocols are the protocols the gateway speaks.
//
// 网关三扇门都收：Claude Code 的 Anthropic、多数 agent 的 OpenAI Chat
// Completions、Codex 的 OpenAI Responses。这件事要摆在卡片上：直接指网关
// 的客户端按自己的协议说话，magpie 跟网关讲的是 Anthropic（别的客户端
// magpie 自己翻译）。
const protocols = "Anthropic · Chat Completions · Responses"

// Card is the gateway's usage as magpie reads it.
//
// 上面四个字段是 magpie 的：它照着这些名字读。mmxcode 那块是我们自己的
// （读数是什么时候的、几个账号在后面），magpie 不认识它，也不会把它
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
	// Windows are the windows of the card: the pool's credits, how many
	// accounts there are, the protocols the gateway speaks, and the
	// windows of the accounts that have something to say.
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
}

// Window is one window of the card.
//
// magpie 的窗口：名字、用掉的份额（百分比）、还有它数的是什么东西。
type Window struct {
	// Name is the window's name.
	Name string `json:"name"`
	// Used is the share used, in percent.
	//
	// 它是一个指针，为的是分清「不知道」和「真的是 0」：
	//
	// - nil 是不知道：这个窗口没有 count（Unlimited 的窗口、协议那一
	//   行就是这样）。magpie 的规矩是「vendor 没说就别写 used，绝不要拿 0
	//   冒充不知道」—— 路由拿 used 给账号算权重，一个「不知道」的窗口
	//   报 0，就被当成「还剩 100%」。
	// - 指向 0 是真的是 0：这个窗口有 count，用量就是零。
	//
	// 所以指针是空的才不写这个字段，指向 0 照写。
	Used *float64 `json:"used,omitempty"`
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
//
// spent are the marks of the accounts that have run out: each one is an
// account and the plan group that is spent for it.
//
// An account that is marked spent is out: the gateway will not hand it
// to a client, so the card must not claim its credits -- a card that
// promises credits the gateway refuses is worse than a short balance.
// The windows of the spent group say so, and the account's credits are
// left out of the balance.
func Of(snaps []store.UsageSnapshot, accounts []store.Account, spent []store.Exhausted) Card {
	byAccount := map[string]store.Account{}
	for _, a := range accounts {
		byAccount[a.ID] = a
	}
	marks := spentOf(spent, time.Now())
	c := Card{Windows: []Window{}}
	read := map[string]bool{}

	// 每个账号一块：它自己的积分窗口、它自己的计划窗口。
	type piece struct {
		name string
		// credit 是它自己的积分窗口（读不到就是没有）。
		credit *Window
		plans  []Window
		// used 是它用掉的最大份额（%）。
		used float64
	}
	pieces := map[string]*piece{}
	var order []string

	var granted, used float64
	var expires *time.Time

	for _, s := range snaps {
		read[s.AccountID] = true
		name := accountName(byAccount[s.AccountID], s.AccountID)
		p := pieces[s.AccountID]
		if p == nil {
			p = &piece{name: name}
			pieces[s.AccountID] = p
			order = append(order, s.AccountID)
		}
		mark := marks[s.AccountID]
		if w, ok := creditWindow(s, mark); ok {
			p.credit = &w
			p.used = bigger(p.used, shareOf(w))
		}
		p.plans = append(p.plans, planWindows(s, mark)...)
		for _, w := range p.plans {
			p.used = bigger(p.used, shareOf(w))
		}
		if s.HasAmounts {
			if !mark.out {
				// 用完的账号没有积分可以花：网关不会把它给客户端，卡上
				// 不能还摆着它的数。
				granted += s.Granted
				used += s.Consumed
				c.remaining += s.Remaining
			}
			c.readable = true
			if !mark.out && !s.ExpiresAt.IsZero() && (expires == nil || s.ExpiresAt.Before(*expires)) {
				t := s.ExpiresAt
				expires = &t
			}
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

	// 一个账号的时候，它自己的窗口就是全池的窗口；账号多了，全池的积
	// 分单独摆一行，账号各自的窗口另摆。
	several := len(accounts) > 1
	if several && c.readable && granted > 0 {
		c.Windows = append(c.Windows, poolWindow(granted, used, expires))
	}

	// 有话说的账号：用过积分的、用完的。
	//
	// 只有一个账号的时候，它的窗口就是全池的窗口，都摆着。
	var notable []*piece
	for _, id := range order {
		p := pieces[id]
		if p == nil {
			continue
		}
		if several && p.used <= 0 && !marks[id].out {
			continue
		}
		notable = append(notable, p)
	}
	sort.SliceStable(notable, func(i, j int) bool {
		if notable[i].used != notable[j].used {
			return notable[i].used > notable[j].used
		}
		return notable[i].name < notable[j].name
	})
	hidden := 0
	if len(notable) > shownAccounts {
		hidden = len(notable) - shownAccounts
		notable = notable[:shownAccounts]
	}

	// 账号那一行：几个账号、有几个读到、有几个没上卡。
	summary, ok := summaryWindow(accounts, snaps, read, hidden)
	if ok {
		c.Windows = append(c.Windows, summary)
	}

	for _, p := range notable {
		prefix := ""
		if several {
			prefix = p.name + " · "
		}
		if p.credit != nil {
			w := *p.credit
			w.Name = prefix + "Credits"
			c.Windows = append(c.Windows, w)
		}
		for _, w := range p.plans {
			w.Name = prefix + w.Name
			c.Windows = append(c.Windows, w)
		}
	}

	// 协议那一行：网关讲哪几个协议。
	c.Windows = append(c.Windows, Window{Name: "协议", Display: protocols, Aside: true})

	unread := 0
	for _, a := range accounts {
		if !read[a.ID] {
			unread++
		}
	}
	c.Extra = &Extra{
		Accounts: len(accounts),
		ReadAt:   readAt(snaps),
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

// poolWindow gives the whole pool's credits as one window.
//
// 全池的积分摆一行：98 个号各摆一行，卡片就没法看了。用了多少、共给了
// 多少，是所有读得到数的账号加起来。
func poolWindow(granted, used float64, expires *time.Time) Window {
	usedR := round2(used)
	limitR := round2(granted)
	share := round2(100 * used / granted)
	return Window{
		Name:    "Credits",
		Used:    &share,
		Display: strconv.FormatFloat(usedR, 'f', -1, 64) + " / " + strconv.FormatFloat(limitR, 'f', -1, 64),
		Amount:  &usedR,
		Limit:   &limitR,
		Unit:    "credits",
		// 积分什么时候过期：账号里最早的那个。
		RunsOutAt: expires,
	}
}

// summaryWindow gives the card's 账号 window: how many accounts are behind
// it, how many of them have a reading, and how many were left off.
//
// 只有一个账号的时候不用摆：它自己的窗口已经说了。
func summaryWindow(accounts []store.Account, snaps []store.UsageSnapshot, read map[string]bool, hidden int) (Window, bool) {
	unread := 0
	for _, a := range accounts {
		if !read[a.ID] {
			unread++
		}
	}
	unreadable := unreadableCount(snaps)
	if len(accounts) <= 1 && unread == 0 && unreadable == 0 && hidden == 0 {
		return Window{}, false
	}
	text := fmt.Sprintf("%d 个账号 · %d 有读数", len(accounts), len(snaps))
	if unread > 0 {
		text += fmt.Sprintf(" · %d 没读过", unread)
	}
	if unreadable > 0 {
		text += fmt.Sprintf(" · %d 没读到数", unreadable)
	}
	if hidden > 0 {
		text += fmt.Sprintf(" · %d 没上卡", hidden)
	}
	// 没有 used：这不是一个「用掉了多少」的窗口，是一句话。
	return Window{Name: "账号", Display: text, Aside: true}, true
}

// creditWindow gives the credits as a window.
//
// magpie 的卡片上，积分是一个窗口：共给了多少、用了多少、什么时候用完。
func creditWindow(s store.UsageSnapshot, spent spentInfo) (Window, bool) {
	if !s.HasAmounts || s.Granted <= 0 {
		return Window{}, false
	}
	// 账本给的是十进制的小数，加起来会带出一串小数点后头的零头。
	// 四舍五入到两位：卡片上的数是给人看的。
	used := round2(math.Max(0, s.Consumed))
	limit := round2(s.Granted)
	left := round2(s.Remaining)
	share := round2(100 * used / limit)
	w := Window{
		Name:    "Credits",
		Used:    &share,
		Display: strconv.FormatFloat(used, 'f', -1, 64) + " / " + strconv.FormatFloat(limit, 'f', -1, 64),
		Amount:  &used,
		Limit:   &limit,
		Unit:    "credits",
	}
	if spent.out {
		// 用完的账号没有积分可以花：网关不会把它给客户端。卡上摆着
		// 「437.5 / 500」就是骗人 —— 它说了就要兑现。
		w.Display = "用完"
		return w, true
	}
	if !s.ExpiresAt.IsZero() {
		t := s.ExpiresAt
		w.RunsOutAt = &t
		w.RunsOut = &left
	}
	return w, true
}

// planWindows gives the plan's windows.
func planWindows(s store.UsageSnapshot, spent spentInfo) []Window {
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
		cw := Window{Name: w.Name}
		if w.Unlimited {
			// Unlimited 的窗口没有 count：不知道用了多少，就不给它写
			// used。写 0 是拿 0 冒充「不知道」—— magpie 的路由拿 used
			// 给账号算权重，一个「不知道」的窗口报 0 就被当成「还剩
			// 100%」。参考实现也是这样的：它给的是 {name, display:
			// "Unlimited", aside:true}，里面没有 used。
			cw.Display = "Unlimited"
			cw.Aside = true
			out = append(out, cw)
			continue
		}
		// 这个窗口有 count：用了多少就是多少，真的是 0 就写 0。
		used := w.Used
		cw.Used = &used
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
		if spent.spentGroup(w.ModelName) {
			// 这个组用完了：窗口上就写着「用完」，别让人以为还有。
			cw.Display = "用完"
		}
		out = append(out, cw)
	}
	return out
}

// spentInfo says what is spent of one account.
//
// 标记的粒度是「账号 × 窗口组」：别的组不是用完的，那些窗口照旧摆着数。
type spentInfo struct {
	// out says the account is out: the gateway will not hand it to a
	// client.
	out bool
	// whole says the mark has no group: it counts for every group.
	whole bool
	// groups are the plan groups that are spent.
	groups map[string]bool
}

// spentGroup says whether the plan group is spent for this account.
//
// 组的名字比的时候不看大小写：站上写的是 "General"，这边说的是
// "general"，说的是同一个组（跟网关那边一个规矩）。
func (i spentInfo) spentGroup(group string) bool {
	if i.whole {
		return true
	}
	return i.groups[strings.ToLower(strings.TrimSpace(group))]
}

// spentOf gives the accounts' live spent marks.
//
// 时刻过了的标记不是标记了：那个组已经回来，账号回到轮转里（规格 §4.5
// 的恢复点 A）。所以只有将来的那些算数。
func spentOf(spent []store.Exhausted, now time.Time) map[string]spentInfo {
	out := map[string]spentInfo{}
	for _, e := range spent {
		if !e.Until.After(now) {
			continue
		}
		i := out[e.AccountID]
		if i.groups == nil {
			i.groups = map[string]bool{}
		}
		i.out = true
		key := strings.ToLower(strings.TrimSpace(e.WindowKey))
		if key == "" {
			// 账号级的标记：它对每一个组都算。
			i.whole = true
		} else {
			i.groups[key] = true
		}
		out[e.AccountID] = i
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

// shareOf gives the window's share used, if it says one.
func shareOf(w Window) float64 {
	if w.Used == nil {
		return 0
	}
	return *w.Used
}

// bigger gives the bigger of the two.
func bigger(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

// unreadableCount counts the readings that failed.
func unreadableCount(snaps []store.UsageSnapshot) int {
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
