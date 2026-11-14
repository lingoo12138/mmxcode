package provision

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"mmxcode/internal/account"
	"mmxcode/internal/oauth"
	"mmxcode/internal/store"
	"mmxcode/internal/webauth"
)

// fakeStore keeps what it is given.
//
// 它也照着真库的样子来：事务期间别人进不来（真库是一开始就把写锁拿
// 着），写错了要退回去。
type fakeStore struct {
	// mu is the write lock.
	mu       sync.Mutex
	accounts []string
	tokens   []string
	password []string
	// failToken makes the token refuse to be saved.
	failToken bool
	// rows are the accounts themselves.
	rows []store.Account
	// names are the names that were written.
	names []string
	// statuses are the states that were written.
	statuses []string
	// groups are the outlets that were written.
	groups []string
	// make is where the rows are made, when the test wants two imports
	// to meet there.
	make *barrier
}

// barrier lets a fixed number of callers meet.
//
// 真库里两个并发的导入是排队的：等不到对方就放行，这里就是这个意思。
type barrier struct {
	mu   sync.Mutex
	n    int
	want int
	ch   chan struct{}
}

func newBarrier(want int) *barrier {
	return &barrier{want: want, ch: make(chan struct{})}
}

// wait blocks until the wanted number of callers have arrived, or until
// the wait is over.
//
// 等不到也要放行：真库里的事务是排队的，不是卡死的。
func (b *barrier) wait() {
	b.mu.Lock()
	b.n++
	if b.n >= b.want {
		select {
		case <-b.ch:
		default:
			close(b.ch)
		}
		b.mu.Unlock()
		return
	}
	b.mu.Unlock()
	select {
	case <-b.ch:
	case <-time.After(200 * time.Millisecond):
	}
}

// snapshot is what the fake holds right now.
type snapshot struct {
	accounts []string
	tokens   []string
	password []string
	rows     []store.Account
	names    []string
	statuses []string
	groups   []string
}

// snapshot takes what the fake holds, so that it can be put back.
func (s *fakeStore) snapshot() snapshot {
	return snapshot{
		accounts: append([]string(nil), s.accounts...),
		tokens:   append([]string(nil), s.tokens...),
		password: append([]string(nil), s.password...),
		rows:     append([]store.Account(nil), s.rows...),
		names:    append([]string(nil), s.names...),
		statuses: append([]string(nil), s.statuses...),
		groups:   append([]string(nil), s.groups...),
	}
}

// restore puts back what the fake held before.
//
// 事务里写下去的东西，一眼吞掉就要退回去。
func (s *fakeStore) restore(before snapshot) {
	s.accounts = before.accounts
	s.tokens = before.tokens
	s.password = before.password
	s.rows = before.rows
	s.names = before.names
	s.statuses = before.statuses
	s.groups = before.groups
}

// Tx runs fn inside a transaction.
//
// 真库的事务一开始就把写锁拿着：两个并发的导入是排队进来的，不是同时
// 进来的。写错了就退回去。
func (s *fakeStore) Tx(ctx context.Context, fn func(store.Tx) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	before := s.snapshot()
	if err := fn(fakeTx{s}); err != nil {
		s.restore(before)
		return err
	}
	return nil
}

// fakeTx is one transaction of the fake store.
type fakeTx struct {
	s *fakeStore
}

func (t fakeTx) AccountByEmail(ctx context.Context, email string) (store.Account, error) {
	return t.s.lookup(email)
}

func (t fakeTx) CreateAccount(ctx context.Context, a store.Account) error {
	t.s.waitMake()
	return t.s.createAccount(a)
}

func (t fakeTx) SaveToken(ctx context.Context, tok store.Token) error {
	return t.s.saveToken(tok)
}

func (t fakeTx) SetAccountName(ctx context.Context, id, name string, updatedAt time.Time) error {
	return t.s.setAccountName(id, name)
}

func (t fakeTx) SetAccountProxy(ctx context.Context, id, group, url string, updatedAt time.Time) error {
	return t.s.setAccountProxy(id, group, url)
}

func (t fakeTx) UpdateAccountStatus(ctx context.Context, id, status, lastError string, updatedAt time.Time) error {
	return t.s.updateAccountStatus(id, status)
}

// waitMake lets two imports meet before they make a row.
//
// 两个并发的导入都要按邮箱找这一行，都找到没有，都要建一行——这就是要
// 防的那件事。让它们在门口碰头，这事就一定会发生，而不是碰运气。
//
// 等的时候不拿着锁：不然另一个导入连看都看不了，就成了排队，不是并发。
func (s *fakeStore) waitMake() {
	if s.make != nil {
		s.make.wait()
	}
}

// createAccount makes a row.
func (s *fakeStore) createAccount(a store.Account) error {
	s.accounts = append(s.accounts, a.ID+":"+a.Email)
	s.rows = append(s.rows, a)
	return nil
}

// saveToken writes a token.
func (s *fakeStore) saveToken(tok store.Token) error {
	if s.failToken {
		return errors.New("the disk is full")
	}
	s.tokens = append(s.tokens, tok.AccountID+":"+tok.Refresh)
	return nil
}

// setAccountName writes a name.
func (s *fakeStore) setAccountName(id, name string) error {
	s.names = append(s.names, id+":"+name)
	for i := range s.rows {
		if s.rows[i].ID == id {
			s.rows[i].Name = name
		}
	}
	return nil
}

// setAccountProxy writes an outlet.
func (s *fakeStore) setAccountProxy(id, group, url string) error {
	s.groups = append(s.groups, id+":"+group)
	for i := range s.rows {
		if s.rows[i].ID == id {
			s.rows[i].ProxyGroup = group
			s.rows[i].ProxyURL = url
		}
	}
	return nil
}

// updateAccountStatus writes a state.
func (s *fakeStore) updateAccountStatus(id, status string) error {
	s.statuses = append(s.statuses, id+":"+status)
	for i := range s.rows {
		if s.rows[i].ID == id {
			s.rows[i].Status = status
		}
	}
	return nil
}

// lookup gives the account with that email.
func (s *fakeStore) lookup(email string) (store.Account, error) {
	for _, r := range s.rows {
		if strings.EqualFold(r.Email, email) {
			return r, nil
		}
	}
	return store.Account{}, fmt.Errorf("get the account by email %s: %w", email, store.ErrNotFound)
}

func (s *fakeStore) CreateAccount(ctx context.Context, a store.Account) error {
	s.waitMake()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.createAccount(a)
}

// AccountByEmail gives the account with that email.
func (s *fakeStore) AccountByEmail(ctx context.Context, email string) (store.Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lookup(email)
}

// SetAccountName writes the account's name.
func (s *fakeStore) SetAccountName(ctx context.Context, id, name string, updatedAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.setAccountName(id, name)
}

// UpdateAccountStatus writes the account's state.
func (s *fakeStore) UpdateAccountStatus(ctx context.Context, id, status, lastError string, updatedAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.updateAccountStatus(id, status)
}

// SetAccountProxy writes the account's outlet.
func (s *fakeStore) SetAccountProxy(ctx context.Context, id, group, url string, updatedAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.setAccountProxy(id, group, url)
}

func (s *fakeStore) SaveToken(ctx context.Context, t store.Token) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveToken(t)
}

func (s *fakeStore) SetPassword(ctx context.Context, id, cipher string, updatedAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.password = append(s.password, id+":"+cipher)
	return nil
}

// DeleteAccount takes an account back out.
//
// A context that is already done is refused, the way a real database
// would refuse the delete: the test wants to see that the chain does
// not need the caller's context to be alive.
func (s *fakeStore) DeleteAccount(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, a := range s.accounts {
		if strings.HasPrefix(a, id+":") {
			s.accounts = append(s.accounts[:i], s.accounts[i+1:]...)
			break
		}
	}
	return nil
}

// fakeWeb stands in for the account site.
type fakeWeb struct {
	deviceID   string
	probed     []string
	signed     []string
	approved   []string
	email      string
	password   string
	redirect   string
	approveErr error
}

func (w *fakeWeb) RegisterDevice(ctx context.Context) (string, error) {
	return w.deviceID, nil
}

func (w *fakeWeb) Probe(ctx context.Context, email, deviceID, redirect string) error {
	w.probed = append(w.probed, email)
	w.email = email
	w.redirect = redirect
	return nil
}

func (w *fakeWeb) Password(ctx context.Context, email, password, deviceID, redirect string) error {
	w.signed = append(w.signed, email)
	w.email = email
	w.password = password
	w.redirect = redirect
	return nil
}

func (w *fakeWeb) Approve(ctx context.Context, userCode string) error {
	w.approved = append(w.approved, userCode)
	return w.approveErr
}

// aChain is a chain that does everything but talk to the site.
func aChain(st *fakeStore) *Chain {
	return &Chain{
		Store: st,
		Start: func(ctx context.Context) (*oauth.Device, error) {
			return &oauth.Device{UserCode: "ABCD"}, nil
		},
		Poll: func(ctx context.Context, d *oauth.Device) (*oauth.Token, error) {
			return &oauth.Token{Access: "acc", Refresh: "ref", Subject: "sub"}, nil
		},
		Identity: func(ctx context.Context, access, subject string) (account.Identity, error) {
			return account.Identity{Email: "a@example.com", RealUserID: "u1"}, nil
		},
		Web:   &fakeWeb{deviceID: "574786429807779842"},
		Now:   func() time.Time { return time.Unix(1763000000, 0) },
		NewID: func() string { return "acc_1" },
	}
}

func TestTheChainSignsInAndStores(t *testing.T) {
	st := &fakeStore{}
	c := aChain(st)
	c.Keeper = func(password string) (string, error) { return "cipher", nil }

	res, err := c.Import(context.Background(), "a@example.com", "hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.AccountID != "acc_1" {
		t.Fatalf("res = %+v", res)
	}
	if len(st.accounts) != 1 || st.accounts[0] != "acc_1:a@example.com" {
		t.Fatalf("accounts = %v", st.accounts)
	}
	if len(st.tokens) != 1 || st.tokens[0] != "acc_1:ref" {
		t.Fatalf("tokens = %v", st.tokens)
	}
	if len(st.password) != 1 || st.password[0] != "acc_1:cipher" {
		t.Fatalf("passwords = %v", st.password)
	}
}

func TestTheChainRunsTheWebStepsInOrder(t *testing.T) {
	st := &fakeStore{}
	c := aChain(st)
	web := c.Web.(*fakeWeb)

	if _, err := c.Import(context.Background(), "a@example.com", "hunter2"); err != nil {
		t.Fatal(err)
	}
	if len(web.probed) != 1 || web.probed[0] != "a@example.com" {
		t.Fatalf("probed = %v", web.probed)
	}
	if len(web.signed) != 1 || web.signed[0] != "a@example.com" {
		t.Fatalf("signed = %v", web.signed)
	}
	if web.password != "hunter2" {
		t.Fatalf("password = %q", web.password)
	}
	if len(web.approved) != 1 || web.approved[0] != "ABCD" {
		t.Fatalf("approved = %v", web.approved)
	}
	if !strings.Contains(web.redirect, "user_code=ABCD") {
		t.Fatalf("redirect = %q", web.redirect)
	}
}

func TestTheChainDoesNotKeepThePasswordWithoutAKey(t *testing.T) {
	st := &fakeStore{}
	c := aChain(st)

	if _, err := c.Import(context.Background(), "a@example.com", "hunter2"); err != nil {
		t.Fatal(err)
	}
	if len(st.password) != 0 {
		t.Fatalf("the password was kept: %v", st.password)
	}
}

func TestAWrongPasswordSaysSoAndIsNotTriedAgain(t *testing.T) {
	c := &Chain{}
	err := c.webError(StagePassword, &webauth.SiteError{Code: 1200046})

	var pe *Error
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v", err)
	}
	if pe.Code != 1200046 || pe.Retry {
		t.Fatalf("err = %+v", pe)
	}
	if !strings.Contains(pe.Message, "密码") {
		t.Fatalf("message = %q", pe.Message)
	}
}

func TestARefusalWithoutACodeIsStillExplained(t *testing.T) {
	c := &Chain{}
	err := c.webError(StageApprove, &webauth.SiteError{Reason: "login_required"})

	var pe *Error
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v", err)
	}
	if pe.Message == "" || !strings.Contains(pe.Message, "登录") {
		t.Fatalf("message = %q", pe.Message)
	}
}

func TestAMissingDefaultGroupIsNotTriedAgain(t *testing.T) {
	c := &Chain{
		Start: func(ctx context.Context) (*oauth.Device, error) {
			return &oauth.Device{UserCode: "ABCD"}, nil
		},
		Web: &fakeWeb{deviceID: "d1", approveErr: webauth.ErrNoDefaultGroup},
	}
	_, err := c.SignIn(context.Background(), "a@example.com", "hunter2")
	if err == nil {
		t.Fatal("no error")
	}
	var pe *Error
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v", err)
	}
	if pe.Stage != StageApprove || pe.Retry {
		t.Fatalf("err = %+v", pe)
	}
}

func TestAStaleLoginIsTriedAgain(t *testing.T) {
	c := &Chain{}
	err := c.webError(StagePassword, &webauth.SiteError{Code: 1200007})

	var pe *Error
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v", err)
	}
	if !pe.Retry {
		t.Fatalf("err = %+v", pe)
	}
}

func TestALostConnectionIsTriedAgain(t *testing.T) {
	c := &Chain{}
	err := c.webError(StageProbe, errors.New("connection reset"))

	var pe *Error
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v", err)
	}
	if !pe.Retry {
		t.Fatalf("err = %+v", pe)
	}
	// 等多久是标准退避的事（1s、4s），不是这一处说了算。
	if pe.Wait != 0 {
		t.Fatalf("wait = %v, want the standard backoff", pe.Wait)
	}
}

func TestALostConnectionWhilePollingIsTriedAgain(t *testing.T) {
	// 轮询的时候连接断了，跟前面几步的连接断了是一回事：再来一次。
	c := &Chain{}
	err := c.pollError(context.Background(), errors.New("device token: connection reset"))

	var pe *Error
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v", err)
	}
	if !pe.Retry {
		t.Fatalf("err = %+v", pe)
	}
}

func TestARefusedDeviceCodeIsNotTriedAgain(t *testing.T) {
	// 设备码过期了，或者站点不让这个码进来：再来一次也是这个答案。
	for _, code := range []string{"expired_token", "access_denied"} {
		c := &Chain{}
		err := c.pollError(context.Background(), &oauth.Error{Code: code, What: oauth.WhatSignIn})

		var pe *Error
		if !errors.As(err, &pe) {
			t.Fatalf("err = %v", err)
		}
		if pe.Retry {
			t.Fatalf("%s: err = %+v, want it not to be tried again", code, pe)
		}
	}
}

func TestASiteThatIsLimitingIsWaitedFor(t *testing.T) {
	// 429：等久一点再来一次，试一次就够。
	c := &Chain{}
	err := c.pollError(context.Background(), &oauth.Error{Code: "too_many_requests", HTTPStatus: 429, What: oauth.WhatSignIn})

	var pe *Error
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v", err)
	}
	if !pe.Retry || pe.Wait != 30*time.Second || pe.Tries != 2 {
		t.Fatalf("err = %+v", pe)
	}
}

func TestACancelledPollIsNotTriedAgain(t *testing.T) {
	// 上下文没了：再试也是白试。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := &Chain{}
	err := c.pollError(ctx, context.Canceled)

	var pe *Error
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v", err)
	}
	if pe.Retry {
		t.Fatalf("err = %+v", pe)
	}
}

func TestAClientSideTimeoutWhilePollingIsTriedAgain(t *testing.T) {
	// 客户端自己的超时（http.Client.Timeout）报出来的错也是
	// context.DeadlineExceeded，但那是连接那边的事：调用方的上下文还
	// 好好地活着，这种要再试一次，跟前面几步的连接断了是一回事。
	//
	// 真的一次超时长什么样：拿真的客户端去问一个不吭声的服务。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
	}))
	defer srv.Close()
	hc := &http.Client{Timeout: 30 * time.Millisecond}
	_, err := hc.Get(srv.URL)
	if err == nil {
		t.Fatal("the request did not time out")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want it to be a deadline", err)
	}

	c := &Chain{}
	pe := pollErrorOf(t, c.pollError(context.Background(), err))
	if !pe.Retry {
		t.Fatalf("err = %+v, want it to be tried again", pe)
	}
}

func TestAPollTimeoutIsNotTriedAgainWhenTheRunIsOver(t *testing.T) {
	// 上下文没了：再试也是白试。这时候超时就是超时，不再试。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	c := &Chain{}
	pe := pollErrorOf(t, c.pollError(ctx, context.DeadlineExceeded))
	if pe.Retry {
		t.Fatalf("err = %+v, want it not to be tried again", pe)
	}
}

// pollErrorOf says what the error the chain made was.
func pollErrorOf(t *testing.T, err error) *Error {
	t.Helper()
	var pe *Error
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v", err)
	}
	return pe
}

func TestTheChainIsTriedAgainWhenThePollLosesTheConnection(t *testing.T) {
	// 轮询时的一次连接断了，整个链要重来一次：这个账号最后是进来了的。
	st := &fakeStore{}
	c := aChain(st)
	polls := 0
	c.Poll = func(ctx context.Context, d *oauth.Device) (*oauth.Token, error) {
		polls++
		if polls == 1 {
			return nil, errors.New("device token: connection reset")
		}
		return &oauth.Token{Access: "acc", Refresh: "ref", Subject: "sub"}, nil
	}
	r := &Runner{
		Importer: &Importer{
			Import: func(ctx context.Context, email, password string) (Result, error) {
				return c.Import(ctx, email, password)
			},
		},
		Sleep: func(context.Context, time.Duration) error { return nil },
	}
	out, err := r.Run(context.Background(), []Entry{{Email: "a@example.com", Password: "pw"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || !out[0].OK {
		t.Fatalf("out = %+v", out)
	}
	if polls != 2 {
		t.Fatalf("polled %d times, want 2", polls)
	}
}

func TestAnAccountWithoutATokenIsTakenBackOut(t *testing.T) {
	st := &fakeStore{failToken: true}
	c := aChain(st)

	_, err := c.Import(context.Background(), "a@example.com", "hunter2")
	if err == nil {
		t.Fatal("no error")
	}
	var pe *Error
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v", err)
	}
	if pe.Stage != StageStore {
		t.Fatalf("err = %+v", pe)
	}
	if len(st.accounts) != 0 {
		t.Fatalf("the broken account was left behind: %v", st.accounts)
	}
}

func TestAFailedIdentityLookupKeepsTheEmailOfTheList(t *testing.T) {
	// 站点那边问不出身份的时候，邮箱得从清单里兜底：邮箱空着的账号
	// 以后落线了是登不回来的——有密码也不知道该登谁。
	st := &fakeStore{}
	c := aChain(st)
	c.Identity = func(ctx context.Context, access, subject string) (account.Identity, error) {
		return account.Identity{}, errors.New("the account API answered 503")
	}

	res, err := c.Import(context.Background(), "a@example.com", "hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Fatalf("res = %+v", res)
	}
	if len(st.accounts) != 1 || st.accounts[0] != "acc_1:a@example.com" {
		t.Fatalf("accounts = %v", st.accounts)
	}
	if !strings.Contains(res.Message, "身份") {
		t.Fatalf("message = %q, want it to say the identity was not confirmed", res.Message)
	}
	if strings.Contains(res.Message, "hunter2") {
		t.Fatalf("message = %q, want no password in it", res.Message)
	}
}

func TestTheSameAccountTwiceIsOneRow(t *testing.T) {
	// 重跑一次导入是常事：同一个站点账号只能有一行活着。
	st := &fakeStore{}
	c := aChain(st)

	first, err := c.Import(context.Background(), "a@example.com", "hunter2")
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.Import(context.Background(), "a@example.com", "hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if len(st.accounts) != 1 {
		t.Fatalf("accounts = %v, want one row", st.accounts)
	}
	if second.AccountID != first.AccountID {
		t.Fatalf("account id = %q, want %q", second.AccountID, first.AccountID)
	}
	if !second.OK {
		t.Fatalf("res = %+v", second)
	}
	if !strings.Contains(second.Message, "已经在库里") {
		t.Fatalf("message = %q, want it to say it was an update", second.Message)
	}
	// 令牌是换新的：这一行现在用的是刚登进去的那条链。
	if len(st.tokens) != 2 {
		t.Fatalf("tokens = %v, want the token written twice", st.tokens)
	}
	if len(st.statuses) != 1 || st.statuses[0] != "acc_1:"+store.StatusActive {
		t.Fatalf("statuses = %v", st.statuses)
	}
}

func TestAFailedUpdateKeepsTheRowThatIsThere(t *testing.T) {
	// 令牌没写下去的时候，库里原来那一行不能动：它还带着一条能用的
	// 链，把它删掉就是毁掉一个还能用的账号。
	st := &fakeStore{}
	c := aChain(st)
	if _, err := c.Import(context.Background(), "a@example.com", "hunter2"); err != nil {
		t.Fatal(err)
	}
	st.failToken = true
	res, err := c.Import(context.Background(), "a@example.com", "hunter2")
	if err == nil {
		t.Fatal("no error for a token that could not be saved")
	}
	if res.OK {
		t.Fatalf("res = %+v", res)
	}
	if len(st.accounts) != 1 {
		t.Fatalf("accounts = %v, want the row still there", st.accounts)
	}
}

// TestTwoConcurrentImportsOfOneEmailAreOneRow says two imports of one
// email do not make two rows.
//
// 两个并发的导入（网页上传两趟，或者两个进程各跑一趟）：都要按邮箱找
// 这一行，都找到没有，都建一行——同一个站点账号两行都是活的，页面把
// 它列两遍，其中一个的令牌迟早被另一个的重登作废。
//
// 找和建要在一个事务里：事务一开始就把写锁拿着，第二个进来的那个看见
// 的是第一个建好的那一行。
func TestTwoConcurrentImportsOfOneEmailAreOneRow(t *testing.T) {
	st := &fakeStore{make: newBarrier(2)}
	c := aChain(st)
	// 两次导入给两个不同的号：两行就看得出来是两行。
	var mu sync.Mutex
	n := 0
	c.NewID = func() string {
		mu.Lock()
		defer mu.Unlock()
		n++
		return fmt.Sprintf("acc_%d", n)
	}

	var wg sync.WaitGroup
	results := make([]Result, 2)
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = c.Import(context.Background(), "a@example.com", "hunter2")
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("import %d: %v", i, err)
		}
	}
	if len(st.accounts) != 1 {
		t.Fatalf("accounts = %v, want one row", st.accounts)
	}
	if results[0].AccountID != results[1].AccountID {
		t.Fatalf("account ids = %q and %q, want the same row",
			results[0].AccountID, results[1].AccountID)
	}
}

func TestTheAccountGoesOutOfTheGroupItWasImportedWith(t *testing.T) {
	// 账号从哪个口登进去的，以后它的活就从哪个口出：站点看到的应该是
	// 同一个账号从同一个地方来，而不是两个地方。
	st := &fakeStore{}
	c := aChain(st)
	c.ProxyGroup = "hk"

	if _, err := c.Import(context.Background(), "a@example.com", "hunter2"); err != nil {
		t.Fatal(err)
	}
	if len(st.rows) != 1 {
		t.Fatalf("rows = %v", st.rows)
	}
	if st.rows[0].ProxyGroup != "hk" {
		t.Fatalf("group = %q, want hk", st.rows[0].ProxyGroup)
	}
}

func TestAnAccountWithoutAGroupGoesOutOfTheDefault(t *testing.T) {
	st := &fakeStore{}
	c := aChain(st)

	if _, err := c.Import(context.Background(), "a@example.com", "hunter2"); err != nil {
		t.Fatal(err)
	}
	if st.rows[0].ProxyGroup != "" {
		t.Fatalf("group = %q, want the default one", st.rows[0].ProxyGroup)
	}
}

func TestTheBrokenAccountIsTakenBackOutWhenTheRunWasCutShort(t *testing.T) {
	// 导入被掐断的时候，令牌写不进去：那个半成的行要拿走。删它用的
	// 上下文不能是调用方那个——那时候它已经没了。
	st := &fakeStore{failToken: true}
	c := aChain(st)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := c.Import(ctx, "a@example.com", "hunter2"); err == nil {
		t.Fatal("no error")
	}
	if len(st.accounts) != 0 {
		t.Fatalf("the broken account was left behind: %v", st.accounts)
	}
}
