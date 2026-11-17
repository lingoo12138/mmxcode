package pool

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"mmxcode/internal/store"
)

// anAccount is an account that is signed in.
func anAccount(id string) store.Account {
	return store.Account{ID: id, Name: id, Status: store.StatusActive}
}

// TestThePoolTakesAccountsUntilItIsFull says the pool has an upper
// bound.
//
// 池的上界就是它存在的理由之一：库里的号再多，池里也只有那么多。
func TestThePoolTakesAccountsUntilItIsFull(t *testing.T) {
	p := New(2)
	if !p.Admit(Entry{Account: anAccount("a")}) {
		t.Fatal("the first account did not go in")
	}
	if !p.Admit(Entry{Account: anAccount("b")}) {
		t.Fatal("the second account did not go in")
	}
	if p.Admit(Entry{Account: anAccount("c")}) {
		t.Fatal("a full pool took another account")
	}
	if p.Len() != 2 {
		t.Fatalf("len = %d, want 2", p.Len())
	}
	if !p.Full() {
		t.Fatal("the pool is full")
	}
}

// TestAPoolWithoutACapTakesEverything says a capacity of zero means no
// limit.
func TestAPoolWithoutACapTakesEverything(t *testing.T) {
	p := New(0)
	for i := 0; i < 100; i++ {
		if !p.Admit(Entry{Account: anAccount(string(rune('a'+i%26)) + string(rune('0'+i/26)))}) {
			t.Fatalf("account %d did not go in", i)
		}
	}
	if p.Full() {
		t.Fatal("a pool without a cap is full")
	}
}

// TestAnAccountThatIsInThePoolIsNotTakenTwice says an account that is
// already in the pool keeps its place.
//
// 池里的号带着它读到的积分：换掉它就是把刚读到的数扔了。
func TestAnAccountThatIsInThePoolIsNotTakenTwice(t *testing.T) {
	p := New(4)
	if !p.Admit(Entry{Account: anAccount("a"), Remaining: 400, Known: true}) {
		t.Fatal("the account did not go in")
	}
	if p.Admit(Entry{Account: anAccount("a"), Remaining: 1, Known: true}) {
		t.Fatal("the account went in twice")
	}
	e, ok := p.Get("a")
	if !ok {
		t.Fatal("the account is not in the pool")
	}
	if e.Remaining != 400 {
		t.Fatalf("remaining = %v, want 400", e.Remaining)
	}
}

// TestASpentAccountIsNotUsable says an account whose window is spent
// cannot answer.
//
// 它的窗口什么时候回来，标记说了算：还在将来，它就接不了活（规格
// §4.2）。
func TestASpentAccountIsNotUsable(t *testing.T) {
	p := New(4)
	p.Admit(Entry{Account: anAccount("a")})
	p.MarkSpent("a", "general", time.Now().Add(time.Hour), "quota")

	if p.Usable(time.Now()) != 0 {
		t.Fatalf("usable = %d, want 0", p.Usable(time.Now()))
	}
	spent := p.Spent()
	if len(spent) != 1 || spent[0].AccountID != "a" {
		t.Fatalf("spent = %v, want just a", spent)
	}
}

// TestASpentAccountWhoseTimeHasComeIsUsableAgain says a spent account
// comes back when its window comes back.
func TestASpentAccountWhoseTimeHasComeIsUsableAgain(t *testing.T) {
	p := New(4)
	p.Admit(Entry{Account: anAccount("a")})
	p.MarkSpent("a", "general", time.Now().Add(-time.Minute), "quota")

	if p.Usable(time.Now()) != 1 {
		t.Fatalf("usable = %d, want 1", p.Usable(time.Now()))
	}
}

// TestAnAccountThatIsSignedOutIsNotUsable says an account that is not
// signed in cannot answer.
func TestAnAccountThatIsSignedOutIsNotUsable(t *testing.T) {
	p := New(4)
	p.Admit(Entry{Account: store.Account{ID: "a", Status: store.StatusSignedOut}})

	if p.Usable(time.Now()) != 0 {
		t.Fatalf("usable = %d, want 0", p.Usable(time.Now()))
	}
}

// TestTheCreditsOfAnAccountThatWasNeverReadSayNothing says the credits
// that were never read say nothing.
//
// 不是 0 —— 0 是「读到了，没有了」（规格不变量 2）。
func TestTheCreditsOfAnAccountThatWasNeverReadSayNothing(t *testing.T) {
	p := New(4)
	p.Admit(Entry{Account: anAccount("a")})

	e, ok := p.Get("a")
	if !ok {
		t.Fatal("the account is not in the pool")
	}
	if e.Known {
		t.Fatal("the credits of an account that was never read cannot be known")
	}
	if e.Remaining != 0 {
		t.Fatalf("remaining = %v, want 0", e.Remaining)
	}
}

// TestThePoolGivesItsAccounts says the pool gives the accounts it holds,
// and taking one out takes it away from the list.
func TestThePoolGivesItsAccounts(t *testing.T) {
	p := New(4)
	p.Admit(Entry{Account: anAccount("a")})
	p.Admit(Entry{Account: anAccount("b")})

	if len(p.Accounts()) != 2 {
		t.Fatalf("accounts = %d, want 2", len(p.Accounts()))
	}
	if !p.Remove("a") {
		t.Fatal("the account was not in the pool")
	}
	if p.Remove("a") {
		t.Fatal("the account was removed twice")
	}
	accounts := p.Accounts()
	if len(accounts) != 1 || accounts[0].ID != "b" {
		t.Fatalf("accounts = %v, want just b", accounts)
	}
	if _, ok := p.Get("a"); ok {
		t.Fatal("the removed account is still in the pool")
	}
}

// TestTheCreditsOfAnAccountAreWritten says the credits that were read
// later are written into the pool.
func TestTheCreditsOfAnAccountAreWritten(t *testing.T) {
	p := New(4)
	p.Admit(Entry{Account: anAccount("a")})
	p.SetRemaining("a", 123, true)

	e, ok := p.Get("a")
	if !ok {
		t.Fatal("the account is not in the pool")
	}
	if !e.Known || e.Remaining != 123 {
		t.Fatalf("entry = %+v, want 123 known", e)
	}
}

// TestThePoolGivesItsAccountsInTheOrderTheyWentIn says the pool hands out
// its accounts — and its entries — in the order they went in.
//
// 顺序是给「轮转」用的：map 的顺序是随机的，随机的顺序让轮转不是轮转 ——
// 冷号（先进来的）该先被挑。名单和条目是同一套顺序：两套顺序会让挑号的人
// 不知道自己在走哪一套。
func TestThePoolGivesItsAccountsInTheOrderTheyWentIn(t *testing.T) {
	p := New(4)
	for _, id := range []string{"a", "b", "c"} {
		p.Admit(Entry{Account: anAccount(id)})
	}

	accounts := p.Accounts()
	if len(accounts) != 3 {
		t.Fatalf("accounts = %d, want 3", len(accounts))
	}
	for i, want := range []string{"a", "b", "c"} {
		if accounts[i].ID != want {
			t.Fatalf("accounts = %v, want a, b, c", accounts)
		}
	}

	entries := p.Entries()
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(entries))
	}
	for i, want := range []string{"a", "b", "c"} {
		if entries[i].Account.ID != want {
			t.Fatalf("entries = %v, want a, b, c", entries)
		}
	}
}

// TestAnAccountThatComesBackGoesToTheEnd says an account that left the
// pool and came back goes to the end of the order.
//
// 它刚用过：排到队尾，下一轮轮转才轮到它。
func TestAnAccountThatComesBackGoesToTheEnd(t *testing.T) {
	p := New(4)
	p.Admit(Entry{Account: anAccount("a")})
	p.Admit(Entry{Account: anAccount("b")})
	p.Remove("a")
	p.Admit(Entry{Account: anAccount("a")})

	accounts := p.Accounts()
	if len(accounts) != 2 {
		t.Fatalf("accounts = %d, want 2", len(accounts))
	}
	if accounts[0].ID != "b" || accounts[1].ID != "a" {
		t.Fatalf("accounts = %v, want b then a", accounts)
	}
}

// TestThePoolIsSafeWhileItIsUsed says the pool can be read and written at
// the same time.
//
// 聊天那条路读它，后台补号那条路写它 —— 两边同时。这个包里说的「并发
// 安全」得有一道测试钉住，不然那只是注释里的一句话。
//
// 就算没有 -race（这台机器上没有 C 编译器），Go 自己会喊：并发读写同一
// 张 map 是 fatal error，跑一次就够。
func TestThePoolIsSafeWhileItIsUsed(t *testing.T) {
	p := New(64)
	stop := make(chan struct{})
	var wg sync.WaitGroup

	// 一边写：进池、写余额、标记用完、出池。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			id := fmt.Sprintf("acc_%d", i%32)
			p.Admit(Entry{Account: anAccount(id)})
			p.SetRemaining(id, float64(i), true)
			p.MarkSpent(id, "general", time.Now().Add(time.Hour), "quota")
			p.Remove(id)
		}
	}()

	// 一边读：拿账号、看余额、数能答的。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			p.Get("acc_1")
			p.Entries()
			p.IDs()
			p.Accounts()
			p.Spent()
			p.Usable(time.Now())
			p.Len()
			p.Full()
		}
	}()

	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// TestTheCapacityOfZeroMeansNoLimit says a capacity of zero is no limit,
// whatever is in the pool.
//
// 0 = 不限，是池空的时候也是池里有号的时候。同一个 0 在两种状态下是两种
// 意思的话，池会被永久冻住。
func TestTheCapacityOfZeroMeansNoLimit(t *testing.T) {
	p := New(4)
	p.Admit(Entry{Account: anAccount("a")})
	p.Admit(Entry{Account: anAccount("b")})

	p.SetCapacity(0)

	if p.Capacity() != 0 {
		t.Fatalf("capacity = %d, want 0", p.Capacity())
	}
	if p.Full() {
		t.Fatal("the pool is full, want not full")
	}
	if !p.Admit(Entry{Account: anAccount("c")}) {
		t.Fatal("the account did not go into the pool")
	}
}

// TestTheCapacityDoesNotTakeAccountsOut says a smaller capacity does not
// throw accounts away.
//
// 缩容请不走任何号：请出去的理由只有一个 —— 额度用完了（规格 §4.4）。池里
// 超了就是超了，等号一个个地被用完。
func TestTheCapacityDoesNotTakeAccountsOut(t *testing.T) {
	p := New(4)
	p.Admit(Entry{Account: anAccount("a")})
	p.Admit(Entry{Account: anAccount("b")})

	p.SetCapacity(1)

	if p.Len() != 2 {
		t.Fatalf("pool = %d, want 2", p.Len())
	}
	if p.Capacity() != 1 {
		t.Fatalf("capacity = %d, want 1", p.Capacity())
	}
}

// TestTheSpentListDropsTheExpiredOnes says a spent window that has come
// back is not in the spent list.
//
// 窗口回来了，那个号就回来了（规格 §4.5）：名单里还留着它，挑号就会跳过
// 一个能用的号。
func TestTheSpentListDropsTheExpiredOnes(t *testing.T) {
	p := New(4)
	p.Admit(Entry{Account: anAccount("a")})
	p.MarkSpent("a", "general", time.Now().Add(-time.Minute), "quota")

	if spent := p.Spent(); len(spent) != 0 {
		t.Fatalf("spent = %v, want none", spent)
	}
}

// TestTheNamesAreCopies says the caller cannot break the pool by changing
// the list it was given.
//
// 顺序是轮转靠的东西：rotator 拿着名单从队首走到队尾，冷号先被挑。谁改
// 了顺序，轮转就不是轮转了。
func TestTheNamesAreCopies(t *testing.T) {
	p := New(4)
	for _, id := range []string{"a", "b", "c"} {
		p.Admit(Entry{Account: anAccount(id)})
	}

	accounts := p.Accounts()
	accounts[0], accounts[2] = accounts[2], accounts[0]
	accounts[1].Status = store.StatusSignedOut

	again := p.Accounts()
	if again[0].ID != "a" || again[1].ID != "b" || again[2].ID != "c" {
		t.Fatalf("accounts = %v, want a, b, c", again)
	}
	if again[1].Status != store.StatusActive {
		t.Fatalf("status = %q, want %q", again[1].Status, store.StatusActive)
	}
}
