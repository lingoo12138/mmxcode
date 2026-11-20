package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"mmxcode/internal/apikey"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	return openTestOpts(t)
}

// openTestOpts opens a store for a test, with the options it is given.
//
// 有些测试要看的是「库是照什么开的」：那条会 fsync 的连接在不在，主连接是
// 不是 normal —— 这些都是打开的时候定下来的。
func openTestOpts(t *testing.T, opts ...Option) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"), opts...)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// makeAccount puts an account in the store.
//
// The children say which account they belong to, so a child without an
// account is refused.
func makeAccount(t *testing.T, s *Store, id string) {
	t.Helper()
	err := s.CreateAccount(context.Background(), Account{
		ID: id, Name: id, Status: StatusActive, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestTwoTransactionsThatLookUpTheSameEmailMakeOneRow says a lookup
// and an insert inside one transaction are one thing to everybody.
//
// 两个并发的导入：都按邮箱找，都没有就建一行。真库的事务一开始就把
// 写锁拿着，所以第二个进来的那个根本进不来——它等第一个提交完，才看得见
// 第一个建好的那一行。不然同一个站点账号会有两行都是活的。
//
// 这一条守的是**先后**，不是等多久：第一个事务查完邮箱之后先把事务开
// 着不放，第二个事务来敲门（它该在这期间被写锁挡在门外），然后第一个
// 才建行、提交。第二个事务的事务体因此只能在第一个提交之后才跑，它看
// 到的必须是第一个建好的那一行。写锁要是没生效，两个事务就会同时查到
// 「没有」，建出两行来——那时候记下来的先后就不对了。
//
// 靠「等半秒钟没动静」是看不出写锁有没有生效的：两个线程谁先跑到哪一
// 步是碰运气的事，慢的那一个可能压根没赶上。
func TestTwoTransactionsThatLookUpTheSameEmailMakeOneRow(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now()

	// 第一个事务查完邮箱了。starting：第二个事务要敲门了。
	holding := make(chan struct{})
	starting := make(chan struct{})
	// events 记着事情发生的先后。
	var mu sync.Mutex
	var events []string
	note := func(e string) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, e)
	}
	take := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), events...)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := s.Tx(ctx, func(tx Tx) error {
			_, err := tx.AccountByEmail(ctx, "a@example.com")
			if !errors.Is(err, ErrNotFound) {
				return fmt.Errorf("the account is already there: %v", err)
			}
			note("first looked")
			close(holding)
			// 等第二个事务来敲门，然后给它一点时间去撞那把锁：撞上的那
			// 一下要在这之前发生，不然这一条就什么也没验到。
			<-starting
			time.Sleep(200 * time.Millisecond)
			acc := Account{
				ID: "acc_1", Name: "a@example.com", Email: "a@example.com",
				Status: StatusActive, CreatedAt: now, UpdatedAt: now,
			}
			if err := tx.CreateAccount(ctx, acc); err != nil {
				return err
			}
			note("first made")
			return tx.SaveToken(ctx, Token{
				AccountID: acc.ID, Access: "tok", Refresh: "ref",
				ExpiresAt: now.Add(time.Hour), UpdatedAt: now,
			})
		})
		if err != nil {
			t.Errorf("the first transaction: %v", err)
		}
	}()
	<-holding

	wg.Add(1)
	go func() {
		defer wg.Done()
		// 敲门：这一下之后，这个事务就该去撞写锁了。
		close(starting)
		err := s.Tx(ctx, func(tx Tx) error {
			acc, err := tx.AccountByEmail(ctx, "a@example.com")
			if errors.Is(err, ErrNotFound) {
				note("second made one of its own")
				acc = Account{
					ID: "acc_2", Name: "a@example.com", Email: "a@example.com",
					Status: StatusActive, CreatedAt: now, UpdatedAt: now,
				}
				if err := tx.CreateAccount(ctx, acc); err != nil {
					return err
				}
				return tx.SaveToken(ctx, Token{
					AccountID: acc.ID, Access: "tok", Refresh: "ref",
					ExpiresAt: now.Add(time.Hour), UpdatedAt: now,
				})
			}
			if err != nil {
				return err
			}
			note("second found the row")
			return nil
		})
		if err != nil {
			t.Errorf("the second transaction: %v", err)
		}
	}()
	wg.Wait()

	// The order is the point: the second one looked after the first one
	// had made its row, and it found that row.
	want := []string{"first looked", "first made", "second found the row"}
	got := take()
	if len(got) != len(want) {
		t.Fatalf("what happened = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("what happened = %v, want %v", got, want)
		}
	}

	rows, err := s.ListAccounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("accounts = %d, want one row", len(rows))
	}
	if rows[0].ID != "acc_1" {
		t.Fatalf("the row is %q, want the one the first transaction made", rows[0].ID)
	}
}

// TestAnAccountIsFoundByItsSubject says the token's subject is a key.
//
// 令牌里带的那个 sub 是站点账号自己的 id：一个站点账号一个，重登多少次
// 都是它。去重靠它，才不会在清单上的地址和站点那边报的地址不一样的时候
// 建出第二行。
func TestAnAccountIsFoundByItsSubject(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now()

	if err := s.CreateAccount(ctx, Account{
		ID: "acc_1", Name: "a", Email: "a@example.com", Status: StatusActive,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveToken(ctx, Token{
		AccountID: "acc_1", Access: "tok", Refresh: "ref",
		ExpiresAt: now.Add(time.Hour), Subject: "sub-1", UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	acc, err := s.AccountBySubject(ctx, "sub-1")
	if err != nil {
		t.Fatal(err)
	}
	if acc.ID != "acc_1" {
		t.Fatalf("account = %q, want acc_1", acc.ID)
	}
	if _, err := s.AccountBySubject(ctx, "sub-2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if _, err := s.AccountBySubject(ctx, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// TestTheAccountFoundByItsSubjectIsTheOldestOne says which row comes
// when two rows carry the same subject.
//
// 同一个 subject 有两行，说明库里本来就有同一个站点账号的两行（老数
// 据）。取哪一行是要说好的：取先建的那一行——后来那一行可能是哪一次
// 出错的导入留下的，那一行的令牌未必对。
func TestTheAccountFoundByItsSubjectIsTheOldestOne(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now()

	// 后建的那一行先拿到手，为的是看清「先建的」说的是创建时间，不
	// 是碰巧。
	if err := s.CreateAccount(ctx, Account{
		ID: "acc_2", Name: "b", Email: "b@example.com", Status: StatusActive,
		CreatedAt: now.Add(time.Minute), UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveToken(ctx, Token{
		AccountID: "acc_2", Access: "tok2", Refresh: "ref2",
		ExpiresAt: now.Add(time.Hour), Subject: "sub-1", UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateAccount(ctx, Account{
		ID: "acc_1", Name: "a", Email: "a@example.com", Status: StatusActive,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveToken(ctx, Token{
		AccountID: "acc_1", Access: "tok", Refresh: "ref",
		ExpiresAt: now.Add(time.Hour), Subject: "sub-1", UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	acc, err := s.AccountBySubject(ctx, "sub-1")
	if err != nil {
		t.Fatal(err)
	}
	if acc.ID != "acc_1" {
		t.Fatalf("account = %q, want the one that was made first", acc.ID)
	}
}

// TestASignedOutAccountIsStillFoundByItsSubject says signing an
// account out does not lose the key that says which site account it is.
//
// 签出是要收走令牌的：站点不认这个登录了，这份凭据没有用。可是令牌行
// 里的 subject 得留着——那是站点那边账号自己的 id，是「这个站点账号在
// 库里是哪一行」的键。签出的账号恰恰是最要紧的那一个：操作的人看见它
// 掉线了，第一件事就是重新导入它。键要是被抹掉，那一次导入就会当成
// 新账号，再插一行——两行都是活的，轮转会挑到两次。
func TestASignedOutAccountIsStillFoundByItsSubject(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now()

	if err := s.CreateAccount(ctx, Account{
		ID: "acc_1", Name: "a", Email: "a@example.com", Status: StatusActive,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveToken(ctx, Token{
		AccountID: "acc_1", Access: "tok", Refresh: "ref",
		ExpiresAt: now.Add(time.Hour), Subject: "sub-1", UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	written, err := s.SignOutIfTheTokenIsStillThere(ctx, "acc_1", "tok", "the site refused it", now)
	if err != nil {
		t.Fatal(err)
	}
	if !written {
		t.Fatal("the sign-out was not written")
	}
	a, err := s.GetAccount(ctx, "acc_1")
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != StatusSignedOut {
		t.Fatalf("status = %q, want %q", a.Status, StatusSignedOut)
	}
	tok, err := s.GetToken(ctx, "acc_1")
	if err != nil {
		t.Fatal(err)
	}
	if tok.Access != "" || tok.Refresh != "" {
		t.Fatalf("the token = %+v, want it taken away", tok)
	}
	if tok.Subject != "sub-1" {
		t.Fatalf("subject = %q, want it kept", tok.Subject)
	}
	acc, err := s.AccountBySubject(ctx, "sub-1")
	if err != nil {
		t.Fatalf("the signed-out account is not found by its subject: %v", err)
	}
	if acc.ID != "acc_1" {
		t.Fatalf("account = %q, want acc_1", acc.ID)
	}
}

// TestASignOutDoesNotUndoAFreshSignIn says a sign-out is only written
// while the token in hand is still the one in the store.
//
// 手里这份令牌的续期被站点拒了，可是这期间另一个进程把这账号登回来
// 了：库里换成了新令牌。这时候把签出写下去，就是把别人刚登好的账号
// 作废掉，还把那份新令牌收走——页面说这个账号登出了，轮转池也不用它，
// 而它手里握着一份能用的凭据。
func TestASignOutDoesNotUndoAFreshSignIn(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now()

	if err := s.CreateAccount(ctx, Account{
		ID: "acc_1", Name: "a", Email: "a@example.com", Status: StatusActive,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveToken(ctx, Token{
		AccountID: "acc_1", Access: "old", Refresh: "ref",
		ExpiresAt: now.Add(time.Hour), Subject: "sub-1", UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	// 另一个进程登回来了。
	if err := s.SaveToken(ctx, Token{
		AccountID: "acc_1", Access: "fresh", Refresh: "freshref",
		ExpiresAt: now.Add(2 * time.Hour), Subject: "sub-1", UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	written, err := s.SignOutIfTheTokenIsStillThere(ctx, "acc_1", "old", "the site refused it", now)
	if err != nil {
		t.Fatal(err)
	}
	if written {
		t.Fatal("the sign-out was written even though the account was signed in again")
	}
	a, err := s.GetAccount(ctx, "acc_1")
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != StatusActive {
		t.Fatalf("status = %q, want %q", a.Status, StatusActive)
	}
	tok, err := s.GetToken(ctx, "acc_1")
	if err != nil {
		t.Fatal(err)
	}
	if tok.Access != "fresh" || tok.Refresh != "freshref" {
		t.Fatalf("the token = %+v, want the one the other sign-in wrote", tok)
	}
}

// TestTheErrorStateIsAlsoConditional says the error state is only
// written while the token in hand is still the one in the store.
//
// 跟签出一样：这次失败说的是手里这份令牌，库里换成了别的，就是有人替
// 它续好了。把别人刚续好的账号写成出错，页面会说一个好好在用的账号有
// 毛病。
func TestTheErrorStateIsAlsoConditional(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now()

	if err := s.CreateAccount(ctx, Account{
		ID: "acc_1", Name: "a", Email: "a@example.com", Status: StatusActive,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveToken(ctx, Token{
		AccountID: "acc_1", Access: "old", Refresh: "ref",
		ExpiresAt: now.Add(time.Hour), Subject: "sub-1", UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveToken(ctx, Token{
		AccountID: "acc_1", Access: "fresh", Refresh: "freshref",
		ExpiresAt: now.Add(2 * time.Hour), Subject: "sub-1", UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	written, err := s.MarkErrorIfTheTokenIsStillThere(ctx, "acc_1", "old", "the answer was lost", now)
	if err != nil {
		t.Fatal(err)
	}
	if written {
		t.Fatal("the error was written even though the account was signed in again")
	}
	a, err := s.GetAccount(ctx, "acc_1")
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != StatusActive {
		t.Fatalf("status = %q, want %q", a.Status, StatusActive)
	}
}

// TestASignInLandsOnTheRowThatIsAlreadyThere says a sign-in does not
// make a second row for one site account.
//
// 一个站点账号在库里只能有一行。这一行多半是批量导入进来的：登录又登
// 了一遍，是把那一行改好，不是再插一行——同一个站点账号两行都是活的，
// 轮转会挑到两次，页面把它列两遍，而且其中一个的令牌迟早被另一个的重
// 登作废。
func TestASignInLandsOnTheRowThatIsAlreadyThere(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now()

	acc, err := s.SignIn(ctx, Account{
		ID: "acc_1", Name: "the first one", UserID: "u1", Email: "a@example.com",
		Status: StatusActive, CreatedAt: now, UpdatedAt: now,
	}, Token{
		Access: "tok", Refresh: "ref", ExpiresAt: now.Add(time.Hour),
		Subject: "sub-1", UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if acc.ID != "acc_1" {
		t.Fatalf("account = %+v, want the one that was made", acc)
	}
	if acc.Name != "the first one" {
		t.Fatalf("name = %q, want the one that was given", acc.Name)
	}

	// 又登了一遍：同一行。
	again, err := s.SignIn(ctx, Account{
		ID: "acc_2", Name: "", UserID: "u1", Email: "a@example.com",
		Status: StatusActive, CreatedAt: now, UpdatedAt: now,
	}, Token{
		Access: "tok2", Refresh: "ref2", ExpiresAt: now.Add(2 * time.Hour),
		Subject: "sub-1", UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != "acc_1" {
		t.Fatalf("account = %q, want the row that was already there", again.ID)
	}
	rows, err := s.ListAccounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("accounts = %v, want one row for one site account", rows)
	}
	if rows[0].Name != "the first one" {
		t.Fatalf("name = %q, want the label that was there", rows[0].Name)
	}
	tok, err := s.GetToken(ctx, "acc_1")
	if err != nil {
		t.Fatal(err)
	}
	if tok.Access != "tok2" {
		t.Fatalf("the token = %+v, want the new one", tok)
	}
}

// TestTheAddressIsWrittenWithoutTheSpacesAroundIt says the address is
// stored trimmed.
//
// 地址是去重的键之一：同一个地址写成两种样子（一边带空格一边不带），
// 库里就会看成两个账号。
func TestTheAddressIsWrittenWithoutTheSpacesAroundIt(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now()

	if err := s.CreateAccount(ctx, Account{
		ID: "acc_1", Name: "one", Email: "  a@example.com  ",
		Status: StatusActive, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.AccountByEmail(ctx, "a@example.com")
	if err != nil {
		t.Fatalf("the account by its address: %v", err)
	}
	if got.Email != "a@example.com" {
		t.Fatalf("email = %q, want it trimmed", got.Email)
	}

	// 登录那一路也一样：地址两头带着空格，落库的时候得去掉。
	if _, err := s.SignIn(ctx, Account{
		ID: "acc_2", Name: "two", Email: "  b@example.com  ",
		Status: StatusActive, CreatedAt: now, UpdatedAt: now,
	}, Token{
		Access: "tok", Refresh: "ref", ExpiresAt: now.Add(time.Hour),
		Subject: "sub-2", UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	got2, err := s.AccountByEmail(ctx, "b@example.com")
	if err != nil {
		t.Fatalf("the account by its address: %v", err)
	}
	if got2.Email != "b@example.com" {
		t.Fatalf("email = %q, want it trimmed", got2.Email)
	}
}

// TestASignInOfAnotherAccountMakesItsOwnRow says two site accounts are
// two rows.
func TestASignInOfAnotherAccountMakesItsOwnRow(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now()

	for i, sub := range []string{"sub-1", "sub-2"} {
		if _, err := s.SignIn(ctx, Account{
			ID: "acc_" + sub, Name: sub, UserID: "u" + sub, Email: sub + "@example.com",
			Status: StatusActive, CreatedAt: now, UpdatedAt: now,
		}, Token{
			Access: "tok", Refresh: "ref", ExpiresAt: now.Add(time.Hour),
			Subject: sub, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
		_ = i
	}
	rows, err := s.ListAccounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("accounts = %v, want two rows", rows)
	}
}

// TestAMarkedSignedOutAccountIsNotMarkedAgain says the reason for a
// failed sign-in is only written while the account is still signed out.
//
// 另一个进程可能刚把这个账号登回去了，那一行现在是 active 了：这时候
// 把它写回 signed_out，就是把别人刚换好的令牌作废掉，还让这个账号显示
// 成登出的状态——手里却握着一份能用的凭据。
func TestAMarkedSignedOutAccountIsNotMarkedAgain(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now()

	if err := s.CreateAccount(ctx, Account{
		ID: "acc_1", Name: "a", Email: "a@example.com", Status: StatusSignedOut,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	written, err := s.MarkSignedOut(ctx, "acc_1", "the site refused the password", now)
	if err != nil {
		t.Fatal(err)
	}
	if !written {
		t.Fatal("the reason was not written down")
	}
	acc, err := s.GetAccount(ctx, "acc_1")
	if err != nil {
		t.Fatal(err)
	}
	if acc.Status != StatusSignedOut || !strings.Contains(acc.LastError, "refused") {
		t.Fatalf("account = %+v", acc)
	}

	// 别人把它登回去了。
	if err := s.UpdateAccountStatus(ctx, "acc_1", StatusActive, "", now); err != nil {
		t.Fatal(err)
	}
	written, err = s.MarkSignedOut(ctx, "acc_1", "the site refused the password", now)
	if err != nil {
		t.Fatal(err)
	}
	if written {
		t.Fatal("the account that is active was marked as signed out")
	}
	acc, err = s.GetAccount(ctx, "acc_1")
	if err != nil {
		t.Fatal(err)
	}
	if acc.Status != StatusActive {
		t.Fatalf("status = %q, want active", acc.Status)
	}
}

// TestATransactionThatFailsLeavesNothingBehind says a transaction that
// fails takes its writes back.
//
// 写失败的时候不能留半截子：读者会看见一行没有令牌的账号，那个账号
// 以后就登不回来了。
func TestATransactionThatFailsLeavesNothingBehind(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now()

	err := s.Tx(ctx, func(tx Tx) error {
		if err := tx.CreateAccount(ctx, Account{
			ID: "acc_1", Name: "a@example.com", Email: "a@example.com",
			Status: StatusActive, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			return err
		}
		return errors.New("the disk is full")
	})
	if err == nil {
		t.Fatal("no error")
	}
	rows, err := s.ListAccounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("accounts = %d, want none", len(rows))
	}
}

// TestATransactionThatIsCutShortHalfWayLeavesNothingBehind says a
// transaction that is cut short half way takes its writes back.
//
// 导入被掐断的时候，行写下去了、令牌没写下去：这样的半截子留在库里，
// 读者看见的是一个没有令牌的账号——那个账号以后就登不回来了。
//
// 回滚不用调用方的上下文：那时候它已经没了。
func TestATransactionThatIsCutShortHalfWayLeavesNothingBehind(t *testing.T) {
	s := openTest(t)
	ctx, cancel := context.WithCancel(context.Background())
	now := time.Now()

	err := s.Tx(ctx, func(tx Tx) error {
		if err := tx.CreateAccount(ctx, Account{
			ID: "acc_1", Name: "a@example.com", Email: "a@example.com",
			Status: StatusActive, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			return err
		}
		// 这一趟导入被掐断了：下一笔写不进去了。
		cancel()
		return tx.SaveToken(ctx, Token{
			AccountID: "acc_1", Access: "tok", Refresh: "ref",
			ExpiresAt: now.Add(time.Hour), UpdatedAt: now,
		})
	})
	if err == nil {
		t.Fatal("no error")
	}
	rows, err := s.ListAccounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("accounts = %d, want none", len(rows))
	}
}

func TestAccountRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	a := Account{
		ID:         "acc_01",
		Name:       "主号",
		UserID:     "1234567890",
		Email:      "a@example.com",
		Status:     StatusActive,
		ProxyGroup: "warp",
		ProxyURL:   "",
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}
	if err := s.CreateAccount(ctx, a); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetAccount(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if got.Email != "a@example.com" || got.ProxyGroup != "warp" || got.Status != StatusActive {
		t.Fatalf("got %+v", got)
	}

	a.Name = "改名了"
	a.ProxyURL = "http://127.0.0.1:1080"
	if err := s.UpdateAccount(ctx, a); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetAccount(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "改名了" || got.ProxyURL != "http://127.0.0.1:1080" {
		t.Fatalf("after update got %+v", got)
	}

	list, err := s.ListAccounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("list = %d accounts", len(list))
	}

	if err := s.DeleteAccount(ctx, "acc_01"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetAccount(ctx, "acc_01"); err == nil {
		t.Fatal("expected the account to be gone")
	}
}

// TestNotFoundIsOneSentinel says every way of saying "there is no
// such row" says it the same way.
//
// 找不到就是找不到，一种说法就够。
func TestNotFoundIsOneSentinel(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	if _, err := s.GetAccount(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get account: %v, want ErrNotFound", err)
	}
	if _, err := s.GetToken(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get token: %v, want ErrNotFound", err)
	}
	err := s.UpdateAccount(ctx, Account{ID: "nope", Name: "x", Status: StatusActive})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("update account: %v, want ErrNotFound", err)
	}
	err = s.DeleteAccount(ctx, "nope")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete account: %v, want ErrNotFound", err)
	}
	// 哨兵不能是 database/sql 那个
	if errors.Is(err, sql.ErrNoRows) {
		t.Fatal("the error is the database's own sentinel")
	}
}

// TestDeleteAccountTakesTheChildrenWithIt says an account's children
// go with the account.
//
// 删除账号要带着它的账本、签到、用量和会话一起走。
func TestDeleteAccountTakesTheChildrenWithIt(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	makeAccount(t, s, "acc_01")

	if err := s.SaveToken(ctx, Token{AccountID: "acc_01", Access: "a", Refresh: "r",
		ExpiresAt: time.Now().Add(time.Hour), UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetExhausted(ctx, "acc_01", "general", time.Now().Add(time.Hour), "quota"); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveCheckin(ctx, Checkin{AccountID: "acc_01", Day: "2026-11-13",
		Outcome: "claimed", Credit: 25, Streak: 3, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveUsageSnapshot(ctx, UsageSnapshot{AccountID: "acc_01", HasAmounts: true,
		Granted: 100, Remaining: 100, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := s.BindSession(ctx, "sess-1", "acc_01"); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteAccount(ctx, "acc_01"); err != nil {
		t.Fatal(err)
	}

	for _, table := range []string{
		"tokens", "exhausted_windows", "checkins", "usage_snapshots", "sessions",
	} {
		var n int
		if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("%s has %d rows left, want none", table, n)
		}
	}
}

// TestChildRowWithoutAnAccountIsRefused says a child row of an
// account that is not there is not accepted.
//
// 没有账号的孩子不算数。
func TestChildRowWithoutAnAccountIsRefused(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	err := s.SaveToken(ctx, Token{AccountID: "nope", Access: "a", Refresh: "r",
		ExpiresAt: time.Now().Add(time.Hour), UpdatedAt: time.Now()})
	if err == nil {
		t.Fatal("the token of an account that is not there was accepted")
	}
	err = s.SaveUsageSnapshot(ctx, UsageSnapshot{AccountID: "nope", HasAmounts: true,
		Granted: 1, Remaining: 1, CreatedAt: time.Now()})
	if err == nil {
		t.Fatal("the reading of an account that is not there was accepted")
	}
	err = s.BindSession(ctx, "sess-1", "nope")
	if err == nil {
		t.Fatal("the session of an account that is not there was accepted")
	}
	err = s.SaveCheckin(ctx, Checkin{AccountID: "nope", Day: "2026-11-13",
		Outcome: "claimed", CreatedAt: time.Now()})
	if err == nil {
		t.Fatal("the check-in of an account that is not there was accepted")
	}
	err = s.SetExhausted(ctx, "nope", "general", time.Now().Add(time.Hour), "quota")
	if err == nil {
		t.Fatal("the window of an account that is not there was accepted")
	}
}

func TestTokenRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	makeAccount(t, s, "acc_01")
	exp := time.Now().Add(time.Hour)
	if err := s.SaveToken(ctx, Token{
		AccountID: "acc_01", Access: "a", Refresh: "r",
		ExpiresAt: exp, Subject: "sub1", UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetToken(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if got.Access != "a" || got.Refresh != "r" || got.Subject != "sub1" {
		t.Fatalf("got %+v", got)
	}
	if d := got.ExpiresAt.Sub(exp); d > time.Second || d < -time.Second {
		t.Fatalf("expires_at drifted by %v", d)
	}
}

// TestANullSubjectIsReadable says a token whose subject was never
// written can still be read.
//
// 老库里的 subject 可以是空的，空的也要读得出来。
func TestANullSubjectIsReadable(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "old.db")
	makeOldFile(t, path)

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetToken(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if got.Subject != "" {
		t.Fatalf("subject = %q, want nothing", got.Subject)
	}
	if got.Access != "a" {
		t.Fatalf("got %+v", got)
	}
}

func TestCheckinUniquePerDay(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	makeAccount(t, s, "acc_01")
	c := Checkin{AccountID: "acc_01", Day: "2026-11-13", Outcome: "claimed", Credit: 25, Streak: 3}
	if err := s.SaveCheckin(ctx, c); err != nil {
		t.Fatal(err)
	}
	// 同一天再存一次是更新，不是插入第二行
	c.Outcome = "done"
	if err := s.SaveCheckin(ctx, c); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListCheckins(ctx, "acc_01", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Outcome != "done" {
		t.Fatalf("got %+v", list)
	}
}

// TestANullCreditIsReadable says a check-in whose credit was never
// written can still be read.
func TestANullCreditIsReadable(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "old.db")
	makeOldFile(t, path)

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListCheckins(ctx, "acc_01", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("got %+v", list)
	}
	if list[0].Credit != 0 || list[0].Streak != 0 {
		t.Fatalf("got %+v", list[0])
	}
}

// TestListCheckinsLimit says a limit of zero is no limit.
func TestListCheckinsLimit(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	makeAccount(t, s, "acc_01")
	for _, day := range []string{"2026-11-11", "2026-11-12", "2026-11-13"} {
		if err := s.SaveCheckin(ctx, Checkin{AccountID: "acc_01", Day: day,
			Outcome: "claimed", CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.ListCheckins(ctx, "acc_01", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("got %d check-ins, want all three", len(list))
	}
	list, err = s.ListCheckins(ctx, "acc_01", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("got %d check-ins, want two", len(list))
	}
}

func TestExhaustedWindows(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	makeAccount(t, s, "acc_01")
	until := time.Now().Add(time.Hour)
	if err := s.SetExhausted(ctx, "acc_01", "general", until, "quota"); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListExhausted(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].WindowKey != "general" || got[0].Reason != "quota" {
		t.Fatalf("got %+v", got)
	}
	if err := s.ClearExhausted(ctx, "acc_01", "general"); err != nil {
		t.Fatal(err)
	}
	got, err = s.ListExhausted(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("got %+v", got)
	}
}

// TestTheWindowsThatHaveComeBackAreStillThere says ListExhausted
// gives the windows that have run out as well.
//
// 到期的窗口还在表里，谁在用自己看着办。
func TestTheWindowsThatHaveComeBackAreStillThere(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	makeAccount(t, s, "acc_01")

	now := time.Now()
	if err := s.SetExhausted(ctx, "acc_01", "general", now.Add(-time.Hour), "quota"); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListExhausted(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %+v, want the expired window too", got)
	}
	if err := s.PruneExpiredWindows(ctx, now); err != nil {
		t.Fatal(err)
	}
	got, err = s.ListExhausted(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("got %+v, want the expired window gone", got)
	}
}

// TestPruneUsageSnapshots says the old readings are thrown away and
// the newest ones are kept.
//
// 旧读数扔掉，新的留下。
func TestPruneUsageSnapshots(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	makeAccount(t, s, "acc_01")

	old := time.Now().Add(-48 * time.Hour)
	for i := 0; i < 5; i++ {
		if err := s.SaveUsageSnapshot(ctx, UsageSnapshot{AccountID: "acc_01", HasAmounts: true,
			Granted: float64(i), Remaining: float64(i), CreatedAt: old.Add(time.Duration(i) * time.Minute)}); err != nil {
			t.Fatal(err)
		}
	}
	// 一条新的
	if err := s.SaveUsageSnapshot(ctx, UsageSnapshot{AccountID: "acc_01", HasAmounts: true,
		Granted: 9, Remaining: 9, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	if err := s.PruneUsageSnapshots(ctx, time.Now().Add(-24*time.Hour), 1); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM usage_snapshots").Scan(&n); err != nil {
		t.Fatal(err)
	}
	// 每个账号留最新的一条
	if n != 1 {
		t.Fatalf("got %d readings, want the newest one", n)
	}
	// 一条都不留的时候就没有了
	if err := s.PruneUsageSnapshots(ctx, time.Now().Add(time.Hour), 0); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM usage_snapshots").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("got %d readings, want none", n)
	}
}

func TestSessionBinding(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	makeAccount(t, s, "acc_01")
	makeAccount(t, s, "acc_02")
	if err := s.BindSession(ctx, "sess-1", "acc_01"); err != nil {
		t.Fatal(err)
	}
	id, ok, err := s.SessionAccount(ctx, "sess-1")
	if err != nil || !ok || id != "acc_01" {
		t.Fatalf("got %q %v %v", id, ok, err)
	}
	// 改绑
	if err := s.BindSession(ctx, "sess-1", "acc_02"); err != nil {
		t.Fatal(err)
	}
	id, _, _ = s.SessionAccount(ctx, "sess-1")
	if id != "acc_02" {
		t.Fatalf("got %q", id)
	}
}

// TestUpstreamIDIsRandomAndKept says every session has an upstream id
// of its own, and keeps it.
//
// 两段一样的开场白不能共用一个上游会话。
func TestUpstreamIDIsRandomAndKept(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	first, err := s.UpstreamID(ctx, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if first == "" {
		t.Fatal("the session has no upstream id")
	}
	again, err := s.UpstreamID(ctx, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if again != first {
		t.Fatalf("got %q and %q, want the same id", again, first)
	}
	other, err := s.UpstreamID(ctx, "sess-2")
	if err != nil {
		t.Fatal(err)
	}
	if other == first {
		t.Fatal("two sessions got the same upstream id")
	}
	// 同一个 key 再问还是那个 id
	third, err := s.UpstreamID(ctx, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if third != first {
		t.Fatalf("got %q, want %q", third, first)
	}
}

// TestTheUpstreamIDSurvivesTheBinding says the upstream id belongs
// to the conversation and not to the account.
func TestTheUpstreamIDSurvivesTheBinding(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	makeAccount(t, s, "acc_01")

	id, err := s.UpstreamID(ctx, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.BindSession(ctx, "sess-1", "acc_01"); err != nil {
		t.Fatal(err)
	}
	again, err := s.UpstreamID(ctx, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if again != id {
		t.Fatalf("got %q, want %q", again, id)
	}
	acc, ok, err := s.SessionAccount(ctx, "sess-1")
	if err != nil || !ok || acc != "acc_01" {
		t.Fatalf("got %q %v %v", acc, ok, err)
	}
}

func TestUsageSnapshot(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	makeAccount(t, s, "acc_01")
	if err := s.SaveUsageSnapshot(ctx, UsageSnapshot{
		AccountID: "acc_01", Granted: 600, Consumed: 120.5, Remaining: 479.5,
		HasAmounts: true,
		ExpiresAt:  time.Now().Add(time.Hour), Plan: "M Plan",
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.LatestUsageSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Remaining != 479.5 {
		t.Fatalf("got %+v", got)
	}
	if !got[0].HasAmounts {
		t.Fatal("the amounts were lost")
	}
}

func TestUsageSnapshotWithoutAmounts(t *testing.T) {
	// 读不到账本和读到 0 是两回事：读不到就没有数
	ctx := context.Background()
	s := openTest(t)
	makeAccount(t, s, "acc_02")
	if err := s.SaveUsageSnapshot(ctx, UsageSnapshot{
		AccountID: "acc_02", Plan: "M Plan", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.LatestUsageSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %+v", got)
	}
	if got[0].HasAmounts {
		t.Fatalf("a reading with no amounts says it has amounts: %+v", got[0])
	}
}

// TestTheAmountsAreKeptWithoutTheFlag says a caller that forgets to
// say that a reading has amounts does not lose the numbers.
//
// 忘了说有没有数，数还在。
func TestTheAmountsAreKeptWithoutTheFlag(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	makeAccount(t, s, "acc_01")

	if err := s.SaveUsageSnapshot(ctx, UsageSnapshot{
		AccountID: "acc_01", Granted: 100, Consumed: 10, Remaining: 90,
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.LatestUsageSnapshot(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if !got.HasAmounts {
		t.Fatalf("the numbers were lost: %+v", got)
	}
	if got.Granted != 100 || got.Consumed != 10 || got.Remaining != 90 {
		t.Fatalf("got %+v", got)
	}
}

// TestTheNewestUsageSnapshotIsTheOneReadLast says the newest reading
// of an account is the one that was read last, not the one that was
// written last.
//
// 读额度的时候时刻是在读的开头盖的，插入是在读完之后：两次读撞在
// 一起的时候，先读到的那一次反而是后写进去的（id 更大）。只按 id
// 排，卡上就摆着旧的数。
//
// 时刻说哪一次读是最新的，id 只在时刻一样时分出先后——跟签到那边
// 一个样。
func TestTheNewestUsageSnapshotIsTheOneReadLast(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	makeAccount(t, s, "acc_01")
	readFirst := time.UnixMilli(1763000000000)
	readLater := readFirst.Add(time.Minute)
	// 先写进去的那一次读，是更晚读到的那一次。
	if err := s.SaveUsageSnapshot(ctx, UsageSnapshot{
		AccountID: "acc_01", HasAmounts: true,
		Granted: 100, Consumed: 0, Remaining: 100,
		ExpiresAt: readLater.Add(time.Hour), CreatedAt: readLater,
	}); err != nil {
		t.Fatal(err)
	}
	// 后写进去的那一次读，是更早读到的那一次：id 更大，读到的更早。
	if err := s.SaveUsageSnapshot(ctx, UsageSnapshot{
		AccountID: "acc_01", HasAmounts: true,
		Granted: 300, Consumed: 0, Remaining: 300,
		ExpiresAt: readFirst.Add(time.Hour), CreatedAt: readFirst,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.LatestUsageSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows, want one per account", len(got))
	}
	if got[0].Remaining != 100 {
		t.Fatalf("got %+v, want the reading that was made last", got[0])
	}
	one, err := s.LatestUsageSnapshot(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if one.Remaining != 100 {
		t.Fatalf("got %+v, want the reading that was made last", one)
	}
}

// TestLatestUsageSnapshotIsOneRowPerAccount says two readings of one
// account in the same millisecond come back as one row.
//
// 两行都回来的时候求和会把同一个账号的积分算两遍。
func TestLatestUsageSnapshotIsOneRowPerAccount(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	makeAccount(t, s, "acc_01")
	// 同一毫秒里的两次刷新
	at := time.UnixMilli(1763000000000)
	if err := s.SaveUsageSnapshot(ctx, UsageSnapshot{
		AccountID: "acc_01", HasAmounts: true,
		Granted: 100, Consumed: 0, Remaining: 100,
		ExpiresAt: at.Add(time.Hour), CreatedAt: at,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveUsageSnapshot(ctx, UsageSnapshot{
		AccountID: "acc_01", HasAmounts: true,
		Granted: 300, Consumed: 0, Remaining: 300,
		ExpiresAt: at.Add(time.Hour), CreatedAt: at,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.LatestUsageSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows, want one per account", len(got))
	}
	if got[0].Remaining != 300 {
		t.Fatalf("got %+v, want the newest reading", got[0])
	}
}

// TestLatestUsageSnapshotGivesOneAccount says one account's newest
// reading is asked for on its own.
func TestLatestUsageSnapshotGivesOneAccount(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	now := time.Now()
	for _, id := range []string{"acc_01", "acc_02"} {
		makeAccount(t, s, id)
		if err := s.SaveUsageSnapshot(ctx, UsageSnapshot{
			AccountID: id, HasAmounts: true, Granted: 10, Remaining: 10, CreatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.LatestUsageSnapshot(ctx, "acc_02")
	if err != nil {
		t.Fatal(err)
	}
	if got.AccountID != "acc_02" {
		t.Fatalf("got %+v, want acc_02", got)
	}
	// 没有读过的账号没有读数，也没有错
	none, err := s.LatestUsageSnapshot(ctx, "acc_99")
	if err != nil {
		t.Fatal(err)
	}
	if none.AccountID != "" {
		t.Fatalf("got %+v, want nothing", none)
	}
}

// TestUsageSnapshotKeepsTheReadErrors says the errors of the parts
// that could not be read are written down with the reading.
//
// 读不到窗口和没有窗口不是一回事，读数要能说出来。
func TestUsageSnapshotKeepsTheReadErrors(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	makeAccount(t, s, "acc_01")
	if err := s.SaveUsageSnapshot(ctx, UsageSnapshot{
		AccountID: "acc_01", HasAmounts: true, Granted: 10, Remaining: 10,
		MembershipError: "the membership is not there",
		WindowsError:    "the windows are not there",
		CreatedAt:       time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.LatestUsageSnapshot(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if got.MembershipError != "the membership is not there" {
		t.Fatalf("got %+v", got)
	}
	if got.WindowsError != "the windows are not there" {
		t.Fatalf("got %+v", got)
	}
}

// TestDayKeyIsTheSitesDay says the day key is the site's day.
//
// The site counts its days in China: a check-in at half past midnight
// in that zone is the next day, whatever the machine's own zone is.
func TestDayKeyIsTheSitesDay(t *testing.T) {
	// 2026-11-13 20:00 UTC is 2026-11-14 04:00 in China
	at := time.Date(2026, 11, 13, 20, 0, 0, 0, time.UTC)
	if got := DayKey(at); got != "2026-11-14" {
		t.Fatalf("day = %q, want 2026-11-14", got)
	}
}

// TestTheTimesComeBackInUTC says a time that was written in UTC comes
// back in UTC.
//
// 写进去是 UTC，读出来也得是 UTC。
func TestTheTimesComeBackInUTC(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	at := time.Date(2026, 11, 13, 20, 0, 0, 0, time.UTC)
	makeAccount(t, s, "acc_01")
	if err := s.SaveUsageSnapshot(ctx, UsageSnapshot{
		AccountID: "acc_01", HasAmounts: true, Granted: 1, Remaining: 1, CreatedAt: at,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.LatestUsageSnapshot(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if got.CreatedAt.Location() != time.UTC {
		t.Fatalf("created_at is in %v, want UTC", got.CreatedAt.Location())
	}
	if !got.CreatedAt.Equal(at) {
		t.Fatalf("created_at = %v, want %v", got.CreatedAt, at)
	}
}

// TestTheStoreHasAClock says the store's clock is what a row is
// written at when the caller does not say.
//
// 调用方不说是几点，存储层自己说。
func TestTheStoreHasAClock(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	at := time.Date(2026, 11, 13, 20, 0, 0, 0, time.UTC)
	s.Clock = func() time.Time { return at }
	makeAccount(t, s, "acc_01")

	if err := s.SaveUsageSnapshot(ctx, UsageSnapshot{
		AccountID: "acc_01", HasAmounts: true, Granted: 1, Remaining: 1,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.LatestUsageSnapshot(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if !got.CreatedAt.Equal(at) {
		t.Fatalf("created_at = %v, want %v", got.CreatedAt, at)
	}
}

// TestAnUnknownStatusIsRefused says an account cannot have a status
// nobody wrote.
//
// 写错的状态会让账号悄悄不能用。
func TestAnUnknownStatusIsRefused(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	err := s.CreateAccount(ctx, Account{ID: "acc_01", Name: "a", Status: "actve"})
	if err == nil {
		t.Fatal("the account with the misspelled status was accepted")
	}
	makeAccount(t, s, "acc_02")
	err = s.UpdateAccount(ctx, Account{ID: "acc_02", Name: "a", Status: "actve"})
	if err == nil {
		t.Fatal("the account with the misspelled status was accepted")
	}
	err = s.UpdateAccountStatus(ctx, "acc_02", "actve", "", time.Now())
	if err == nil {
		t.Fatal("the account with the misspelled status was accepted")
	}
}

// TestTheNarrowSetters says the state and the outlet are written
// without reading the account first.
func TestTheNarrowSetters(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	makeAccount(t, s, "acc_01")

	if err := s.UpdateAccountStatus(ctx, "acc_01", StatusSignedOut, "the sign-in was refused", time.Now()); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetAccount(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusSignedOut || got.LastError != "the sign-in was refused" {
		t.Fatalf("got %+v", got)
	}
	if err := s.SetAccountProxy(ctx, "acc_01", "warp", "http://127.0.0.1:1080", time.Now()); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetAccount(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if got.ProxyGroup != "warp" || got.ProxyURL != "http://127.0.0.1:1080" {
		t.Fatalf("got %+v", got)
	}
	// 没有的账号还是找不到
	if err := s.UpdateAccountStatus(ctx, "nope", StatusActive, "", time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update the status: %v, want ErrNotFound", err)
	}
	if err := s.SetAccountProxy(ctx, "nope", "warp", "", time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("set the outlet: %v, want ErrNotFound", err)
	}
}

// TestThePragmasAreOnEveryConnection says every connection of the
// pool has the pragmas, and not only the first one.
//
// 每个连接都得带着 pragma，不能只有第一个。
func TestThePragmasAreOnEveryConnection(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	conns := make([]*sql.Conn, 0, maxConns)
	for i := 0; i < maxConns; i++ {
		c, err := s.db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
	}
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	for i, c := range conns {
		var fk int
		if err := c.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil {
			t.Fatal(err)
		}
		if fk != 1 {
			t.Fatalf("connection %d has foreign_keys = %d, want 1", i, fk)
		}
		var busy int
		if err := c.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busy); err != nil {
			t.Fatal(err)
		}
		if busy != 5000 {
			t.Fatalf("connection %d has busy_timeout = %d, want 5000", i, busy)
		}
		var mode string
		if err := c.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
			t.Fatal(err)
		}
		if mode != "wal" {
			t.Fatalf("connection %d has journal_mode = %q, want wal", i, mode)
		}
	}
}

// TestOneStoreIsUsedFromSeveralGoroutines says the store can be used
// by several goroutines at once.
//
// 几个线程一起用也不该打架。
func TestOneStoreIsUsedFromSeveralGoroutines(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("acc_%02d", i)
			if err := s.CreateAccount(ctx, Account{ID: id, Name: id, Status: StatusActive}); err != nil {
				errs <- err
				return
			}
			for j := 0; j < 5; j++ {
				if _, err := s.GetAccount(ctx, id); err != nil {
					errs <- err
					return
				}
				if err := s.SaveUsageSnapshot(ctx, UsageSnapshot{
					AccountID: id, HasAmounts: true, Granted: 1, Remaining: 1,
					CreatedAt: time.Now(),
				}); err != nil {
					errs <- err
					return
				}
				if _, err := s.LatestUsageSnapshots(ctx); err != nil {
					errs <- err
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("the store was used from several goroutines: %v", err)
	}
}

// TestThePathWithAQuestionMarkIsThePath says a path with a question
// mark in it is the path and not the end of the file name.
//
// 路径里有问号，问号不能把文件名截断。
func TestThePathWithAQuestionMarkIsThePath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a?b&c#d%e.db")

	d := dsn(path, options{})
	if !strings.HasPrefix(d, "file:") {
		t.Fatalf("dsn = %q, want a file: URI", d)
	}
	if !strings.Contains(d, "%3F") {
		t.Fatalf("dsn = %q, want the question mark escaped", d)
	}
	if !strings.Contains(d, "%26") || !strings.Contains(d, "%23") || !strings.Contains(d, "%25") {
		t.Fatalf("dsn = %q, want the other marks escaped", d)
	}
	if !strings.Contains(d, "_pragma=foreign_keys(1)") {
		t.Fatalf("dsn = %q, want the pragmas", d)
	}

	if runtime.GOOS == "windows" {
		// Windows does not allow a question mark in a file name, so
		// the file itself cannot be made there.
		t.Skip("windows does not allow a question mark in a file name")
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the database is not at the path: %v", err)
	}
	var fk int
	if err := s.db.QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil {
		t.Fatal(err)
	}
	if fk != 1 {
		t.Fatalf("foreign_keys = %d, want 1", fk)
	}
}

// TestTheFileIsOnlyForItsOwner says the database file is not for
// everybody.
//
// 数据库里有明文的 refresh token，别人不能读。
//
// 目录是存储层自己建的那一个：它建成只有属主能进的样子。已经在那儿
// 的目录不归它管 —— 把用户自己挑的目录改成 700 是它不该做的事。文件
// 不一样：不管是谁建的，存储层都把它改成 600。
func TestTheFileIsOnlyForItsOwner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("windows has no such a mode")
	}
	dir := filepath.Join(t.TempDir(), "data")
	path := filepath.Join(dir, "data.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("the file's mode is %o, want 600", mode)
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if mode := di.Mode().Perm(); mode != 0o700 {
		t.Fatalf("the directory's mode is %o, want 700", mode)
	}
	// WAL 和 SHM 里也是明文：SQLite 照着数据库文件的权限建它们。
	for _, name := range []string{path + "-wal", path + "-shm"} {
		wi, err := os.Stat(name)
		if err != nil {
			continue
		}
		if mode := wi.Mode().Perm(); mode != 0o600 {
			t.Fatalf("%s's mode is %o, want 600", filepath.Base(name), mode)
		}
	}
}

// oldSchema is the schema of a file that was made before the
// versions were kept.
const oldSchema = `
CREATE TABLE accounts (
  id               TEXT PRIMARY KEY,
  name             TEXT NOT NULL,
  user_id          TEXT,
  email            TEXT,
  status           TEXT NOT NULL,
  proxy_group      TEXT,
  proxy_url        TEXT,
  pending_persist  INTEGER NOT NULL DEFAULT 0,
  exhausted_until  INTEGER,
  last_error       TEXT,
  created_at       INTEGER NOT NULL,
  updated_at       INTEGER NOT NULL
);
CREATE TABLE tokens (
  account_id  TEXT PRIMARY KEY,
  access      TEXT NOT NULL,
  refresh     TEXT NOT NULL,
  expires_at  INTEGER NOT NULL,
  subject     TEXT,
  updated_at  INTEGER NOT NULL
);
CREATE TABLE exhausted_windows (
  account_id TEXT NOT NULL,
  window_key TEXT NOT NULL,
  until      INTEGER NOT NULL,
  reason     TEXT,
  PRIMARY KEY (account_id, window_key)
);
CREATE TABLE sessions (
  session_key TEXT PRIMARY KEY,
  account_id  TEXT NOT NULL,
  updated_at  INTEGER NOT NULL
);
CREATE TABLE checkins (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  account_id TEXT NOT NULL,
  day        TEXT NOT NULL,
  outcome    TEXT NOT NULL,
  credit     REAL,
  streak     INTEGER,
  message    TEXT,
  created_at INTEGER NOT NULL,
  UNIQUE (account_id, day)
);
CREATE TABLE usage_snapshots (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  account_id  TEXT NOT NULL,
  granted     REAL,
  consumed    REAL,
  remaining   REAL,
  expires_at  INTEGER,
  plan        TEXT,
  plan_until  INTEGER,
  windows_json TEXT,
  balance     TEXT,
  created_at  INTEGER NOT NULL
);
CREATE TABLE settings (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE egress_health (
  url        TEXT PRIMARY KEY,
  ok         INTEGER NOT NULL,
  error      TEXT,
  checked_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_usage_account ON usage_snapshots(account_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_checkins_account ON checkins(account_id, day DESC);
`

// makeOldFile writes a file in the shape the code used to make.
func makeOldFile(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(oldSchema); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO accounts (id,name,user_id,email,status,proxy_group,proxy_url,pending_persist,exhausted_until,last_error,created_at,updated_at)
		  VALUES ('acc_01','old','123','a@example.com','active','warp','',1,NULL,'',?,?)`, []any{now, now}},
		{`INSERT INTO tokens (account_id,access,refresh,expires_at,subject,updated_at) VALUES ('acc_01','a','r',?,NULL,?)`, []any{now, now}},
		{`INSERT INTO checkins (account_id,day,outcome,credit,streak,message,created_at) VALUES ('acc_01','2026-11-13','claimed',NULL,NULL,'',?)`, []any{now}},
		{`INSERT INTO usage_snapshots (account_id,granted,consumed,remaining,created_at) VALUES ('acc_01',600,100,500,?)`, []any{now}},
		{`INSERT INTO sessions (session_key,account_id,updated_at) VALUES ('sess-1','acc_01',?)`, []any{now}},
	} {
		if _, err := db.Exec(q.sql, q.args...); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}
}

// TestTheOldFileIsBroughtForward says a file that was made by an
// older version is brought to the shape the code wants, with its
// rows still in it.
//
// 老库要能打开，行还要在。
func TestTheOldFileIsBroughtForward(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "old.db")
	makeOldFile(t, path)

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion {
		t.Fatalf("the file is at version %d, want %d", version, schemaVersion)
	}

	// 行还在
	a, err := s.GetAccount(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if a.Name != "old" || a.ProxyGroup != "warp" {
		t.Fatalf("got %+v", a)
	}
	tok, err := s.GetToken(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if tok.Access != "a" || tok.Subject != "" {
		t.Fatalf("got %+v", tok)
	}
	list, err := s.ListCheckins(ctx, "acc_01", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Credit != 0 {
		t.Fatalf("got %+v", list)
	}
	snaps, err := s.LatestUsageSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 1 || snaps[0].Remaining != 500 {
		t.Fatalf("got %+v", snaps)
	}
	acc, ok, err := s.SessionAccount(ctx, "sess-1")
	if err != nil || !ok || acc != "acc_01" {
		t.Fatalf("got %q %v %v", acc, ok, err)
	}

	// 新列有了
	for _, c := range []struct{ table, column string }{
		{"usage_snapshots", "membership_error"},
		{"usage_snapshots", "windows_error"},
		{"sessions", "upstream_id"},
	} {
		cols, err := columnsOf(ctx, s.db, c.table)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := cols[c.column]; !ok {
			t.Fatalf("%s has no column %s", c.table, c.column)
		}
	}

	// 外键在
	err = s.SaveToken(ctx, Token{AccountID: "nope", Access: "a", Refresh: "r",
		ExpiresAt: time.Now().Add(time.Hour), UpdatedAt: time.Now()})
	if err == nil {
		t.Fatal("a token of an account that is not there was accepted")
	}

	// 自增的号接着走
	if err := s.SaveCheckin(ctx, Checkin{AccountID: "acc_01", Day: "2026-11-14",
		Outcome: "claimed", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	list, err = s.ListCheckins(ctx, "acc_01", 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range list {
		if c.Day == "2026-11-14" && c.ID <= 1 {
			t.Fatalf("the new check-in has the id %d, want one after the old ones", c.ID)
		}
	}
}

// TestMigrateIsIdempotent says the migration can be done twice, and
// the second time changes nothing.
//
// 迁移做两遍也没关系。
func TestMigrateIsIdempotent(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	makeAccount(t, s, "acc_01")

	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion {
		t.Fatalf("the file is at version %d, want %d", version, schemaVersion)
	}
	if _, err := s.GetAccount(ctx, "acc_01"); err != nil {
		t.Fatal(err)
	}
}

// TestTheNameIsWrittenOnItsOwn says the name setter writes the name
// and nothing else.
//
// 改名只写名字，别的列不碰。
func TestTheNameIsWrittenOnItsOwn(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	makeAccount(t, s, "acc_01")

	// 状态那一列一被写就报错，所以改名字的时候它一定没被写。
	if _, err := s.db.ExecContext(ctx, `
		CREATE TRIGGER no_status_write BEFORE UPDATE OF status ON accounts
		BEGIN SELECT RAISE(ABORT, 'the status was written'); END`); err != nil {
		t.Fatal(err)
	}

	if err := s.SetAccountName(ctx, "acc_01", "renamed", time.Now()); err != nil {
		t.Fatal(err)
	}
	a, err := s.GetAccount(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if a.Name != "renamed" {
		t.Fatalf("the name is %q", a.Name)
	}
	if a.Status != StatusActive {
		t.Fatalf("the status is %q", a.Status)
	}

	// 没有的账号是找不到，不是改成功。
	err = s.SetAccountName(ctx, "nope", "x", time.Now())
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want a not-found", err)
	}
}

// TestTheLastErrorIsWrittenOnItsOwn says the last-error setter writes
// the error and neither the status nor anything else.
//
// 记一条错误只写错误，不改状态。
func TestTheLastErrorIsWrittenOnItsOwn(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	makeAccount(t, s, "acc_01")

	if _, err := s.db.ExecContext(ctx, `
		CREATE TRIGGER no_status_write BEFORE UPDATE OF status ON accounts
		BEGIN SELECT RAISE(ABORT, 'the status was written'); END`); err != nil {
		t.Fatal(err)
	}

	if err := s.SetAccountLastError(ctx, "acc_01", "the site said no", time.Now()); err != nil {
		t.Fatal(err)
	}
	a, err := s.GetAccount(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if a.LastError != "the site said no" {
		t.Fatalf("the last error is %q", a.LastError)
	}
	if a.Status != StatusActive {
		t.Fatalf("the status is %q", a.Status)
	}

	err = s.SetAccountLastError(ctx, "nope", "x", time.Now())
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want a not-found", err)
	}
}

// TestThePlanErrorIsKept says the plan's error is written down with
// the reading, so a plan whose title could not be read is not a plan
// without a title.
//
// 套餐名读不出来，要记下来。
func TestThePlanErrorIsKept(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	makeAccount(t, s, "acc_01")

	if err := s.SaveUsageSnapshot(ctx, UsageSnapshot{
		AccountID: "acc_01",
		Granted:   100, Consumed: 10, Remaining: 90,
		HasAmounts: true,
		PlanError:  "the plan title could not be read",
		CreatedAt:  time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	snaps, err := s.LatestUsageSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 1 {
		t.Fatalf("got %d readings", len(snaps))
	}
	if snaps[0].PlanError != "the plan title could not be read" {
		t.Fatalf("the plan error is %q", snaps[0].PlanError)
	}

	// 没有错的读数，读回来也是空的，不是上一条留下的。
	if err := s.SaveUsageSnapshot(ctx, UsageSnapshot{
		AccountID: "acc_01",
		Granted:   100, Consumed: 20, Remaining: 80,
		HasAmounts: true,
		CreatedAt:  time.Now().Add(time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	snaps, err = s.LatestUsageSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 1 || snaps[0].PlanError != "" {
		t.Fatalf("got %+v", snaps)
	}
}

// TestTheFileAtVersionTwoGetsThePlanErrorColumn says a file that was
// made before the plan error column is brought forward by the third
// step.
//
// 版本二的库要补上这一列，行还在。
func TestTheFileAtVersionTwoGetsThePlanErrorColumn(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "v2.db")

	// 先做一个新库，再把那一列去掉、把版本写回二，就像上一个版本存下的那样。
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	makeAccount(t, s, "acc_01")
	if err := s.SaveUsageSnapshot(ctx, UsageSnapshot{
		AccountID: "acc_01", Granted: 600, Consumed: 100, Remaining: 500,
		HasAmounts: true, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "ALTER TABLE usage_snapshots DROP COLUMN plan_error"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "PRAGMA user_version = 2"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// 再打开就该被补上。
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if err := s2.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var version int
	if err := s2.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion {
		t.Fatalf("the file is at version %d, want %d", version, schemaVersion)
	}
	cols, err := columnsOf(ctx, s2.db, "usage_snapshots")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cols["plan_error"]; !ok {
		t.Fatal("usage_snapshots has no column plan_error")
	}
	// 行还在，读数还是那一条。
	snaps, err := s2.LatestUsageSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 1 || snaps[0].Remaining != 500 {
		t.Fatalf("got %+v", snaps)
	}
	if snaps[0].PlanError != "" {
		t.Fatalf("the plan error is %q", snaps[0].PlanError)
	}
}

// TestThePasswordGoesInAndComesBack says the encrypted password can be
// written and read back.
func TestThePasswordGoesInAndComesBack(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)
	if err := s.CreateAccount(ctx, Account{ID: "acc_1", Name: "a", Status: StatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPassword(ctx, "acc_1", "Y2lwaGVy", now); err != nil {
		t.Fatal(err)
	}
	got, err := s.Password(ctx, "acc_1")
	if err != nil {
		t.Fatal(err)
	}
	if got != "Y2lwaGVy" {
		t.Fatalf("password = %q", got)
	}
}

// TestAnAccountWithoutAPassword says an account that was not brought
// in by a batch import has no password, and that is not an error.
func TestAnAccountWithoutAPassword(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)
	if err := s.CreateAccount(ctx, Account{ID: "acc_1", Name: "a", Status: StatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Password(ctx, "acc_1")
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("password = %q, want none", got)
	}
}

// TestTheImportRunIsKept says a batch import is written down with what
// came of it.
func TestTheImportRunIsKept(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)
	if err := s.CreateImportRun(ctx, ImportRun{ID: "run_1", Source: "cli", StartedAt: now, Total: 2}); err != nil {
		t.Fatal(err)
	}
	err := s.FinishImportRun(ctx, "run_1", now, 1, 1, `[{"email":"a@example.com","ok":true}]`)
	if err != nil {
		t.Fatal(err)
	}
	runs, err := s.ListImportRuns(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("got %d runs", len(runs))
	}
	if runs[0].OK != 1 || runs[0].Failed != 1 || runs[0].Results == "" {
		t.Fatalf("run = %+v", runs[0])
	}
}

// TestAnImportRunCanBeAskedForByItsName says one batch import can be
// taken out on its own.
//
// 页面上要看得见某一次导入走到哪儿了：光有"最近的几次"不够，
// 得能按名字把这一次导入单独取出来。
func TestAnImportRunCanBeAskedForByItsName(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)
	if err := s.CreateImportRun(ctx, ImportRun{ID: "run_1", Source: "web", StartedAt: now, Total: 2}); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishImportRun(ctx, "run_1", now, 1, 1, "[]"); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetImportRun(ctx, "run_1")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "run_1" || got.Source != "web" || got.OK != 1 || got.Failed != 1 {
		t.Fatalf("run = %+v", got)
	}
}

// TestAnImportRunThatIsNotThere says a name that is not there is an
// error, and not a quiet empty run.
func TestAnImportRunThatIsNotThere(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if _, err := s.GetImportRun(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get import run: %v, want ErrNotFound", err)
	}
}

// TestTheOldFileGetsThePasswordColumn says a file that was made before
// the password column is brought forward: the column is there, the
// rows are still in it, and the accounts that had no password have
// none.
//
// 老库要有 password 那一列，行还要在，没有密码的账号还是 NULL。
func TestTheOldFileGetsThePasswordColumn(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "old.db")

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	makeAccount(t, s, "acc_01")
	if err := s.SaveToken(ctx, Token{AccountID: "acc_01", Access: "a", Refresh: "r",
		ExpiresAt: time.Now().Add(time.Hour), UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "ALTER TABLE accounts DROP COLUMN password"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "DROP TABLE import_runs"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "PRAGMA user_version = 3"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// 再打开就该被补上。
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if err := s2.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var version int
	if err := s2.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion {
		t.Fatalf("the file is at version %d, want %d", version, schemaVersion)
	}
	cols, err := columnsOf(ctx, s2.db, "accounts")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cols["password"]; !ok {
		t.Fatal("accounts has no column password")
	}
	// 行还在
	if _, err := s2.GetAccount(ctx, "acc_01"); err != nil {
		t.Fatal(err)
	}
	// 账本也还在
	tok, err := s2.GetToken(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if tok.Access != "a" {
		t.Fatalf("got %+v", tok)
	}
	// 没有密码的账号，那一格是空的，不是空字符串
	var isNull bool
	if err := s2.db.QueryRowContext(ctx,
		"SELECT password IS NULL FROM accounts WHERE id = ?", "acc_01").Scan(&isNull); err != nil {
		t.Fatal(err)
	}
	if !isNull {
		t.Fatal("the password of an account that never had one is not NULL")
	}
	got, err := s2.Password(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("password = %q, want none", got)
	}
	// 导入留痕的表也有了
	if err := s2.CreateImportRun(ctx, ImportRun{ID: "run_1", Source: "cli",
		StartedAt: time.Now(), Total: 1}); err != nil {
		t.Fatal(err)
	}
}

// TestAnAccountIsFoundByItsEmail says an account can be found by the
// email it was brought in with.
//
// 同一个邮箱只该有一行：重跑一次导入的时候，靠这个把原来那一行找
// 出来更新，而不是再来一行。
func TestAnAccountIsFoundByItsEmail(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now()
	if err := s.CreateAccount(ctx, Account{
		ID: "acc_1", Name: "a", Email: "a@example.com",
		Status: StatusActive, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.AccountByEmail(ctx, "a@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "acc_1" {
		t.Fatalf("account = %+v", got)
	}
	// 邮箱比大小写：清单上写的是哪个样子，库里存的是哪个样子，人看着
	// 是同一个就是同一个。
	got, err = s.AccountByEmail(ctx, "A@Example.com")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "acc_1" {
		t.Fatalf("account = %+v", got)
	}
	_, err = s.AccountByEmail(ctx, "b@example.com")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want not found", err)
	}
}

// TestASignInLandsOnTheRowWithTheSameUserID says a sign-in lands on the
// row that has the same site user id.
//
// 站点给的 realUserID 是账号唯一稳当的钥匙。有的账号没有邮箱，令牌里
// 也没有 sub：这样的账号再登一遍，要是只按邮箱和 sub 找，就会多出一行
// 活的——同一个站点账号两行都是活的，轮转会挑到两次，页面把它列两遍。
func TestASignInLandsOnTheRowWithTheSameUserID(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now()

	acc, err := s.SignIn(ctx, Account{
		ID: "acc_1", Name: "the first one", UserID: "578078539524956167",
		Status: StatusActive, CreatedAt: now, UpdatedAt: now,
	}, Token{
		Access: "tok", Refresh: "ref", ExpiresAt: now.Add(time.Hour),
		UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if acc.ID != "acc_1" {
		t.Fatalf("account = %q, want the one that was made", acc.ID)
	}

	// 又登了一遍：没有邮箱，令牌里也没有 sub，只有那个 user id。
	again, err := s.SignIn(ctx, Account{
		ID: "acc_2", Name: "", UserID: "578078539524956167",
		Status: StatusActive, CreatedAt: now, UpdatedAt: now,
	}, Token{
		Access: "tok2", Refresh: "ref2", ExpiresAt: now.Add(2 * time.Hour),
		UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != "acc_1" {
		t.Fatalf("account = %q, want the row that was already there", again.ID)
	}
	rows, err := s.ListAccounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("accounts = %v, want one row for one site account", rows)
	}
}

// TestTheAccountIsFoundByItsUserID says the site user id finds the
// account.
//
// 签到和积分明细要的是真的那个 user id，找账号也要按它找得到。
func TestTheAccountIsFoundByItsUserID(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now()

	if err := s.CreateAccount(ctx, Account{
		ID: "acc_1", Name: "one", UserID: "578078539524956167",
		Status: StatusActive, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.AccountByUserID(ctx, "578078539524956167")
	if err != nil {
		t.Fatalf("the account by its user id: %v", err)
	}
	if got.ID != "acc_1" {
		t.Fatalf("account = %q, want acc_1", got.ID)
	}
	// 空的 user id 不是一把钥匙：没有它的账号，谁也不是。
	if _, err := s.AccountByUserID(ctx, "  "); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want not found", err)
	}
}

// TestAKeyIsWrittenAndFound says a key that is written down can be
// found again by its hash.
//
// 网关认一把 key，靠的就是它的哈希：库里存哈希，请求带来的 key 算一次
// 哈希，两边对上了就是那一把。
func TestAKeyIsWrittenAndFound(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now()

	plain, err := apikey.New()
	if err != nil {
		t.Fatal(err)
	}
	hash := apikey.Hash(plain)
	if err := s.CreateAPIKey(ctx, APIKey{
		ID: "key_1", Name: "给我的笔记本", KeyHash: hash,
		Prefix: apikey.Display(plain), CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	got, err := s.APIKeyByHash(ctx, hash)
	if err != nil {
		t.Fatalf("the key by its hash: %v", err)
	}
	if got.ID != "key_1" {
		t.Fatalf("key = %+v, want the one that was written", got)
	}
	if got.Name != "给我的笔记本" {
		t.Fatalf("name = %q", got.Name)
	}
	if got.RevokedAt != nil {
		t.Fatalf("the key is already revoked: %v", got.RevokedAt)
	}
}

// TestTheKeyItselfIsNotInTheDatabase says the database does not keep the
// key.
//
// 数据库被人拿走的时候，里面不该有一把能开门的 key：它存的是哈希。
func TestTheKeyItselfIsNotInTheDatabase(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now()

	plain, err := apikey.New()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateAPIKey(ctx, APIKey{
		ID: "key_1", Name: "n", KeyHash: apikey.Hash(plain),
		Prefix: apikey.Display(plain), CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	keys, err := s.ListAPIKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Fatalf("keys = %v, want one", keys)
	}
	if strings.Contains(keys[0].KeyHash, plain) {
		t.Fatal("the key itself is in the database")
	}
	if keys[0].KeyHash != apikey.Hash(plain) {
		t.Fatalf("hash = %q, want the hash of the key", keys[0].KeyHash)
	}
}

// TestARevokedKeyIsNotFound says a revoked key does not open the gate.
//
// 撤销是把门关上：撤了的那一把不该再能进门。它还在库里——人得看得见
// 它曾经有过，也得说得清它什么时候没的。
func TestARevokedKeyIsNotFound(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now()

	plain, _ := apikey.New()
	hash := apikey.Hash(plain)
	if err := s.CreateAPIKey(ctx, APIKey{
		ID: "key_1", Name: "n", KeyHash: hash,
		Prefix: apikey.Display(plain), CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeAPIKey(ctx, "key_1", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.APIKeyByHash(ctx, hash); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want not found", err)
	}
	keys, err := s.ListAPIKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Fatalf("keys = %v, want the revoked one still to be there", keys)
	}
	if keys[0].RevokedAt == nil {
		t.Fatal("the key does not say when it was revoked")
	}
}

// TestTheKeysAreListedNewestFirst says the newest key comes first.
func TestTheKeysAreListedNewestFirst(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now()

	for i, id := range []string{"key_1", "key_2"} {
		if err := s.CreateAPIKey(ctx, APIKey{
			ID: id, Name: id, KeyHash: apikey.Hash(id),
			Prefix: "sk-…", CreatedAt: now.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
	}
	keys, err := s.ListAPIKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 {
		t.Fatalf("keys = %v, want two", keys)
	}
	if keys[0].ID != "key_2" {
		t.Fatalf("the first key is %q, want the newest one", keys[0].ID)
	}
}

// TestAKeyThatIsNotThereIsNotFound says a key that was never written is
// not found.
func TestAKeyThatIsNotThereIsNotFound(t *testing.T) {
	s := openTest(t)
	if _, err := s.APIKeyByHash(context.Background(), "nothing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want not found", err)
	}
}

// TestThePruningKeepsTheNewestReading says the reading that is kept is
// the one that was read last.
//
// 读额度的时候时刻是在读的开头盖的，插入是在读完之后：两次读撞在一起
// 的时候，先读到的那一次反而是后写进去的（id 更大）。按 id 排就留错了那
// 一条 —— 卡上摆着旧的数。
//
// 时刻说哪一次读是最新的，id 只在时刻一样时分出先后 —— 跟
// LatestUsageSnapshots 一个规矩。
func TestThePruningKeepsTheNewestReading(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	makeAccount(t, s, "acc_01")

	readFirst := time.Now().Add(-2 * time.Hour)
	readLater := time.Now().Add(-time.Hour)
	// 先写进去的那一次读，是更晚读到的那一次。
	if err := s.SaveUsageSnapshot(ctx, UsageSnapshot{
		AccountID: "acc_01", HasAmounts: true,
		Granted: 100, Remaining: 100, CreatedAt: readLater,
	}); err != nil {
		t.Fatal(err)
	}
	// 后写进去的那一次读，是更早读到的那一次：id 更大，读到的更早。
	if err := s.SaveUsageSnapshot(ctx, UsageSnapshot{
		AccountID: "acc_01", HasAmounts: true,
		Granted: 300, Remaining: 300, CreatedAt: readFirst,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.PruneUsageSnapshots(ctx, time.Now(), 1); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM usage_snapshots").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("got %d readings, want the newest one", n)
	}
	got, err := s.LatestUsageSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows, want one per account", len(got))
	}
	if got[0].Remaining != 100 {
		t.Fatalf("got %+v, want the reading that was made last", got[0])
	}
}

// queryPlan says what SQLite is going to do with a statement.
//
// 它是量「问法」的工具：同一条查询，问法不同，SQLite 做的事就不同 ——
// 而做的事不同，快慢差着十倍（一年 36 万行的签到表上，「一个号一次问一
// 次签到没有」是 4.05 ms，「一天签到的号一次拿全」是 1.52 ms）。
func queryPlan(t *testing.T, s *Store, q string, args ...any) string {
	t.Helper()
	rows, err := s.db.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+q, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatal(err)
		}
		b.WriteString(detail)
		b.WriteString("\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// TestTheLatestUsageSnapshotsAreAskedPerAccount says the newest reading
// of every account is asked for account by account.
//
// 这条查询在卡片那条路上（用户点一次卡片问一次「每个号最新的一条读数是多
// 少」）。从读数表里扫、每读一行问一次「这个号最新的是哪一行」，在只有
// account_id 的索引上要把这个号的读数排一遍 —— 而读数表比账号多得多（一个
// 号一刻钟一条的话，一天就是几千行）。从账号那边走是一千次查找。
//
// 断言抓的是**我们自己的**名字（索引名、表的别名），不是 SQLite 的措辞
// ——「SCAN」「SEARCH」「LIST SUBQUERY」那些词是 SQLite 的话，升级一下
// 就可能换一句，而它要防的那件事（读数表不被扫）没变。
func TestTheLatestUsageSnapshotsAreAskedPerAccount(t *testing.T) {
	s := openTest(t)
	makeAccount(t, s, "one")

	plan := queryPlan(t, s, latestUsageSnapshotsSQL)
	// 读数表只被查，不被扫：那个别名是 u（和子查询里的 s2）。
	for _, line := range strings.Split(plan, "\n") {
		if strings.Contains(line, "SCAN u") || strings.Contains(line, "SCAN s2") {
			t.Fatalf("the plan scans the readings: %s", plan)
		}
	}
	// 找最新那一条走的是那条索引（account_id, created_at）。
	if !strings.Contains(plan, "idx_usage_account_created") {
		t.Fatalf("the plan does not use the index on the readings: %s", plan)
	}
}

// TestTheDurableConnectionSyncsAndTheEverydayOneDoesNot says the writes
// that must not be lost go on a connection that fsyncs, and the everyday
// writes go on one that does not.
//
// 「丢不起」的两笔（续签后的 token、清掉的「用完了」标记）走同一个文件的另
// 一条连接，那条是 full 的：提交的时候它把 WAL fsync 掉，写完了的东西就
// 真的在盘上。常用的那条连接还是 normal —— 别的写丢了都能自己好起来。
//
// 配了 full 的时候不必再开第二条：主连接自己就在 fsync。
func TestTheDurableConnectionSyncsAndTheEverydayOneDoesNot(t *testing.T) {
	ctx := context.Background()

	s := openTestOpts(t, WithSynchronous("normal"))
	var sync int
	if err := s.durable.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&sync); err != nil {
		t.Fatal(err)
	}
	if sync != 2 {
		t.Errorf("the durable connection's synchronous = %d, want 2 (full)", sync)
	}
	if err := s.db.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&sync); err != nil {
		t.Fatal(err)
	}
	if sync != 1 {
		t.Errorf("the everyday connection's synchronous = %d, want 1 (normal)", sync)
	}

	// 配了 full 就没有第二条连接了：主连接自己就在 fsync。
	full := openTestOpts(t, WithSynchronous("full"))
	if full.durable != full.db {
		t.Error("there is a second connection although the everyday one fsyncs")
	}
}

// TestTheTokenIsWrittenOnTheDurableConnection says the token is written
// on the connection that fsyncs.
//
// 把那条连接关掉，token 就写不进去了 —— 而别的写照常写得进去。这一条守的
// 是「走哪条连接」：token 走错了连接，断电就能把它带走。
func TestTheTokenIsWrittenOnTheDurableConnection(t *testing.T) {
	ctx := context.Background()
	s := openTestOpts(t, WithSynchronous("normal"))
	makeAccount(t, s, "one")

	if err := s.durable.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveToken(ctx, Token{
		AccountID: "one", Access: "a", Refresh: "r",
		ExpiresAt: time.Now().Add(time.Hour), UpdatedAt: time.Now(),
	}); err == nil {
		t.Fatal("the token was written without the connection that fsyncs")
	}

	// 别的写还在那条连接上：它们照常写得进去。
	if err := s.SaveCheckin(ctx, Checkin{
		AccountID: "one", Day: "2026-11-19", Outcome: "claimed", Credit: 25,
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("the everyday write failed too: %v", err)
	}
}

// TestTheTokenWrittenInATransactionIsOnTheDurableConnection says the
// token written inside a transaction is written on the connection that
// fsyncs.
//
// 签入和导入都是走事务写令牌的（internal/store 的 SignIn 和
// internal/provision 的链）。那一笔也是丢不得的：库里留着的是一个已经被
// 站点用掉一次的 refresh token，下一次续签站点会拒，账号就被记成签入失效。
//
// 把那条连接关掉，事务就起不来了 —— 这一条守的是「走哪条连接」。
func TestTheTokenWrittenInATransactionIsOnTheDurableConnection(t *testing.T) {
	ctx := context.Background()
	s := openTestOpts(t, WithSynchronous("normal"))
	makeAccount(t, s, "one")

	if err := s.durable.Close(); err != nil {
		t.Fatal(err)
	}
	err := s.Tx(ctx, func(tx Tx) error {
		return tx.SaveToken(ctx, Token{
			AccountID: "one", Access: "a", Refresh: "r",
			ExpiresAt: time.Now().Add(time.Hour), UpdatedAt: time.Now(),
		})
	})
	if err == nil {
		t.Fatal("the token was written in a transaction without the connection that fsyncs")
	}
}

// TestTheClearedSpentMarkIsWrittenOnTheDurableConnection says the "used
// up" mark is taken off on the connection that fsyncs.
//
// 把那条连接关掉，标记就清不掉 —— 而别的写照常写得进去。
func TestTheClearedSpentMarkIsWrittenOnTheDurableConnection(t *testing.T) {
	ctx := context.Background()
	s := openTestOpts(t, WithSynchronous("normal"))
	makeAccount(t, s, "one")
	if err := s.SetExhausted(ctx, "one", "general", time.Now().Add(time.Hour), "quota"); err != nil {
		t.Fatal(err)
	}

	if err := s.durable.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.ClearExhausted(ctx, "one", "general"); err == nil {
		t.Fatal("the spent mark was cleared without the connection that fsyncs")
	}
}

// TestTheClearedSpentMarkIsReallyGone says clearing the "used up" mark
// really clears it, and says nothing is wrong.
//
// 删掉了就是删掉了：不报假错。以前那个 flush 失败是「删掉了但没落盘」这种
// 半个状态 —— 现在写不进去就是没删掉，写进去了就是在盘上的。
func TestTheClearedSpentMarkIsReallyGone(t *testing.T) {
	ctx := context.Background()
	s := openTestOpts(t, WithSynchronous("normal"))
	makeAccount(t, s, "one")
	if err := s.SetExhausted(ctx, "one", "general", time.Now().Add(time.Hour), "quota"); err != nil {
		t.Fatal(err)
	}
	if err := s.ClearExhausted(ctx, "one", "general"); err != nil {
		t.Fatalf("clear exhausted: %v", err)
	}
	var n int
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM exhausted_windows WHERE account_id=?", "one").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d spent marks are left", n)
	}
}

// TestTheCheckinsInABatchAreWrittenDown says a batch of check-ins is
// written down and can be read back.
//
// 一批签到写进去能读出来 —— 这一条是「写进去了」的意思。
func TestTheCheckinsInABatchAreWrittenDown(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	makeAccount(t, s, "one")
	makeAccount(t, s, "two")

	if err := s.SaveCheckins(ctx, []Checkin{
		{AccountID: "one", Day: "2026-11-19", Outcome: "claimed", Credit: 25, Streak: 3},
		{AccountID: "two", Day: "2026-11-19", Outcome: "claimed", Credit: 10, Streak: 1},
	}); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListCheckins(ctx, "one", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d check-ins for the account, want one", len(got))
	}
	if got[0].Day != "2026-11-19" || got[0].Credit != 25 {
		t.Fatalf("got %+v, want the check-in that was written", got[0])
	}
}

// TestTheSameBatchOfCheckinsWrittenTwiceIsAnOverwrite says writing the
// same batch again replaces the rows instead of making more of them.
//
// 签到那一趟会重跑：一个号一天只有一行（account_id + day 上有唯一约束），
// 写第二次是覆盖，不是多出一行来。
func TestTheSameBatchOfCheckinsWrittenTwiceIsAnOverwrite(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	makeAccount(t, s, "one")

	first := []Checkin{{AccountID: "one", Day: "2026-11-19", Outcome: "claimed", Credit: 25}}
	if err := s.SaveCheckins(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := []Checkin{{AccountID: "one", Day: "2026-11-19", Outcome: "claimed", Credit: 40}}
	if err := s.SaveCheckins(ctx, second); err != nil {
		t.Fatal(err)
	}

	var n int
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM checkins WHERE account_id=?", "one").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("got %d rows for the account and the day, want one", n)
	}
	var credit float64
	if err := s.db.QueryRowContext(ctx,
		"SELECT credit FROM checkins WHERE account_id=?", "one").Scan(&credit); err != nil {
		t.Fatal(err)
	}
	if credit != 40 {
		t.Fatalf("the credit = %v, want the one that was written last", credit)
	}
}

// TestAnEmptyBatchOfCheckinsWritesNothing says an empty batch is not an
// error, and writes nothing.
//
// 没有要签到的号：那是「今天不用签」，不是「签到坏了」。
func TestAnEmptyBatchOfCheckinsWritesNothing(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	if err := s.SaveCheckins(ctx, nil); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM checkins").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("got %d rows, want none", n)
	}
}

// TestABatchOfCheckinsWithABadRowIsNotWrittenAtAll says one bad row
// stops the whole batch.
//
// 整批是一个事务：一个写不下去，一整批都回去 —— 半批写下去比全批写下去
// 更贵，而丢掉的最多是这一批里那些号今天的积分进不了池。
func TestABatchOfCheckinsWithABadRowIsNotWrittenAtAll(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	makeAccount(t, s, "one")

	err := s.SaveCheckins(ctx, []Checkin{
		{AccountID: "one", Day: "2026-11-19", Outcome: "claimed", Credit: 25},
		// 这个号不在库里：外键不答应，这一批就都回去。
		{AccountID: "nope", Day: "2026-11-19", Outcome: "claimed", Credit: 25},
	})
	if err == nil {
		t.Fatal("the batch with the bad row was written down")
	}
	var n int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM checkins").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("got %d rows, want none: the batch is one transaction", n)
	}
}

// TestAtMillisecondSaysTheTimeAsTheStoreWritesItDown says the time is
// cut to the millisecond, the way the store writes it down.
//
// 库里存的时间是毫秒 —— 比毫秒细的那部分存下去就没有了。所以拿一个时刻跟
// 库里的比，要先把它也砍到毫秒：不砍的话，库里那个（毫秒）永远比手里那个
// （毫秒以下）旧那么一点，于是每一次比较都说「要重写」。
func TestAtMillisecondSaysTheTimeAsTheStoreWritesItDown(t *testing.T) {
	t.Run("the finer than a millisecond is cut", func(t *testing.T) {
		got := AtMillisecond(time.Date(2026, 11, 19, 10, 30, 0, 123456789, time.UTC))
		want := time.Date(2026, 11, 19, 10, 30, 0, 123000000, time.UTC)
		if !got.Equal(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})
	t.Run("the zero time is still the zero time", func(t *testing.T) {
		if got := AtMillisecond(time.Time{}); !got.IsZero() {
			t.Fatalf("got %v, want the zero time", got)
		}
	})
	t.Run("the result is in UTC", func(t *testing.T) {
		got := AtMillisecond(time.Date(2026, 11, 19, 10, 30, 0, 0, time.FixedZone("CST", 8*3600)))
		if got.Location() != time.UTC {
			t.Fatalf("got the zone %v, want UTC", got.Location())
		}
	})
}

// TestTheCheckinsInABatchAreWrittenInOneGo says a batch of check-ins is
// written in one transaction.
//
// 一趟签到有几百个号：一个号一个事务的话，每个号都要把签到表的那一页和
// 它的几个索引页重写一遍 —— 一千个号是 17 MB 的日志。合成一个事务写是
// 1 MB（量出来的：每个号 1133 B，而不是 18004 B）。
//
// 「一个事务」是看得见的：日志涨了多少。
func TestTheCheckinsInABatchAreWrittenInOneGo(t *testing.T) {
	const rows = 100
	oneByOne := logGrowth(t, rows, false)
	inABatch := logGrowth(t, rows, true)
	if inABatch >= oneByOne/2 {
		t.Fatalf("the batch wrote %d bytes, the single writes %d", inABatch, oneByOne)
	}
}

// logGrowth says how many bytes the log grew when check-ins were written.
//
// batch says whether they are written in one transaction or one by one.
func logGrowth(t *testing.T, rows int, batch bool) int64 {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < rows; i++ {
		makeAccount(t, s, fmt.Sprintf("acc_%03d", i))
	}
	before := logSize(t, path)
	cs := make([]Checkin, 0, rows)
	for i := 0; i < rows; i++ {
		cs = append(cs, Checkin{
			AccountID: fmt.Sprintf("acc_%03d", i), Day: "2026-11-19",
			Outcome: "claimed", Credit: 25, Streak: 3, CreatedAt: time.Now(),
		})
	}
	if batch {
		if err := s.SaveCheckins(ctx, cs); err != nil {
			t.Fatal(err)
		}
	} else {
		for _, c := range cs {
			if err := s.SaveCheckin(ctx, c); err != nil {
				t.Fatal(err)
			}
		}
	}
	return logSize(t, path) - before
}

// logSize says how big the write-ahead log is.
func logSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path + "-wal")
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatal(err)
	}
	return fi.Size()
}

// TestTheStoreIsOpenedWithItsTuning says the store is opened with the
// pragmas it was asked for.
//
// pragma 是每条连接自己的（DSN 里给），所以它们得在打开的时候给。
func TestTheStoreIsOpenedWithItsTuning(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir()+"/t.db", WithCacheMB(8), WithSynchronous("normal"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var cache, sync string
	if err := s.db.QueryRowContext(ctx, "PRAGMA cache_size").Scan(&cache); err != nil {
		t.Fatal(err)
	}
	// The size is in KB and SQLite rounds it to a power of two: 8 MB is
	// 8192 KB.
	kb, err := strconv.Atoi(cache)
	if err != nil {
		t.Fatalf("the cache size = %q, want a number of KB", cache)
	}
	if kb > -8000 {
		t.Errorf("the cache size = %d KB, want at least 8000 KB", kb)
	}
	if err := s.db.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&sync); err != nil {
		t.Fatal(err)
	}
	if sync != "1" {
		t.Errorf("synchronous = %s, want 1 (normal)", sync)
	}
}
