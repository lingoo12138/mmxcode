# magpie 插件打磨（地址、id、协议；卡片瘦身）实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 插件签入的时候能填**网关地址**和**账号 id**；「能用哪些协议」摆在明处（卡片上一行、README 一节、provider 名）；额度卡片从 98 个窗口压到十几个。

**Architecture:** 网关侧改 `internal/card`（卡片的形状：全池一行 + 账号一行 + 有话说的账号最多十个 + 协议一行）；插件侧改 `packages/mmxcode/index.mjs`（签入提示、账号命名、provider 名）。插件的 provider 还是**一个**、**一种协议** —— 那是宿主的规矩（实测见下），magpie 自己会翻译，所以不需要第二个 provider。

**Tech Stack:** Go 1.26（`internal/card`）、Node ESM 无依赖（`packages/mmxcode`）、`node:test`

设计文档：`docs/superpowers/specs/2026-11-16-mmxcode-magpie-plugin-polish-design.md`

相关计划：`docs/superpowers/plans/2026-11-15-mmxcode-magpie-card.md`（卡片最初的形状）、`docs/superpowers/plans/2026-11-15-mmxcode-magpie-plugin.md`（插件最初的样子）

---

## 宿主的规矩（实测出来的，别猜）

这几条是在本机的 magpie 上跑出来的（方法见规格 §2）。照着它们做，别再试一遍：

| 规矩 | 是什么 |
|---|---|
| 插件的 provider 只有一个地址 | `config` 钩子里的 `api` 只能是一个字符串。给它 `{anthropic, chat, responses}`，多出来的地址没有 |
| 协议由 `npm` 定 | provider 级的 `npm` 决定宿主跟网关说什么协议。**每个模型自己的 `npm` 不算数** |
| 一个插件 = 一个能签入的 provider | `auths()` 把 provider id 映射到签入钩子。插件 provider 的 id 是插件定的，宿主不改，也没有改名的地方 |
| Image #1 那个协议下拉是宿主给自建 provider 的 | `providers.json` 那种。插件 provider 只有一个地址，没有下拉 |
| magpie 自己会翻译 | 拿 `chat/completions` 的请求去问 `mmxcode` 的模型，magpie 翻成 Anthropic 再问网关，答 200 |
| 账号的名字由我们给 | magpie 用 `accountId`，没有就 `metadata.email`。都不给就显示「API key」 |

结论：**一个 provider 就够**。magpie 跟网关讲 Anthropic（Claude Code 的原生协议，不用翻译），别的客户端 magpie 自己翻。

## 三条不变量

1. **没读到的不是零。** 读不到的账号不算进 `balance`，也不写 `used` —— 指针是空的就别写这个字段。
2. **卡片是 magpie 的形状。** `plan`、`until`、`balance`、`windows`。我们自己多知道的收在 `mmxcode` 那一个名字下面。
3. **插件里没有 MiniMax 的账号、令牌和签名。** 插件只认得网关的地址和一把聊天用的 key。

---

### Task 1: 卡片的形状（瘦身）

**Files:**
- Modify: `internal/card/card.go`
- Test: `internal/card/card_test.go`

卡上摆的行是有限的。网关后面有 98 个号，每个号摆一行「xxx@hotmail.com · Credits 0 / 400」，卡片就没法看了 —— 一片一样的行，什么话也没说。所以：

- 全池的积分摆一行（**Credits**）；
- 账号有几个、有几个读到、几个没上卡，摆一行（**账号**，没有 `used`：这不是一个「用掉多少」的窗口）；
- 只有「有话说」的账号才各有自己的窗口（用过积分的、用完的），用得最多的排前面，最多十个；
- 账号的计划窗口照旧摆着；
- 最后一行摆**协议**：网关讲哪几个协议。

- [ ] **Step 1: 写失败的测试**

`internal/card/card_test.go`（整个文件）：

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

新加的那几个测试说的是哪几件事：

| 测试 | 说的是 |
|---|---|
| `TestThePoolIsOnOneWindow` | 全池的积分摆一行：「150 / 800」，份额 18.75 |
| `TestTheAccountsThatUsedNothingAreNotOnTheCard` | 用都没用的账号不摆自己的窗口：二十个号只用三行（全池、账号、协议） |
| `TestTheAccountsThatUsedSomethingAreOnTheCard` | 用过积分的账号有自己的窗口：「号02 · Credits」 |
| `TestAtMostTenAccountsAreOnTheCard` | 摆的账号最多十个，用得最多的在前面 |
| `TestTheSummarySaysHowManyAccountsThereAre` | 「3 个账号 · 2 有读数 · 1 没读过」 |
| `TestTheProtocolsAreOnTheCard` | 协议那一行：「Anthropic · Chat Completions · Responses」，没有 `used` |

- [ ] **Step 2: 跑测试，确认是红的**

```sh
go test ./internal/card/
```

Expected: FAIL —— 「the pool is on one window」（还没有全池那一行）、「the protocols are on the card」（还没有协议那一行）、「the accounts that used nothing are not on the card」（窗口数不对）。

- [ ] **Step 3: 实现**

`internal/card/card.go`（整个文件）：

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

- [ ] **Step 4: 跑测试，确认是绿的**

```sh
gofmt -l internal/card
go test ./internal/card/
```

Expected: `gofmt` 没有输出；`ok mmxcode/internal/card`。

- [ ] **Step 5: 提交**

```sh
git add internal/card/card.go internal/card/card_test.go
git commit -F .git/commit_msg.txt
```

---

### Task 2: 插件 —— 地址、id、协议说清楚

**Files:**
- Modify: `packages/mmxcode/index.mjs`
- Test: `packages/mmxcode/index.test.mjs`

三件事：

- 签入的第三个提示改说「账号 id」：它是 magpie 里显示这个账号的名字；
- 账号名 = 用户给的 id，**没给就用网关地址**（账号列表里一眼看得见是哪个网关）；
- provider 名 `MMXCode` → `MMXCode 网关`。

- [ ] **Step 1: 写失败的测试**

`packages/mmxcode/index.test.mjs`（整个文件）：

```js
import test from "node:test"
import assert from "node:assert/strict"
import http from "node:http"

import { MMXCodePlugin } from "./index.mjs"

// aGateway starts a fake gateway and gives its address.
//
// 关的时候要把还开着的连接掐掉：fetch 的连接是留着的，不掐的话
// close() 等着它们，这个测试就挂着不走了。
async function aGateway(t, handler) {
  const srv = http.createServer(handler)
  await new Promise((r) => srv.listen(0, "127.0.0.1", r))
  t.after(() => new Promise((r) => {
    srv.closeAllConnections()
    srv.close(r)
  }))
  return `http://127.0.0.1:${srv.address().port}`
}

// thePlugin gives a loaded plugin.
async function thePlugin() {
  return await MMXCodePlugin({})
}

// TestTheSignInSaysTheGatewayIsNotThere says an unreachable gateway is
// said in words.
//
// 签入不成功要说清楚，不留一个半签入的账号。
test("the sign-in says the gateway is not there", async () => {
  const plugin = await thePlugin()
  const res = await plugin.auth.methods[0].authorize({ base: "http://127.0.0.1:1" })
  assert.equal(res.type, "failed")
  assert.match(res.error, /网关连不上/)
})

// TestTheSignInSaysTheKeyWasRefused says a refused key is said in
// words.
//
// 网关开的是哪扇门：聊天接口要聊天 key，后台登录 key 也行。
test("the sign-in says the key was refused", async (t) => {
  const base = await aGateway(t, (req, res) => {
    res.writeHead(401)
    res.end("{}")
  })
  const plugin = await thePlugin()
  const res = await plugin.auth.methods[0].authorize({ base, key: "sk-wrong" })
  assert.equal(res.type, "failed")
  assert.match(res.error, /网关拒绝了这把 key/)
})

// TestTheSignInSaysTheOtherAnswers says an unexpected answer is said
// as it is.
test("the sign-in says the other answers", async (t) => {
  const base = await aGateway(t, (req, res) => {
    res.writeHead(503)
    res.end("{}")
  })
  const plugin = await thePlugin()
  const res = await plugin.auth.methods[0].authorize({ base })
  assert.equal(res.type, "failed")
  assert.match(res.error, /HTTP 503/)
})

// TestTheSignInKeepsTheAddress says a sign-in that worked keeps the
// address.
//
// 地址是签到的一部分：没有它，插件不知道往哪儿发请求。
test("the sign-in keeps the address", async (t) => {
  const base = await aGateway(t, (req, res) => {
    res.writeHead(200, { "content-type": "application/json" })
    res.end(JSON.stringify({ object: "list", data: [] }))
  })
  const plugin = await thePlugin()
  const res = await plugin.auth.methods[0].authorize({ base })
  assert.equal(res.type, "success")
  assert.equal(res.metadata.base, base)
})

// TestTheSignInSaysTheGatewayWantsAKey says a gateway that wants a key
// is said in words.
//
// 网关一把 key 都没有的时候它自己认：聊天接口不鉴权，答的是 200。所以
// 401 只可能是"网关有 key，而这次没给对"—— 少填了，或者填错了。签入
// 不该把它当成功：magpie 会把这样的账号记成签入失效，用户连原因都
// 看不到。
test("the sign-in says the gateway wants a key", async (t) => {
  const base = await aGateway(t, (req, res) => {
    res.writeHead(401)
    res.end("{}")
  })
  const plugin = await thePlugin()
  const res = await plugin.auth.methods[0].authorize({ base })
  assert.equal(res.type, "failed")
  assert.match(res.error, /网关要一把 key/)
})

// TestTheSignInWithoutAKeyIsFine says a gateway that wants no key is
// signed in without one.
//
// 网关一把 key 都没有的时候它自己认：那把 key 可以不填，签入照样成。
test("the sign-in without a key is fine", async (t) => {
  const base = await aGateway(t, (req, res) => {
    res.writeHead(200, { "content-type": "application/json" })
    res.end(JSON.stringify({ object: "list", data: [] }))
  })
  const plugin = await thePlugin()
  const res = await plugin.auth.methods[0].authorize({ base })
  assert.equal(res.type, "success")
  assert.equal(res.key, undefined)
})

// TestTheAccountIsNamedAfterTheAddress says an account with no name of
// its own is named after the gateway's address.
//
// magpie 给账号起名字用的是 accountId，没有就 metadata.email。不给名字
// 的时候，地址就是那个名字 —— magpie 的账号列表里就看得见是哪个网关。
test("the account is named after the address", async (t) => {
  const base = await aGateway(t, (req, res) => {
    res.writeHead(200, { "content-type": "application/json" })
    res.end(JSON.stringify({ object: "list", data: [] }))
  })
  const plugin = await thePlugin()
  const res = await plugin.auth.methods[0].authorize({ base })
  assert.equal(res.type, "success")
  assert.equal(res.metadata.email, base)
})

// TestTheAccountIsNamedAfterTheNameTheUserGave says the name the user
// gave is the account's name.
test("the account is named after the name the user gave", async (t) => {
  const base = await aGateway(t, (req, res) => {
    res.writeHead(200, { "content-type": "application/json" })
    res.end(JSON.stringify({ object: "list", data: [] }))
  })
  const plugin = await thePlugin()
  const res = await plugin.auth.methods[0].authorize({ base, label: "自建网关" })
  assert.equal(res.type, "success")
  assert.equal(res.metadata.email, "自建网关")
})

// TestTheSignInAsksForTheAccountsId says the sign-in asks for the
// account's id.
//
// 「加接口地址和 id」：地址是网关在哪儿，id 是这个账号在 magpie 里叫什么。
// 两个都在签入表单里问。
test("the sign-in asks for the account's id", async () => {
  const plugin = await thePlugin()
  const method = plugin.auth.methods[0]
  const label = method.prompts.find((p) => p.key === "label")
  assert.ok(label, "the account's id is not asked for")
  assert.match(label.message, /账号/)
  assert.equal(label.required, false)
})

// TestTheProviderSaysItIsTheGateway says the provider says what it is.
test("the provider says it is the gateway", async () => {
  const plugin = await thePlugin()
  const config = {}
  await plugin.config(config)
  assert.equal(config.provider.mmxcode.name, "MMXCode 网关")
})

// TestTheCardIsHandedOverAsItIs says the card comes from the gateway
// as it is.
//
// 插件不自己算，也不在卡上写读到的时间：多出来的东西塞进去只会让
// magpie 认错字段。
test("the card is handed over as it is", async (t) => {
  const card = {
    plan: "Token Plan M3",
    until: "2026-12-01T00:00:00Z",
    balance: "437.5 credits",
    windows: [{ name: "主号 · Credits", used: 12.5, display: "62.5 / 500", amount: 62.5, limit: 500, unit: "credits" }],
  }
  const base = await aGateway(t, (req, res) => {
    res.writeHead(200, { "content-type": "application/json" })
    res.end(JSON.stringify(card))
  })
  const plugin = await thePlugin()
  const got = await plugin.auth.usage(async () => ({ type: "api", key: "sk-x", metadata: { base } }))
  assert.equal(got.plan, card.plan)
  assert.equal(got.balance, card.balance)
  assert.equal(got.windows.length, 1)
  assert.equal(got.windows[0].name, card.windows[0].name)
  // 卡片是网关那张：插件一个字也不加。
  assert.deepEqual(got, card)
})

// TestTheModelsAreTheGateways says the models are the gateway's own,
// with what magpie needs to know about them.
test("the models are the gateway's", async (t) => {
  const base = await aGateway(t, (req, res) => {
    res.writeHead(200, { "content-type": "application/json" })
    res.end(
      JSON.stringify({
        object: "list",
        data: [
          {
            id: "MiniMax-M3.1-Flash",
            limit: { context: 1048576, output: 524288 },
            modalities: { input: ["text", "image"], output: ["text"] },
            reasoning: true,
            thinking: "forced_on",
            efforts: ["low", "high"],
          },
        ],
      }),
    )
  })
  const plugin = await thePlugin()
  const provider = { id: "mmxcode", models: {} }
  const got = await plugin.provider.models(provider, { auth: { type: "api", key: "sk-x", metadata: { base } } })
  const m = got["MiniMax-M3.1-Flash"]
  assert.ok(m, "the model is not in the list")
  assert.equal(m.limit.context, 1048576)
  assert.equal(m.limit.output, 524288)
  assert.equal(m.capabilities.reasoning, true)
  assert.equal(m.capabilities.input.image, true)
  assert.equal(m.api.npm, "@ai-sdk/anthropic")
  assert.equal(m.api.url, base + "/v1")
  assert.ok(m.variants.low, "the reasoning levels are not said")
})

// TestTheNoneEffortDoesNotCoverTurningThinkingOff says a "none" in the
// effort list does not take the "turn thinking off" level away.
//
// 名单里的 "none" 和 "default" 是占位，不是档位：滤掉它们，"none" 这
// 一档才只可能是"把思考关掉"。不然选 none 的人以为关了思考，其实关
// 的是"adaptive 思考 + effort=none"。
test("the 'none' effort does not cover turning thinking off", async (t) => {
  const base = await aGateway(t, (req, res) => {
    res.writeHead(200, { "content-type": "application/json" })
    res.end(
      JSON.stringify({
        object: "list",
        data: [{ id: "MiniMax-M3", thinking: "switchable", efforts: ["none", "default", "high"] }],
      }),
    )
  })
  const plugin = await thePlugin()
  const provider = { id: "mmxcode", models: {} }
  const got = await plugin.provider.models(provider, { auth: { type: "api", key: "sk-x", metadata: { base } } })
  const v = got["MiniMax-M3"].variants
  // 选 none 是把思考关掉：没有 effort 跟着。
  assert.deepEqual(v.none, { thinking: { type: "disabled" } })
  assert.equal(v.none.effort, undefined)
  assert.deepEqual(v.high, { thinking: { type: "adaptive" }, effort: "high" })
  // "default" 是占位：它不是一档。
  assert.equal(v.default, undefined)
})

// TestTheModelsFallBackWhenTheGatewayIsSilent says the built-in list
// is used when the gateway cannot be asked.
//
// 跟参考插件一个做法：magpie 手里那份名单留着。
test("the models fall back when the gateway is silent", async (t) => {
  const base = await aGateway(t, (req, res) => {
    res.writeHead(500)
    res.end("{}")
  })
  const plugin = await thePlugin()
  const provider = { id: "mmxcode", models: { "MiniMax-M3.1-Flash": { id: "MiniMax-M3.1-Flash" } } }
  const got = await plugin.provider.models(provider, { auth: { type: "api", key: "sk-x", metadata: { base } } })
  assert.deepEqual(Object.keys(got), ["MiniMax-M3.1-Flash"])
})

// TestTheConfigPutsTheProvider says the config hook puts the provider
// there, with the built-in list.
test("the config puts the provider there", async () => {
  const plugin = await thePlugin()
  const config = {}
  await plugin.config(config)
  const p = config.provider.mmxcode
  assert.ok(p, "the provider is not there")
  assert.equal(p.npm, "@ai-sdk/anthropic")
  assert.equal(p.api, "http://127.0.0.1:8080/v1")
  assert.ok(p.models["MiniMax-M3.1-Flash"], "the built-in list is not there")
})

// TestTheConfigModelsAreConfigShaped says the config hook's models are
// the shape the host reads, not the whole provider row.
//
// 宿主照着配置形状里的名字读：reasoning、tool_call、attachment、
// modalities、limit、cost、variants。塞一整行进去，这些名字就没人读
// 了 —— 图读不了、思考也没有。参考插件的 configModels() 也是这样
// （minimax/index.mjs:203-220），它那 whole row 是 modelOf()
// （:222-246），走的是另一个钩子。
test("the config's models are in the shape the host reads", async () => {
  const plugin = await thePlugin()
  const config = {}
  await plugin.config(config)
  const m = config.provider.mmxcode.models["MiniMax-M3.1-Flash"]
  assert.ok(m, "the model is not in the config")
  assert.equal(m.name, "M3.1-Flash")
  assert.equal(m.reasoning, true)
  assert.equal(m.tool_call, true)
  assert.equal(m.attachment, true)
  assert.deepEqual(m.modalities, { input: ["text", "image", "video"], output: ["text"] })
  assert.deepEqual(m.limit, { context: 1048576, output: 524288 })
  assert.deepEqual(m.cost, { input: 0, output: 0, cache_read: 0, cache_write: 0 })
  assert.ok(m.variants.low, "the reasoning levels are not said")
  assert.deepEqual(m.variants.low, { thinking: { type: "adaptive" }, effort: "low" })
  // 不是 provider 的整行：整行里那些名字宿主不读。
  assert.equal(m.id, undefined)
  assert.equal(m.api, undefined)
  assert.equal(m.capabilities, undefined)
})

// TestTheConfigModelsSayTheThinkingThatCanBeTurnedOff says a model
// whose thinking can be turned off has that said.
//
// 思考能关的模型有 none 这一档，能关才关得掉。
test("the config's models say the thinking that can be turned off", async () => {
  const plugin = await thePlugin()
  const config = {}
  await plugin.config(config)
  const m = config.provider.mmxcode.models["MiniMax-M3"]
  assert.ok(m, "the model is not in the config")
  assert.deepEqual(m.variants.none, { thinking: { type: "disabled" } })
  assert.ok(m.variants.high, "the reasoning levels are not said")
  // 强制开着的模型没有 none：那档它做不到。
  const flash = config.provider.mmxcode.models["MiniMax-M3.1-Flash"]
  assert.equal(flash.variants.none, undefined)
})

// TestTheSignInAsksForTheAddressAndTheKey says the sign-in method asks
// for the gateway's address and the chat key itself.
//
// magpie 调 authorize(inputs) 时给的是提示的答案：没有提示，地址就
// 只能从环境变量里来，用户连网关在哪儿都答不了。所以这方法是
// keyless 的，提示里必须有地址。
test("the sign-in asks for the address and the key", async () => {
  const plugin = await thePlugin()
  const method = plugin.auth.methods[0]
  assert.equal(method.keyless, true)
  assert.ok(Array.isArray(method.prompts), "the sign-in asks for nothing")
  const base = method.prompts.find((p) => p.key === "base")
  assert.ok(base, "the gateway's address is not asked for")
  assert.match(base.message, /网关地址/)
  const key = method.prompts.find((p) => p.key === "key")
  assert.ok(key, "the chat key is not asked for")
  // 网关一把 key 都没有的时候它自己认：这把 key 可以不填。
  assert.equal(key.required, false)
})

// TestTheSignInTakesTheAnswers says the sign-in takes the answers to
// its prompts.
//
// 提示的答案就是 authorize 的入参：地址在 base 上，key 在 key 上。
// 少了这一条，签到就不知道该往哪儿打。
test("the sign-in takes the answers to its prompts", async (t) => {
  const base = await aGateway(t, (req, res) => {
    res.writeHead(200, { "content-type": "application/json" })
    res.end(JSON.stringify({ object: "list", data: [] }))
  })
  const plugin = await thePlugin()
  const method = plugin.auth.methods[0]
  const answers = {}
  for (const p of method.prompts) answers[p.key] = p.key === "base" ? base : ""
  answers.key = "sk-x"
  const res = await method.authorize(answers)
  assert.equal(res.type, "success")
  assert.equal(res.metadata.base, base)
})

// TestTheSessionIdIsGivenToTheGateway says the conversation's id goes
// to the gateway.
//
// 有了它，一段对话黏在一个账号上；没有它，两个用户的第一句话一样就
// 会撞成一个会话。
test("the session id is given to the gateway", async () => {
  const plugin = await thePlugin()
  const out = { headers: {} }
  await plugin["chat.headers"]({ model: { providerID: "mmxcode" }, sessionID: "sess-1" }, out)
  assert.equal(out.headers["X-Session-Id"], "sess-1")
})

// TestTheSessionIdIsOnlyForOurProvider says another provider's request
// is left alone.
test("the session id is only for our provider", async () => {
  const plugin = await thePlugin()
  const out = { headers: {} }
  await plugin["chat.headers"]({ model: { providerID: "somebody-else" }, sessionID: "sess-1" }, out)
  assert.equal(out.headers["X-Session-Id"], undefined)
})

// TestTheLoaderGivesTheGateway says the loader points at the gateway
// with the key.
test("the loader gives the gateway", async () => {
  const plugin = await thePlugin()
  const got = await plugin.auth.loader(async () => ({
    type: "api",
    key: "sk-x",
    metadata: { base: "http://127.0.0.1:9999" },
  }))
  assert.equal(got.baseURL, "http://127.0.0.1:9999/v1")
  assert.equal(got.apiKey, "sk-x")
  assert.equal(got.headers["x-api-key"], "sk-x")
})
```

- [ ] **Step 2: 跑测试，确认是红的**

```sh
node --test packages/mmxcode/index.test.mjs
```

Expected: FAIL —— 「the account is named after the address」（账号名不是地址）、「the provider says it is the gateway」（provider 还叫 MMXCode）。

注意：`node --test packages/mmxcode/` 只会跑一个文件，要点名文件。

- [ ] **Step 3: 实现**

`packages/mmxcode/index.mjs`（整个文件）：

```js
// mmxcode: magpie 的 provider 插件，聊天和余额卡片都指向自己的网关。
//
// 插件里没有 MiniMax 的账号、令牌和签名 —— 那些都在网关里。插件只认
// 得两样东西：网关的地址，和一把聊天用的 key。
//
// 它跟 magpie 自带的 minimax-code-global 的分别：那个插件 magpie 自
// 己拿着账号的令牌去问 MiniMax，账号一多卡片就卡死；我们把查询都留
// 在网关里，卡片是一个请求。

const PROVIDER = "mmxcode"
const NPM = "@ai-sdk/anthropic"
const DEFAULT_BASE = "http://127.0.0.1:8080"
const BASE_ENV = "MMXCODE_BASE"
const KEY_ENV = "MMXCODE_KEY"

// The models the gateway is known to have, for when it cannot be asked.
//
// 兜底的那一份：网关答得出的时候用它自己的（`GET /v1/models`）。
const MODELS = [
  { id: "MiniMax-M3.1-Flash", name: "M3.1-Flash", context: 1048576, output: 524288, inputs: ["text", "image", "video"], thinking: "forced_on", efforts: ["low", "medium", "high", "xhigh", "max"] },
  { id: "MiniMax-M3", name: "M3", context: 1048576, output: 524288, inputs: ["text", "image", "video"], thinking: "switchable", efforts: [] },
]

// baseOf gives the gateway's address.
//
// 签入的时候问的地址存进了签到的 metadata；没有就问环境变量；再没有
// 就是本机的那个默认地址。末尾的斜杠去掉：底座后面还要接路径。
// 空着的地址算是没有：提示里写着默认值，答个空就用它。
const baseOf = (a) => {
  const b = a?.base ?? a?.metadata?.base ?? process.env[BASE_ENV] ?? DEFAULT_BASE
  const s = String(b ?? "").trim()
  return (s || DEFAULT_BASE).replace(/\/+$/, "")
}

// keyOf gives the chat key.
//
// magpie 自己问用户要那把 key，存成签到的 key；签入的时候给的（提示里
// 的或环境变量里的）也算。
const keyOf = (a) => {
  const k = a?.metadata?.key ?? a?.key ?? process.env[KEY_ENV] ?? ""
  return typeof k === "string" ? k.trim() : ""
}

// headersFor gives the headers the gateway wants.
//
// 网关认 x-api-key，也认 Authorization: Bearer。magpie 的 Anthropic
// SDK 发的就是 x-api-key。
const headersFor = (key) => {
  const h = {}
  if (key) h["x-api-key"] = key
  return h
}

// variantsOf are a model's reasoning levels, as @ai-sdk/anthropic options.
//
// "none" 是把思考关掉（模型自己说关得掉的时候）；其余的是思考的档位。
//
// 名单里的 "none" 和 "default" 是占位，不是档位，滤掉它们：不然
// efforts 里一个 "none" 就把关掉思考的那一档盖掉，变成"adaptive 思考
// + effort=none" —— 选它的人以为关了思考，其实没关。参考插件在读活
// 名单的时候就这么滤（minimax/index.mjs:266）。
function variantsOf(thinking, efforts) {
  const out = {}
  if (thinking === "switchable") out.none = { thinking: { type: "disabled" } }
  const levels = efforts.filter((e) => {
    const s = String(e).trim().toLowerCase()
    return s !== "none" && s !== "default"
  })
  if (levels.length) {
    for (const e of levels) out[e] = { thinking: { type: "adaptive" }, effort: e }
  } else if (thinking === "switchable") {
    out.high = { thinking: { type: "adaptive" } }
  }
  return out
}

// rowOf gives one model as magpie's provider wants it.
//
// 网关的名单带元数据：context/output、能不能读图、思考档位。magpie 要
// 的就是这些。
function rowOf(m, base) {
  const inputs = Array.isArray(m.modalities?.input) && m.modalities.input.length ? m.modalities.input : ["text"]
  const efforts = Array.isArray(m.efforts) ? m.efforts : []
  const thinking = typeof m.thinking === "string" ? m.thinking : "off"
  const limit = m.limit ?? {}
  const variants = variantsOf(thinking, efforts)
  return {
    id: m.id,
    providerID: PROVIDER,
    name: m.name ?? m.id,
    api: { id: m.id, url: base + "/v1", npm: NPM },
    status: "active",
    headers: {},
    options: {},
    cost: { input: 0, output: 0, cache: { read: 0, write: 0 } },
    limit: { context: limit.context ?? 0, output: limit.output ?? 0 },
    capabilities: {
      temperature: true,
      reasoning: m.reasoning === true || thinking !== "off",
      attachment: inputs.length > 1,
      toolcall: true,
      input: {
        text: true,
        image: inputs.includes("image"),
        audio: inputs.includes("audio"),
        video: inputs.includes("video"),
        pdf: inputs.includes("pdf"),
      },
      output: { text: true, image: false, audio: false, video: false, pdf: false },
      interleaved: false,
    },
    release_date: "",
    variants,
  }
}

// configRowOf gives one model as magpie's config wants it.
//
// The config hook's models are a smaller shape than the rows the models
// hook gives: the host reads the names it knows (reasoning, tool_call,
// attachment, modalities, limit, cost, variants) and fills the rest in
// itself. A full row loses them.
//
// 配的是配置形状：宿主照着这些名字读（reasoning、tool_call、
// attachment、modalities、limit、cost、variants），剩下的它自己补。塞
// 一整行进去，这些名字就没人读了 —— 图读不了、思考也没有。
function configRowOf(m) {
  const inputs = Array.isArray(m.inputs) && m.inputs.length ? m.inputs : ["text"]
  const efforts = Array.isArray(m.efforts) ? m.efforts : []
  return {
    name: m.name,
    reasoning: m.thinking !== "off",
    tool_call: true,
    attachment: inputs.length > 1,
    modalities: { input: inputs, output: ["text"] },
    limit: { context: m.context ?? 0, output: m.output ?? 0 },
    cost: { input: 0, output: 0, cache_read: 0, cache_write: 0 },
    variants: variantsOf(m.thinking, efforts),
  }
}

// configRowsOf gives the built-in models as config entries.
function configRowsOf(models) {
  const out = {}
  for (const m of models) out[m.id] = configRowOf(m)
  return out
}

// modelsOf asks the gateway for its model list.
//
// 答不出来的时候是空的：调用方摆兜底的那一份。
async function modelsOf(base, key) {
  try {
    const res = await globalThis.fetch(base + "/v1/models", { headers: headersFor(key) })
    if (!res.ok) return []
    const body = await res.json()
    return Array.isArray(body?.data) ? body.data : []
  } catch {
    return []
  }
}

// authorize tries the gateway once, so that a sign-in that cannot work
// says why.
//
// 网关一把 key 都没有的时候它自己认：聊天接口不鉴权，答的是 200 —— 那
// 把 key 可以不填。网关有 key 的时候，没给对就答 401：那说明这次没给
// 上，签入不能算成功（magpie 会把这样的账号记成签入失效，用户连原因
// 都看不到）。
async function authorize(base, key) {
  try {
    const res = await globalThis.fetch(base + "/v1/models", { headers: headersFor(key) })
    if (res.ok) return { type: "success", key: key || undefined, metadata: { base } }
    if (res.status === 401 || res.status === 403) {
      if (key) {
        return { type: "failed", error: "网关拒绝了这把 key —— 它开的是哪扇门？聊天接口要聊天 key，后台登录 key 也行" }
      }
      return { type: "failed", error: "网关要一把 key：聊天接口要聊天 key（后台 /keys 申请的），后台登录 key 也行" }
    }
    return { type: "failed", error: `网关答了 HTTP ${res.status}` }
  } catch (e) {
    return { type: "failed", error: `网关连不上：${base}（${e?.message ?? e}）` }
  }
}

// MMXCodePlugin is the plugin.
//
// 它是一个导出的异步函数：magpie 的宿主调它一次，拿到钩子。
export const MMXCodePlugin = async ({ client, magpie } = {}) => {
  return {
    // the provider, its package and its models
    config: async (config) => {
      config.provider ??= {}
      config.provider[PROVIDER] ??= {}
      const p = config.provider[PROVIDER]
      p.name ??= "MMXCode 网关"
      // 网关讲 Anthropic 的 /v1/messages
      p.npm ??= NPM
      p.api ??= DEFAULT_BASE + "/v1"
      p.models = { ...configRowsOf(MODELS), ...(p.models ?? {}) }
    },

    // the conversation's id, which the gateway binds to an account
    "chat.headers": async (input, output) => {
      const id = input?.model?.providerID ?? input?.provider?.info?.id
      if (id !== PROVIDER || !input?.sessionID) return
      // 网关认的头是 X-Session-Id / X-Conversation-Id / X-Session-Id-Codex
      // / Session-Id，第一个优先。有了它，一段对话黏在一个账号上。
      output.headers["X-Session-Id"] = input.sessionID
    },

    provider: {
      id: PROVIDER,
      // the gateway's own list, when it answers; else the list above
      async models(provider, { auth } = {}) {
        const base = baseOf(auth)
        const list = await modelsOf(base, keyOf(auth))
        if (!list.length) return provider.models
        const rows = {}
        for (const m of list) rows[m.id] = rowOf(m, base)
        return { ...provider.models, ...rows }
      },
    },

    auth: {
      provider: PROVIDER,
      methods: [
        {
          // We ask for the address and the key ourselves.
          //
          // magpie 自己不问 key（keyless），我们问：网关的地址和聊天用的
          // 那把 key 都是提示的答案，authorize 拿得到，才谈得上"打一次
          // 网关，说清它在不在、它收不收"。没有提示，authorize 拿到的
          // 永远是空对象，地址只能从环境变量里来。
          type: "api",
          label: "MMXCode 网关",
          keyless: true,
          placeholder: "sk-…（网关一把 key 都没有的时候可以留空）",
          prompts: [
            { type: "text", key: "base", message: "网关地址（默认 http://127.0.0.1:8080）" },
            { type: "password", key: "key", required: false, message: "聊天用的 key（后台 /keys 申请的；网关一把 key 都没有的时候可以留空）" },
            { type: "text", key: "label", required: false, message: "账号 id（magpie 里显示的名字；留空就用网关地址）" },
          ],
          async authorize(inputs = {}) {
            const base = baseOf(inputs)
            const key = keyOf(inputs)
            const label = typeof inputs?.label === "string" ? inputs.label.trim() : ""
            const res = await authorize(base, key)
            if (res.type !== "success") return res
            // magpie names an account by accountId, else by metadata.email:
            // the label the user gave is the name it will know it by.
            // magpie 给账号起名字用的是 accountId，没有就 metadata.email
            // —— 用户给的那个名字得落在 email 上，账号才认得出来。没给
            // 名字就用网关地址：账号列表里一眼看得见是哪个网关。
            return { ...res, metadata: { ...res.metadata, email: label || base } }
          },
        },
      ],

      // what each request goes out with
      async loader(getAuth) {
        const auth = await getAuth()
        const base = baseOf(auth)
        const key = keyOf(auth)
        return {
          // The Anthropic SDK appends /messages to it.
          baseURL: base + "/v1",
          apiKey: key,
          headers: headersFor(key),
        }
      },

      // the gateway's card: the credits and the plan of every account
      // behind it, in one answer
      async usage(getAuth) {
        const auth = await getAuth()
        const base = baseOf(auth)
        const key = keyOf(auth)
        try {
          // refresh=1 是要现读。网关那一侧只现读最热的几个，有预算，
          // 所以卡片不会等。
          const res = await globalThis.fetch(base + "/v1/usage?refresh=1", { headers: headersFor(key) })
          if (!res.ok) return { error: `网关答了 HTTP ${res.status}` }
          return await res.json()
        } catch (e) {
          return { error: e?.message ?? String(e) }
        }
      },
    },
  }
}

export default MMXCodePlugin
```

三处改动：

1. `baseOf` 的兜底顺序：**签入的值 → 环境变量 → 默认**。环境变量不能排在签入的值前面 —— 签入的时候填的地址是用户选的，环境变量是兜底。
2. 提示里第三个改成「账号 id（magpie 里显示的名字；留空就用网关地址）」。
3. `authorize` 答的 metadata 里 `email` 永远是账号名：`label || base`。

- [ ] **Step 4: 跑测试，确认是绿的**

```sh
node --test packages/mmxcode/index.test.mjs
```

Expected: `pass 22 fail 0`。

- [ ] **Step 5: 提交**

```sh
git add packages/mmxcode/index.mjs packages/mmxcode/index.test.mjs
git commit -F .git/commit_msg.txt
```

---

### Task 3: README 说清地址、id、协议、卡片

**Files:**
- Modify: `packages/mmxcode/README.md`

- [ ] **Step 1: 写**

三节：

- **签入的时候问三样**：网关地址、聊天用的 key、账号 id（留空就用地址当名字）；
- **协议**：网关讲 Anthropic / Chat Completions / Responses 三种；magpie 跟网关讲 Anthropic，别的客户端 magpie 自己翻，直接指网关的客户端三种都收；
- **卡片**：全池的积分一行、账号一行、有话说的账号最多十个、协议一行；没读到的写成「没读到」，不是「0」。

- [ ] **Step 2: 提交**

```sh
git add packages/mmxcode/README.md
git commit -F .git/commit_msg.txt
```

---

### Task 4: 本机验一遍

- [ ] **Step 1: 网关侧**

```sh
go build ./...
go test ./...
```

Expected: 全绿。

- [ ] **Step 2: 装插件，重新签入**

```sh
magpie plugin add ./packages/mmxcode
magpie plugin login mmxcode
```

签入的时候：地址填网关地址，key 填后台申请的那把（网关一把 key 都没有就留空），**账号 id 留空** —— magpie 的账号列表里显示的应该是那个地址。

- [ ] **Step 3: 卡片**

```sh
magpie provider test mmxcode
```

Expected: 过。卡片上是十几行，不是 98 行。

---

### Task 5: 自建 provider —— 能改地址、密钥、协议的那一页

**Files:**
- Modify: `packages/mmxcode/README.md`（把命令和两个供应商的分工写进去）
- Modify: `docs/superpowers/specs/2026-11-16-mmxcode-magpie-plugin-polish-design.md`（§2.4、§3.4）

插件的 provider 页面上不会有「接口地址 / 接口密钥」：宿主把插件 provider 的地址换成 `plugin://`，编辑器那一支就没有地址和密钥（规格 §2.4）。要 Image #1 那一页，网关得当**自建 provider**。

- [ ] **Step 1: 加**

```sh
magpie provider add mmxcode-direct id=mmxcode-direct "name=MMXCode 网关（直连）" \
  anthropic=http://127.0.0.1:8080 \
  chat=http://127.0.0.1:8080/v1 \
  responses=http://127.0.0.1:8080/v1 \
  key=sk-admin \
  models.url=http://127.0.0.1:8080/v1/models \
  balance="http://127.0.0.1:8080/v1/usage?refresh=1" balance.path=balance
```

- 三个协议地址都填：填了 ≥2 个，编辑器的 Base URL 才有分协议的标签页，每把 key 旁边也才有协议下拉。
- `id=` 显式给：不给的话宿主从名字推，`MMXCode 网关` 已经被插件 provider 占了，会推成 `mmxcode-2`。
- 插件 provider 不撤：卡片和会话黏性在它那儿。

- [ ] **Step 2: 验**

```sh
magpie provider test mmxcode-direct
magpie quota mmxcode-direct
```

Expected: 三个协议都答（chat / responses / anthropic）；余额读得到。

- [ ] **Step 3: 提交**

```sh
git add packages/mmxcode/README.md docs/superpowers/specs/2026-11-16-mmxcode-magpie-plugin-polish-design.md
git commit -F .git/commit_msg.txt
```

---

## 实测（本机，2026-11-16）

`magpie quota mmxcode` 答的卡片（98 个号的池子）：

```
mmxcode · Free · http://127.0.0.1:8080  subscription  Credits 2% (746.72 / 39600)
  账号 98 个账号 · 98 有读数
  578078539524956167 Credits 87% (697.63 / 800)
  BurnsSchwinn94062@hotmail.com Credits 8% (31.54 / 400)
  ...
  协议 Anthropic · Chat Completions · Responses
  38853.28 credits left
```

- **98 个窗口 → 11 个**（全池一行、账号一行、七个有话说的账号、协议一行）。
- 协议那一行画出来了：`协议 Anthropic · Chat Completions · Responses`（`aside` 的窗口，magpie 照样画，只是没有份额）。
- provider 名：`magpie providers` 里是 `MMXCode 网关`，`signed in as http://127.0.0.1:8080`。
- 账号名：签入时**账号 id 留空**，magpie 的账号列表里显示的是那个地址。
- `magpie provider test mmxcode` 过：`✓ anthropic 847 ms · MiniMax-M3.1-Flash`。

自建 provider（Task 5）：

```
magpie providers
  MMXCode 网关（直连） custom  mmxcode-direct  127.0.0.1:8080  ● ••••••••  2 of 2 models
  MMXCode 网关            mmxcode              mmxcode            ● signed in as http://127.0.0.1:8080  2 of 2 models

magpie provider test mmxcode-direct
  ✓ chat       998 ms · MiniMax-M3.1-Flash
  ✓ responses  980 ms · MiniMax-M3.1-Flash
  ✓ anthropic 1246 ms · MiniMax-M3.1-Flash

magpie quota mmxcode-direct
  mmxcode-direct  balance  78037.1 credits left
```

- 编辑器（点那一行右边的 `>`）真的有 Image #1 那一页：**Base URL**（分协议标签页：OpenAI 兼容 / OpenAI Responses / Anthropic 兼容 / Gemini 兼容 / System One）、**账号**（密钥那一行 + 协议下拉「任意协议」+ 并发不限 + 「添加另一个密钥」）、**模型**（从网关 `/v1/models` 拉来的 M3.1-Flash / M3）、**高级**（请求头、额外请求体、透传响应头、代理）。
- 三个协议地址都填了，所以协议下拉才有得选。
- 直连这条路**不带会话 id**（前面架记路径的代理，只看见 Authorization / X-Api-Key，没有 `X-Session-Id`）—— 所以卡片和会话黏性还是靠插件那个 provider。

还剩的：

1. 最多十个账号合不合手：现在是十个（98 个号里七个有过话，其余的没摆）。要改就改 `internal/card` 里的 `shownAccounts`。
2. 想要账号名叫「生产网关」这样的，就签入的时候填账号 id —— 那样它就是一个名字，不是地址。
3. 两个 provider 都在模型列表里（各一份网关的模型），agent 挑哪个由它自己定。
