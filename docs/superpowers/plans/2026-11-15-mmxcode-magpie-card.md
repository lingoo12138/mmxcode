# magpie 的额度卡片 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 网关给出一个 magpie 认得的额度卡片接口，magpie 把它当作 provider 的额度卡片画出来。

**Architecture:** `internal/card` 把网关的读数摆成 magpie 的卡片形状（`plan`、`until`、`balance`、`windows`）；`internal/gateway` 的 `GET /v1/usage` 把它交出去，在聊天那扇门后面（手里有聊天那把 key 的都能问）。

**Tech Stack:** Go 1.26、`net/http`、`encoding/json`

---

## 三条必须守住的不变量

1. **形状是 magpie 的。** 字段名照着参考实现（`packages/minimax/index.mjs` 的 `usageOf`）：`plan`、`until`、`balance`、`windows`。我们自己多知道的东西收在 `mmxcode` 那一个名字下面，不跟 magpie 的字段混在一起。
2. **没读到的不是零。** 读不到的账号不算进 `balance`；读不到的计划不写成 `Free`。卡片上少一个数，比卡片上有一个假数好。
3. **网关是轮转的，卡片是整个网关的。** `balance` 是所有账号剩下的积分加起来；计划取账号里最重的那个；窗口每一个都说清是哪个账号的。

### Task 1: card 包

**Files:**
- Create: `internal/card/card.go`
- Create: `internal/card/card_test.go`

- [ ] **Step 1: 写失败的测试**

创建 `internal/card/card_test.go`：

```go
package card

import (
	"encoding/json"
	"testing"
	"time"

	"mmxcode/internal/account"
	"mmxcode/internal/store"
)

// TestTheCardSaysHowManyCreditsAreLeft says the card says what is left.
//
// The balance is what the person wants to see: magpie's card shows it as
// "437.5 credits".
func TestTheCardSaysHowManyCreditsAreLeft(t *testing.T) {
	c := Of(
		[]store.UsageSnapshot{{
			AccountID: "acc_01", HasAmounts: true,
			Granted: 500, Consumed: 62.5, Remaining: 437.5,
			CreatedAt: time.Now(),
		}},
		[]store.Account{{ID: "acc_01", Name: "主号"}},
	)
	if c.Balance != "437.5 credits" {
		t.Errorf("balance = %q, want %q", c.Balance, "437.5 credits")
	}
}

// TestAWholeNumberOfCreditsIsSaidWithoutADecimal says 500 is "500 credits".
func TestAWholeNumberOfCreditsIsSaidWithoutADecimal(t *testing.T) {
	c := Of(
		[]store.UsageSnapshot{{
			AccountID: "acc_01", HasAmounts: true,
			Granted: 500, Consumed: 0, Remaining: 500,
		}},
		[]store.Account{{ID: "acc_01", Name: "主号"}},
	)
	if c.Balance != "500 credits" {
		t.Errorf("balance = %q, want %q", c.Balance, "500 credits")
	}
}

// TestTheCreditsOfEveryAccountAreAddedUp says the card is the whole
// gateway's credits.
//
// 网关是轮转的：卡片说的是所有账号剩下的积分加起来，不是某一个账号的。
func TestTheCreditsOfEveryAccountAreAddedUp(t *testing.T) {
	c := Of(
		[]store.UsageSnapshot{
			{AccountID: "acc_01", HasAmounts: true, Granted: 500, Consumed: 100, Remaining: 400},
			{AccountID: "acc_02", HasAmounts: true, Granted: 300, Consumed: 50, Remaining: 250},
		},
		[]store.Account{
			{ID: "acc_01", Name: "主号"},
			{ID: "acc_02", Name: "副号"},
		},
	)
	if c.Balance != "650 credits" {
		t.Errorf("balance = %q, want %q", c.Balance, "650 credits")
	}
}

// TestAnAccountThatWasNeverReadIsNotCounted says an account without a
// reading is left out of the balance.
//
// 没读过的账号不是零积分，是不知道。把它算成零，卡片就会说一个假的数。
func TestAnAccountThatWasNeverReadIsNotCounted(t *testing.T) {
	c := Of(
		[]store.UsageSnapshot{
			{AccountID: "acc_01", HasAmounts: true, Granted: 500, Consumed: 100, Remaining: 400},
		},
		[]store.Account{
			{ID: "acc_01", Name: "主号"},
			{ID: "acc_02", Name: "副号"},
		},
	)
	if c.Balance != "400 credits" {
		t.Errorf("balance = %q, want %q", c.Balance, "400 credits")
	}
	if c.Extra == nil || c.Extra.Unread != 1 {
		t.Fatalf("extra = %+v, want one unread account", c.Extra)
	}
}

// TestTheCardHasNoBalanceWhenNothingWasRead says there is no balance
// when no account's credits could be read.
//
// 一个数都没有的时候，卡片就不说那个数：说"0 credits"是说它一点也没有。
func TestTheCardHasNoBalanceWhenNothingWasRead(t *testing.T) {
	c := Of(
		[]store.UsageSnapshot{
			{AccountID: "acc_01", MembershipError: "the site did not answer"},
		},
		[]store.Account{{ID: "acc_01", Name: "主号"}},
	)
	if c.Balance != "" {
		t.Errorf("balance = %q, want none", c.Balance)
	}
}

// TestTheCardNamesThePlan says the plan's name is on the card.
func TestTheCardNamesThePlan(t *testing.T) {
	c := Of(
		[]store.UsageSnapshot{{
			AccountID: "acc_01", HasAmounts: true, Granted: 500, Remaining: 500,
			Plan: "Token Plan M3", PlanUntil: time.Now().Add(24 * time.Hour),
		}},
		[]store.Account{{ID: "acc_01", Name: "主号"}},
	)
	if c.Plan != "Token Plan M3" {
		t.Errorf("plan = %q, want %q", c.Plan, "Token Plan M3")
	}
	if c.Until == nil {
		t.Fatal("the card has no until")
	}
}

// TestAnAccountWithoutAPlanIsFree says an account with no plan is "Free".
func TestAnAccountWithoutAPlanIsFree(t *testing.T) {
	c := Of(
		[]store.UsageSnapshot{{
			AccountID: "acc_01", HasAmounts: true, Granted: 500, Remaining: 500,
		}},
		[]store.Account{{ID: "acc_01", Name: "主号"}},
	)
	if c.Plan != "Free" {
		t.Errorf("plan = %q, want %q", c.Plan, "Free")
	}
}

// TestThePlanIsLeftOutWhenItIsNotKnown says a plan that could not be
// read is not named.
//
// 读不到的计划不是一个没有计划：说 Free 是说它没有，说都��说才是真的
// 不知道。
func TestThePlanIsLeftOutWhenItIsNotKnown(t *testing.T) {
	c := Of(
		[]store.UsageSnapshot{{
			AccountID: "acc_01", HasAmounts: true, Granted: 500, Remaining: 500,
			MembershipError: "the site did not answer",
		}},
		[]store.Account{{ID: "acc_01", Name: "主号"}},
	)
	if c.Plan != "" {
		t.Errorf("plan = %q, want none", c.Plan)
	}
}

// TestTheCreditsAreAWindowOfTheirOwn says the credits are a window, the
// way magpie's card has them.
//
// 积分在 magpie 的卡片上是一个窗口：用了多少、共给了多少。
func TestTheCreditsAreAWindowOfTheirOwn(t *testing.T) {
	c := Of(
		[]store.UsageSnapshot{{
			AccountID: "acc_01", HasAmounts: true,
			Granted: 500, Consumed: 62.5, Remaining: 437.5,
			ExpiresAt: time.Now().Add(time.Hour),
		}},
		[]store.Account{{ID: "acc_01", Name: "主号"}},
	)
	if len(c.Windows) != 1 {
		t.Fatalf("windows = %d, want 1", len(c.Windows))
	}
	w := c.Windows[0]
	if w.Name != "Credits" {
		t.Errorf("name = %q, want %q", w.Name, "Credits")
	}
	if w.Unit != "credits" {
		t.Errorf("unit = %q, want %q", w.Unit, "credits")
	}
	if w.Display != "62.5 / 500" {
		t.Errorf("display = %q, want %q", w.Display, "62.5 / 500")
	}
	if w.Amount == nil || *w.Amount != 62.5 {
		t.Errorf("amount = %v, want 62.5", w.Amount)
	}
	if w.Limit == nil || *w.Limit != 500 {
		t.Errorf("limit = %v, want 500", w.Limit)
	}
}

// TestThePlanWindowsAreOnTheCard says the plan's windows are on the card.
func TestThePlanWindowsAreOnTheCard(t *testing.T) {
	ws, _ := json.Marshal([]account.Window{
		{Name: "7 days", ModelName: "general", Used: 24, ResetsAt: time.Now().Add(3 * 24 * time.Hour)},
	})
	c := Of(
		[]store.UsageSnapshot{{
			AccountID: "acc_01", HasAmounts: true, Granted: 500, Remaining: 500,
			Plan: "M Plan", WindowsJSON: string(ws),
		}},
		[]store.Account{{ID: "acc_01", Name: "主号"}},
	)
	found := false
	for _, w := range c.Windows {
		if w.Name == "7 days" {
			found = true
			if w.Used != 24 {
				t.Errorf("used = %v, want 24", w.Used)
			}
			if w.ResetsAt == nil {
				t.Error("the window has no resetsAt")
			}
		}
	}
	if !found {
		t.Errorf("windows = %+v, want a 7 days window", c.Windows)
	}
}

// TestAnUnlimitedWindowIsSaidSo says an unlimited window says so.
func TestAnUnlimitedWindowIsSaidSo(t *testing.T) {
	ws, _ := json.Marshal([]account.Window{
		{Name: "3 hours", ModelName: "general", Unlimited: true},
	})
	c := Of(
		[]store.UsageSnapshot{{
			AccountID: "acc_01", HasAmounts: true, Granted: 500, Remaining: 500,
			WindowsJSON: string(ws),
		}},
		[]store.Account{{ID: "acc_01", Name: "主号"}},
	)
	if len(c.Windows) != 2 {
		t.Fatalf("windows = %d, want 2", len(c.Windows))
	}
	found := false
	for _, w := range c.Windows {
		if w.Display != "Unlimited" {
			continue
		}
		found = true
		if !w.Aside {
			t.Error("the unlimited window is not aside")
		}
	}
	if !found {
		t.Errorf("windows = %+v, want an unlimited one", c.Windows)
	}
}

// TestTheAccountsAreNamedOnTheWindows says every window says whose it is.
//
// 网关有好几个账号，窗口不说清是谁的，卡片上就分不出哪个是哪个。
func TestTheAccountsAreNamedOnTheWindows(t *testing.T) {
	c := Of(
		[]store.UsageSnapshot{
			{AccountID: "acc_01", HasAmounts: true, Granted: 500, Remaining: 400},
			{AccountID: "acc_02", HasAmounts: true, Granted: 300, Remaining: 250},
		},
		[]store.Account{
			{ID: "acc_01", Name: "主号"},
			{ID: "acc_02", Name: "副号"},
		},
	)
	if len(c.Windows) != 2 {
		t.Fatalf("windows = %d, want 2", len(c.Windows))
	}
	if c.Windows[0].Name != "主号 · Credits" {
		t.Errorf("name = %q, want %q", c.Windows[0].Name, "主号 · Credits")
	}
}

// TestTheCardIsMagpiesShape says the JSON is the shape magpie reads.
//
// 字段名是 magpie 卡片上的那些：plan、until、balance、windows。多出来的
// 那些都收在一个名字下面，不会被 magpie 当成它自己的字段。
func TestTheCardIsMagpiesShape(t *testing.T) {
	c := Of(
		[]store.UsageSnapshot{{
			AccountID: "acc_01", HasAmounts: true,
			Granted: 500, Consumed: 62.5, Remaining: 437.5,
			Plan: "Token Plan M3", PlanUntil: time.Now(),
		}},
		[]store.Account{{ID: "acc_01", Name: "主号"}},
	)
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"plan", "until", "balance", "windows"} {
		if _, ok := out[k]; !ok {
			t.Errorf("the card has no %q: %s", k, b)
		}
	}
	if _, ok := out["mmxcode"]; !ok {
		t.Errorf("the card has no mmxcode block: %s", b)
	}
}

// TestTheNumbersAreNotFullOfNoise says the numbers are rounded.
//
// 账本给的是十进制的小数（"0.05"），加起来会带出一串小数点后头的零头
// （0.05000000000001137）。参考实现是把它们四舍五入到两位再摆出来的，
// 这里也是：卡片上的数是给人看的。
func TestTheNumbersAreNotFullOfNoise(t *testing.T) {
	c := Of(
		[]store.UsageSnapshot{{
			AccountID: "acc_01", HasAmounts: true,
			Granted: 400, Consumed: 0.05, Remaining: 399.95,
			ExpiresAt: time.Now().Add(time.Hour),
		}},
		[]store.Account{{ID: "acc_01", Name: "主号"}},
	)
	if len(c.Windows) != 1 {
		t.Fatalf("windows = %d, want 1", len(c.Windows))
	}
	w := c.Windows[0]
	if w.Amount == nil || *w.Amount != 0.05 {
		t.Errorf("amount = %v, want 0.05", w.Amount)
	}
	if w.RunsOut == nil || *w.RunsOut != 399.95 {
		t.Errorf("runsOut = %v, want 399.95", w.RunsOut)
	}
	if w.Used != 0.01 {
		t.Errorf("used = %v, want 0.01", w.Used)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

```
go test ./internal/card/
```

失败：`undefined: Of`。

- [ ] **Step 3: 实现 card**

创建 `internal/card/card.go`：

```go
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
```

- [ ] **Step 4: 运行测试确认通过**

```
go test ./internal/card/
```

- [ ] **Step 5: 提交**

```
git add internal/card
git commit -m "feat(card): 网关的额度摆成 magpie 的卡片形状"
```

### Task 2: 网关的卡片接口

**Files:**
- Create: `internal/gateway/usage.go`
- Modify: `internal/gateway/gateway.go`（路由）
- Modify: `internal/gateway/gateway_test.go`

- [ ] **Step 1: 写失败的测试**

在 `internal/gateway/gateway_test.go` 里加：

```go
// TestTheUsageCardIsMagpiesShape says the gateway has an endpoint that
// says the usage in magpie's card shape.
//
// magpie 的 provider 插件有一个 usage() 钩子，卡片就是从那里来的。我们
// ��自己端点，所以这个钩子得由网关自己顶上来：给 magpie 一个 magpie
// 认得的形状。
func TestTheUsageCardIsMagpiesShape(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{}`)) })
	if err := h.store.SaveUsageSnapshot(context.Background(), store.UsageSnapshot{
		AccountID: "acc_01", HasAmounts: true,
		Granted: 500, Consumed: 62.5, Remaining: 437.5,
		Plan: "Token Plan M3", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/usage", nil)
	rec := httptest.NewRecorder()
	h.gw.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Plan    string `json:"plan"`
		Balance string `json:"balance"`
		Windows []struct {
			Name string `json:"name"`
		} `json:"windows"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Plan != "Token Plan M3" {
		t.Errorf("plan = %q, want %q", out.Plan, "Token Plan M3")
	}
	if out.Balance != "437.5 credits" {
		t.Errorf("balance = %q, want %q", out.Balance, "437.5 credits")
	}
	if len(out.Windows) == 0 {
		t.Error("the card has no windows")
	}
}

// TestTheUsageCardNeedsAKey says the card is behind the key.
//
// 卡片说的是这个网关还剩多少积分：那不是给路人看的。
func TestTheUsageCardNeedsAKey(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{}`)) })
	h.gw.Cfg.Server.APIKeys = []string{"sk-admin"}
	req := httptest.NewRequest(http.MethodGet, "/v1/usage", nil)
	rec := httptest.NewRecorder()
	h.gw.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401", rec.Code)
	}
}

// TestAKeyMadeInTheConsoleOpensTheUsageCard says a chat key opens the
// card.
//
// magpie 是拿着聊天那把 key 来问卡片的：它问的时候手里有的就是这一把。
func TestAKeyMadeInTheConsoleOpensTheUsageCard(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{}`)) })
	h.gw.Cfg.Server.APIKeys = []string{"sk-admin"}
	plain := makeAKey(t, h.store)
	req := httptest.NewRequest(http.MethodGet, "/v1/usage", nil)
	req.Header.Set("x-api-key", plain)
	rec := httptest.NewRecorder()
	h.gw.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

```
go test ./internal/gateway/ -run "UsageCard"
```

失败：`code = 404`。

- [ ] **Step 3: 实现接口**

创建 `internal/gateway/usage.go`：

```go
package gateway

import (
	"encoding/json"
	"log"
	"net/http"

	"mmxcode/internal/card"
)

// usageCard says the gateway's usage in magpie's card shape.
//
// magpie 的 provider 插件有一个 usage() 钩子，卡片就是从那里来的：magpie
// 照着 plan、until、balance、windows 这几个字段画。我们是自己的端点，那
// 个钩子得由网关自己顶上来 —— 给 magpie 一个 magpie 认得的形状，它就能
// 把我们这些账号的积分画出来。
//
// 卡片说的是上一次读到的数（调度器每 15 分钟读一遍，后台页面上也有一个
// 「刷新」按钮）。要现读一遍就在地址上加 refresh=1。
//
// 这个接口在聊天那扇门后面：手里有那把 key 的（聊天用的、或者后台登录
// 用的）都能问。问不到的就是没带 key。
func (s *Server) usageCard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if r.URL.Query().Get("refresh") == "1" && s.Mgr != nil {
		res, err := s.Mgr.RefreshAllUsage(ctx)
		if err != nil {
			// 读不到不是没有卡片：上一次读到的数照样摆出来，只是它是
			// 上一次的。理由记在日志里。
			log.Printf("usage card: the refresh failed: %v", err)
		} else {
			for _, reason := range res.Reasons {
				log.Printf("usage card: %s", reason)
			}
		}
	}
	snaps, err := s.Store.LatestUsageSnapshots(ctx)
	if err != nil {
		writeError(w, protocolOf(r.URL.Path), http.StatusInternalServerError, err.Error())
		return
	}
	accounts, err := s.Store.ListAccounts(ctx)
	if err != nil {
		writeError(w, protocolOf(r.URL.Path), http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(card.Of(snaps, accounts))
}
```

- [ ] **Step 4: 挂路由**

`internal/gateway/gateway.go` 的 `ServeHTTP`：

```go
// ServeHTTP routes a request.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/signin":
		// 登录页不用会话：没有会话的人正要来这里登录。
		s.signInPage(w, r, http.StatusOK, "")
		return
	case "/api/session":
		// 发 cookie 和销 cookie 都不用会话。
		s.session(w, r)
		return
	}
	// 两扇门：聊天那扇认两把 key（配置里的和后台申请的），管理页那扇只
	// 认配置里的那把。
	gate := s.auth
	if isChatPath(r.URL.Path) {
		gate = s.authChat
	}
	if !gate(r) {
		if isAPIPath(r.URL.Path) {
			writeError(w, protocolOf(r.URL.Path), http.StatusUnauthorized, "the key was refused")
			return
		}
		// 浏览器来的是页面：给它登录页，不是一段 JSON。
		s.signInPage(w, r, http.StatusOK, "")
		return
	}
	switch r.URL.Path {
	case "/v1/messages":
		s.handleChat(w, r, protocolAnthropic)
		return
	case "/v1/chat/completions":
		s.handleChat(w, r, protocolOpenAI)
		return
	case "/v1/responses":
		s.handleChat(w, r, protocolCodex)
		return
	case "/v1/models":
		s.handleModels(w, r)
		return
	case "/v1/usage":
		// magpie 的卡片：这个网关还剩多少额度。
		s.usageCard(w, r)
		return
	}
	if s.WebUI != nil && strings.HasPrefix(r.URL.Path, "/api/") {
		s.WebUI.ServeHTTP(w, r)
		return
	}
	if s.WebUI != nil {
		s.WebUI.ServeHTTP(w, r)
		return
	}
	writeError(w, protocolOf(r.URL.Path), http.StatusNotFound, "no such endpoint")
}
```

- [ ] **Step 5: 运行测试确认通过**

```
go test ./internal/gateway/
```

- [ ] **Step 6: 提交**

```
git add internal/gateway
git commit -m "feat(gateway): GET /v1/usage 给 magpie 的卡片"
```

### Task 3: 文档

**Files:**
- Modify: `README.md`

- [ ] **Step 1: 写清楚这个接口**

README 的「对外接口」里加上 `GET /v1/usage`，再写一节「magpie 的额度卡片」：卡片长什么样、四个字段是什么、网关是多账号的时候每个字段怎么来的、数字是上一次读到的（`refresh=1` 才现读）。

- [ ] **Step 2: 提交**

```
git add README.md
git commit -m "docs(readme): magpie 的额度卡片"
```

