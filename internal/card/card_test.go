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
// 读不到的计划不是一个没有计划：说 Free 是说它没有，说都��说才是真的
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
			{AccountID: "acc_01", HasAmounts: true, Granted: 500, Remaining: 400},
			{AccountID: "acc_02", HasAmounts: true, Granted: 300, Remaining: 250},
		},
		[]store.Account{
			{ID: "acc_01", Name: "主号"},
			{ID: "acc_02", Name: "副号"},
		},
		nil,
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
	if w.Used == nil || *w.Used != 0.01 {
		t.Errorf("used = %v, want 0.01", w.Used)
	}
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
