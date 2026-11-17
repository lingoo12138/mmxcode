package pool

import (
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
	p.MarkSpent("a", "general", time.Now().Add(time.Hour))

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
	p.MarkSpent("a", "general", time.Now().Add(-time.Minute))

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
