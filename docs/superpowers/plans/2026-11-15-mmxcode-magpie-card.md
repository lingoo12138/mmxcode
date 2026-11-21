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
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/lingoo12138/mmxcode/internal/account"
	"github.com/lingoo12138/mmxcode/internal/store"
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
		nil,
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
		nil,
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
		nil,
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
		nil,
	)
	if c.Balance != "400 credits" {
		t.Errorf("balance = %q, want %q", c.Balance, "400 credits")
	}
	// 没读过的那个，摆在卡片上看得见：账号那一行说了有几个没读到。
	if got := summaryText(c); got != "2 个账号 · 1 有读数 · 1 没读过" {
		t.Errorf("the summary says %q, want %q", got, "2 个账号 · 1 有读数 · 1 没读过")
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
		nil,
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
		nil,
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
		nil,
	)
	if c.Plan != "Free" {
		t.Errorf("plan = %q, want %q", c.Plan, "Free")
	}
}

// TestThePlanIsLeftOutWhenItIsNotKnown says a plan that could not be
// read is not named.
//
// 读不到的计划不是一个没有计划：说 Free 是说它没有，说都不说才是真的
// 不知道。
func TestThePlanIsLeftOutWhenItIsNotKnown(t *testing.T) {
	c := Of(
		[]store.UsageSnapshot{{
			AccountID: "acc_01", HasAmounts: true, Granted: 500, Remaining: 500,
			MembershipError: "the site did not answer",
		}},
		[]store.Account{{ID: "acc_01", Name: "主号"}},
		nil,
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
		nil,
	)
	// 积分那一行，协议那一行。
	if len(c.Windows) != 2 {
		t.Fatalf("windows = %d, want 2", len(c.Windows))
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
		nil,
	)
	found := false
	for _, w := range c.Windows {
		if w.Name == "7 days" {
			found = true
			if w.Used == nil {
				t.Error("the window does not say how much of it was used")
			} else if *w.Used != 24 {
				t.Errorf("used = %v, want 24", *w.Used)
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
		nil,
	)
	// 积分那一行、Unlimited 那一行、协议那一行。
	if len(c.Windows) != 3 {
		t.Fatalf("windows = %d, want 3", len(c.Windows))
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

// TestTheUnlimitedWindowDoesNotSayItUsedNothing says an unlimited window
// does not claim it used nothing.
//
// 「不知道」和「真的是 0」是两件事。Unlimited 的窗口是没有 count 的：参考
// 实现给的是 {name, display:"Unlimited", aside:true}，里面没有 used。写了
// "used": 0 就是拿 0 冒充「不知道」，而 magpie 的路由拿 used 给账号算权重
// —— 一个「不知道」的窗口报 0，就被当成「还剩 100%」。
func TestTheUnlimitedWindowDoesNotSayItUsedNothing(t *testing.T) {
	ws, _ := json.Marshal([]account.Window{
		{Name: "3 hours", ModelName: "general", Unlimited: true},
	})
	c := Of(
		[]store.UsageSnapshot{{
			AccountID: "acc_01", HasAmounts: true, Granted: 500, Remaining: 500,
			WindowsJSON: string(ws),
		}},
		[]store.Account{{ID: "acc_01", Name: "主号"}},
		nil,
	)
	found := false
	for _, w := range c.Windows {
		if w.Display != "Unlimited" {
			continue
		}
		found = true
		if w.Used != nil {
			t.Errorf("the unlimited window says it used %v: it does not know", *w.Used)
		}
	}
	if !found {
		t.Fatalf("windows = %+v, want an unlimited one", c.Windows)
	}
	// 卡片上也不该有那个字段。
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Windows []map[string]any `json:"windows"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	for _, w := range out.Windows {
		if w["display"] != "Unlimited" {
			continue
		}
		if _, ok := w["used"]; ok {
			t.Errorf("the unlimited window has a used field: %s", b)
		}
	}
}

// TestAWindowThatReallyUsedNothingSaysZero says a window that really
// used nothing says zero.
//
// 「不知道」和「真的是 0」要分得开：真的是 0 的窗口要写出 0，不能因为
// 指针是空就一起没了。
func TestAWindowThatReallyUsedNothingSaysZero(t *testing.T) {
	ws, _ := json.Marshal([]account.Window{
		{Name: "7 days", ModelName: "general", Used: 0},
	})
	c := Of(
		[]store.UsageSnapshot{{
			AccountID: "acc_01", HasAmounts: true, Granted: 500, Remaining: 500,
			WindowsJSON: string(ws),
		}},
		[]store.Account{{ID: "acc_01", Name: "主号"}},
		nil,
	)
	found := false
	for _, w := range c.Windows {
		if w.Name != "7 days" {
			continue
		}
		found = true
		if w.Used == nil {
			t.Error("the window says nothing about its use: it used nothing, that is zero")
		} else if *w.Used != 0 {
			t.Errorf("used = %v, want 0", *w.Used)
		}
	}
	if !found {
		t.Fatalf("windows = %+v, want a 7 days window", c.Windows)
	}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Windows []map[string]any `json:"windows"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	for _, w := range out.Windows {
		if w["name"] != "7 days" {
			continue
		}
		v, ok := w["used"]
		if !ok {
			t.Errorf("the window has no used field: %s", b)
			continue
		}
		if v.(float64) != 0 {
			t.Errorf("used = %v, want 0", v)
		}
	}
}

// TestTheAccountsAreNamedOnTheWindows says every window says whose it is.
//
// 网关有好几个账号，窗口不说清是谁的，卡片上就分不出哪个是哪个。
func TestTheAccountsAreNamedOnTheWindows(t *testing.T) {
	c := Of(
		[]store.UsageSnapshot{
			{AccountID: "acc_01", HasAmounts: true, Granted: 500, Consumed: 50, Remaining: 450},
			{AccountID: "acc_02", HasAmounts: true, Granted: 300, Consumed: 50, Remaining: 250},
		},
		[]store.Account{
			{ID: "acc_01", Name: "主号"},
			{ID: "acc_02", Name: "副号"},
		},
		nil,
	)
	names := map[string]bool{}
	for _, w := range c.Windows {
		names[w.Name] = true
	}
	for _, want := range []string{"主号 · Credits", "副号 · Credits"} {
		if !names[want] {
			t.Errorf("windows = %+v, want a %q window", c.Windows, want)
		}
	}
}

// TestThePoolIsOnOneWindow says the whole pool's credits are one window.
//
// 98 个号各摆一行，卡片就没法看了：全池的积分摆一行，别的账号在「账号」
// 那一行里说清楚。
func TestThePoolIsOnOneWindow(t *testing.T) {
	c := Of(
		[]store.UsageSnapshot{
			{AccountID: "acc_01", HasAmounts: true, Granted: 500, Consumed: 100, Remaining: 400},
			{AccountID: "acc_02", HasAmounts: true, Granted: 300, Consumed: 50, Remaining: 250},
		},
		[]store.Account{
			{ID: "acc_01", Name: "主号"},
			{ID: "acc_02", Name: "副号"},
		},
		nil,
	)
	w, ok := windowOf(c, "Credits")
	if !ok {
		t.Fatalf("windows = %+v, want a Credits window", c.Windows)
	}
	if w.Display != "150 / 800" {
		t.Errorf("display = %q, want %q", w.Display, "150 / 800")
	}
	if w.Used == nil || *w.Used != 18.75 {
		t.Errorf("used = %v, want 18.75", w.Used)
	}
}

// TestTheAccountsThatUsedNothingAreNotOnTheCard says an account that used
// nothing is not a window of its own.
//
// 一行「xxx@hotmail.com · Credits 0 / 400」没有话可说：98 个号全是这
// 样的，卡片就没法看了。
func TestTheAccountsThatUsedNothingAreNotOnTheCard(t *testing.T) {
	snaps := []store.UsageSnapshot{}
	accts := []store.Account{}
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("acc_%02d", i)
		snaps = append(snaps, store.UsageSnapshot{
			AccountID: id, HasAmounts: true, Granted: 400, Remaining: 400,
		})
		accts = append(accts, store.Account{ID: id, Name: fmt.Sprintf("号%02d", i)})
	}
	c := Of(snaps, accts, nil)
	for _, w := range c.Windows {
		if strings.HasPrefix(w.Name, "号") && w.Display == "0 / 400" {
			t.Errorf("the card has a window for an account that used nothing: %+v", c.Windows)
		}
	}
	// 摆着的只有三行：全池的积分，账号那一句，协议那一句。
	if len(c.Windows) != 3 {
		t.Fatalf("windows = %d, want 3: %+v", len(c.Windows), c.Windows)
	}
}

// TestTheAccountsThatUsedSomethingAreOnTheCard says an account that used
// something gets a window of its own.
func TestTheAccountsThatUsedSomethingAreOnTheCard(t *testing.T) {
	c := Of(
		[]store.UsageSnapshot{
			{AccountID: "acc_01", HasAmounts: true, Granted: 400, Remaining: 400},
			{AccountID: "acc_02", HasAmounts: true, Granted: 400, Consumed: 31.5, Remaining: 368.5},
		},
		[]store.Account{
			{ID: "acc_01", Name: "号01"},
			{ID: "acc_02", Name: "号02"},
		},
		nil,
	)
	w, ok := windowOf(c, "号02 · Credits")
	if !ok {
		t.Fatalf("windows = %+v, want a window for the account that used something", c.Windows)
	}
	if w.Display != "31.5 / 400" {
		t.Errorf("display = %q, want %q", w.Display, "31.5 / 400")
	}
	if w.Used == nil || *w.Used != 7.88 {
		t.Errorf("used = %v, want 7.88", w.Used)
	}
}

// TestAtMostTenAccountsAreOnTheCard says the card does not put every
// account on it.
//
// 账号多的时候，卡上摆的账号是有限的：摆用得最多的那几个，其余的在
// 「账号」那一行里说清楚有几个没摆上。
func TestAtMostTenAccountsAreOnTheCard(t *testing.T) {
	snaps := []store.UsageSnapshot{}
	accts := []store.Account{}
	for i := 0; i < 15; i++ {
		id := fmt.Sprintf("acc_%02d", i)
		snaps = append(snaps, store.UsageSnapshot{
			AccountID: id, HasAmounts: true, Granted: 400,
			Consumed: float64(i + 1), Remaining: 400 - float64(i+1),
		})
		accts = append(accts, store.Account{ID: id, Name: fmt.Sprintf("号%02d", i)})
	}
	c := Of(snaps, accts, nil)
	own := 0
	for _, w := range c.Windows {
		if strings.HasPrefix(w.Name, "号") {
			own++
		}
	}
	if own != 10 {
		t.Errorf("the card has %d account windows, want 10: %+v", own, c.Windows)
	}
	// 用得最多的那个摆着，用得最少的那个没摆。
	if _, ok := windowOf(c, "号14 · Credits"); !ok {
		t.Errorf("windows = %+v, want the account that used the most", c.Windows)
	}
	if _, ok := windowOf(c, "号00 · Credits"); ok {
		t.Errorf("windows = %+v, want the account that used the least to be left out", c.Windows)
	}
}

// TestTheSummarySaysHowManyAccountsThereAre says the card says how many
// accounts are behind it.
func TestTheSummarySaysHowManyAccountsThereAre(t *testing.T) {
	c := Of(
		[]store.UsageSnapshot{
			{AccountID: "acc_01", HasAmounts: true, Granted: 400, Remaining: 400},
			{AccountID: "acc_02", HasAmounts: true, Granted: 400, Remaining: 400},
		},
		[]store.Account{
			{ID: "acc_01", Name: "号01"},
			{ID: "acc_02", Name: "号02"},
			{ID: "acc_03", Name: "号03"},
		},
		nil,
	)
	if got := summaryText(c); got != "3 个账号 · 2 有读数 · 1 没读过" {
		t.Errorf("the summary says %q, want %q", got, "3 个账号 · 2 有读数 · 1 没读过")
	}
}

// TestTheProtocolsAreOnTheCard says the card says which protocols the
// gateway speaks.
//
// 「能用哪些协议」这件事要摆在明处：客户端（Claude Code、Codex、脚本）
// 直接指网关的时候按自己的协议说话，magpie 跟网关讲的是 Anthropic。
func TestTheProtocolsAreOnTheCard(t *testing.T) {
	c := Of(
		[]store.UsageSnapshot{{AccountID: "acc_01", HasAmounts: true, Granted: 400, Remaining: 400}},
		[]store.Account{{ID: "acc_01", Name: "主号"}},
		nil,
	)
	w, ok := windowOf(c, "协议")
	if !ok {
		t.Fatalf("windows = %+v, want a 协议 window", c.Windows)
	}
	if w.Display != "Anthropic · Chat Completions · Responses" {
		t.Errorf("display = %q, want %q", w.Display, "Anthropic · Chat Completions · Responses")
	}
	if !w.Aside {
		t.Error("the 协议 window is not aside: it has no share of its own")
	}
	if w.Used != nil {
		t.Errorf("the 协议 window says it used %v: it does not know", *w.Used)
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
		nil,
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
		nil,
	)
	if len(c.Windows) != 2 {
		t.Fatalf("windows = %d, want 2", len(c.Windows))
	}
	w := c.Windows[0]
	if w.Amount == nil || *w.Amount != 0.05 {
		t.Errorf("amount = %v, want 0.05", w.Amount)
	}
	if w.Used == nil || *w.Used != 0.01 {
		t.Errorf("used = %v, want 0.01", w.Used)
	}
	if w.RunsOutAt == nil {
		t.Error("the window says when the credits run out: it does not")
	}
}

// summaryText gives the text of the card's 账号 window.
func summaryText(c Card) string {
	for _, w := range c.Windows {
		if w.Name == "账号" {
			return w.Display
		}
	}
	return ""
}

// windowOf gives the window of that name, if the card has one.
func windowOf(c Card, name string) (Window, bool) {
	for _, w := range c.Windows {
		if w.Name == name {
			return w, true
		}
	}
	return Window{}, false
}

// TestTheCardSaysTheAccountIsUsedUp says an account that is marked
// spent says so on the card, and its credits are not counted.
//
// 上游拒了它：网关不会再把它给任何客户端。卡上还摆着它上一次读到的
// 437.5 积分，人就以为那积分还在 —— 卡上说的话，网关得兑现。
func TestTheCardSaysTheAccountIsUsedUp(t *testing.T) {
	ws, _ := json.Marshal([]account.Window{
		{Name: "5 hours", ModelName: "general", Used: 100},
	})
	c := Of(
		[]store.UsageSnapshot{{
			AccountID: "acc_01", HasAmounts: true,
			Granted: 500, Consumed: 62.5, Remaining: 437.5,
			WindowsJSON: string(ws), CreatedAt: time.Now(),
		}},
		[]store.Account{{ID: "acc_01", Name: "主号"}},
		[]store.Exhausted{{
			AccountID: "acc_01", WindowKey: "general",
			Until: time.Now().Add(time.Hour), Reason: "quota",
		}},
	)
	if c.Balance != "0 credits" {
		t.Errorf("balance = %q, want %q: the account is out, it has no credits to spend",
			c.Balance, "0 credits")
	}
	for _, w := range c.Windows {
		if w.Name == "协议" {
			continue
		}
		if w.Display != "用完" {
			t.Errorf("the window %q says %q, want %q", w.Name, w.Display, "用完")
		}
	}
}

// TestTheCardWithoutASpentMarkIsUnchanged says an account that is not
// marked spent is said as it was read.
//
// 没有标记的账号跟从前一样：它的数就是它上一次读到的数。
func TestTheCardWithoutASpentMarkIsUnchanged(t *testing.T) {
	ws, _ := json.Marshal([]account.Window{
		{Name: "5 hours", ModelName: "general", Used: 12.5},
	})
	c := Of(
		[]store.UsageSnapshot{{
			AccountID: "acc_01", HasAmounts: true,
			Granted: 500, Consumed: 62.5, Remaining: 437.5,
			WindowsJSON: string(ws), CreatedAt: time.Now(),
		}},
		[]store.Account{{ID: "acc_01", Name: "主号"}},
		nil,
	)
	if c.Balance != "437.5 credits" {
		t.Errorf("balance = %q, want %q", c.Balance, "437.5 credits")
	}
	for _, w := range c.Windows {
		if w.Display == "用完" {
			t.Errorf("the window %q says it is used up: there is no mark", w.Name)
		}
	}
}

// TestTheExpiredSpentMarkIsNotUsedUp says a mark whose time has passed
// does not say the account is used up.
//
// 时刻过了的标记不是标记了：那个组已经回来，账号回到轮转里（规格 §4.5
// 的恢复点 A）。卡上不能还写着用完。
func TestTheExpiredSpentMarkIsNotUsedUp(t *testing.T) {
	c := Of(
		[]store.UsageSnapshot{{
			AccountID: "acc_01", HasAmounts: true,
			Granted: 500, Consumed: 62.5, Remaining: 437.5,
			CreatedAt: time.Now(),
		}},
		[]store.Account{{ID: "acc_01", Name: "主号"}},
		[]store.Exhausted{{
			AccountID: "acc_01", WindowKey: "general",
			Until: time.Now().Add(-time.Hour), Reason: "quota",
		}},
	)
	if c.Balance != "437.5 credits" {
		t.Errorf("balance = %q, want %q: the mark is over", c.Balance, "437.5 credits")
	}
	for _, w := range c.Windows {
		if w.Display == "用完" {
			t.Errorf("the window %q says it is used up: the mark is over", w.Name)
		}
	}
}

// TestTheGroupThatIsNotSpentKeepsItsNumbers says a mark for one group
// does not mark the other groups.
//
// 标记的粒度是「账号 × 窗口组」：别的组没有用完，那些窗口照旧摆着数。
func TestTheGroupThatIsNotSpentKeepsItsNumbers(t *testing.T) {
	ws, _ := json.Marshal([]account.Window{
		{Name: "5 hours", ModelName: "general", Used: 100},
		{Name: "5 hours video", ModelName: "video", Used: 10},
	})
	c := Of(
		[]store.UsageSnapshot{{
			AccountID: "acc_01", HasAmounts: true,
			Granted: 500, Consumed: 62.5, Remaining: 437.5,
			WindowsJSON: string(ws), CreatedAt: time.Now(),
		}},
		[]store.Account{{ID: "acc_01", Name: "主号"}},
		[]store.Exhausted{{
			AccountID: "acc_01", WindowKey: "general",
			Until: time.Now().Add(time.Hour), Reason: "quota",
		}},
	)
	general, video := "", ""
	for _, w := range c.Windows {
		switch w.Name {
		case "5 hours":
			general = w.Display
		case "5 hours video":
			video = w.Display
		}
	}
	if general != "用完" {
		t.Errorf("the general window says %q, want %q", general, "用完")
	}
	if video == "用完" {
		t.Errorf("the video window says it is used up: only the general group is")
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

	"github.com/lingoo12138/mmxcode/internal/account"
	"github.com/lingoo12138/mmxcode/internal/store"
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
// 没有自己端点，所以这个钩子得由网关自己顶上来：给 magpie 一个 magpie
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

	"github.com/lingoo12138/mmxcode/internal/card"
)

// usageCard says the gateway's usage in magpie's card shape.
//
// magpie 的 provider 插件有一个 usage() 钩子，卡片就是从那里来的：magpie
// 照着 plan、until、balance、windows 这几个字段画。我们是自己的端点，那
// 个钩子得由网关自己顶上来 —— 给 magpie 一个 magpie 认得的形状，它就能
// 把我们这些账号的积分画出来。
//
// 卡片说的是上一次读到的数（调度器每 usage.refresh.scan_every 扫一遍，
// 默认 30 秒；把它写成 0 才回到老的 schedule.usage_refresh），后台页面上
// 也有一个「刷新」按钮。要现读一遍就在地址上加 refresh=1：那一遍只现读
// 最热的几个账号 —— 卡片等不了所有账号的账。
//
// 这个接口在聊天那扇门后面：手里有那把 key 的（聊天用的、或者后台登录
// 用的）都能问。问不到的就是没带 key。
func (s *Server) usageCard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if r.URL.Query().Get("refresh") == "1" && s.Mgr != nil {
		// 卡片那条路只现读最热的几个：卡片永远几秒内回来。其余的账
		// 用上一次读到的数。
		res, err := s.round().RefreshHot(ctx)
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
	// 标记也摆进去：被记了「额度用完」的账号，卡上要写着用完，它的积分也
	// 不算在余额里 —— 网关不会再把它给客户端，卡上不能还许着积分。
	spent, err := s.Store.ListExhausted(ctx)
	if err != nil {
		writeError(w, protocolOf(r.URL.Path), http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(card.Of(snaps, accounts, spent))
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

