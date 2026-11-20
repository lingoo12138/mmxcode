package provision

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
	// subjects are the token subjects that were written.
	subjects []string
	password []string
	// failToken makes the token refuse to be saved.
	failToken bool
	// lockFailures is how many times the library says it is busy before
	// it lets the write through.
	lockFailures int
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
	subjects []string
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
		subjects: append([]string(nil), s.subjects...),
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
	s.subjects = before.subjects
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
	// 库忙：真库上两个写碰上了就是这样（SQLITE_BUSY）。这一趟没写进去
	// 任何东西，等一下再来一趟就行。
	//
	// 那句话是照着驱动说的写的：库说忙，是它的结果码（store.Busy 认的
	// 就是那个），不是句子里有没有「locked」两个字。
	if s.lockFailures > 0 {
		s.lockFailures--
		return errors.New("database is locked (5) (SQLITE_BUSY)")
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

func (t fakeTx) AccountBySubject(ctx context.Context, subject string) (store.Account, error) {
	return t.s.lookupSubject(subject)
}

func (t fakeTx) AccountByUserID(ctx context.Context, userID string) (store.Account, error) {
	return t.s.lookupUserID(userID)
}

// lookupUserID gives the account with that site user id.
//
// 真的库里是按 accounts.user_id 找的：站点给的那个真的那个 id。一个站
// 点账号一个，重登多少次都是它。
func (s *fakeStore) lookupUserID(userID string) (store.Account, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return store.Account{}, fmt.Errorf("get the account by user id: %w", store.ErrNotFound)
	}
	var found store.Account
	have := false
	for _, r := range s.rows {
		if strings.TrimSpace(r.UserID) != userID {
			continue
		}
		if !have || r.CreatedAt.Before(found.CreatedAt) ||
			(r.CreatedAt.Equal(found.CreatedAt) && r.ID < found.ID) {
			found, have = r, true
		}
	}
	if !have {
		return store.Account{}, fmt.Errorf("get the account by user id %s: %w", userID, store.ErrNotFound)
	}
	return found, nil
}

func (t fakeTx) CreateAccount(ctx context.Context, a store.Account) error {
	t.s.waitMake()
	return t.s.createAccount(a)
}

func (t fakeTx) SaveToken(ctx context.Context, tok store.Token) error {
	return t.s.saveToken(tok)
}

func (t fakeTx) SetAccountIdentity(ctx context.Context, id, email, userID string, updatedAt time.Time) error {
	return t.s.setAccountIdentity(id, email, userID)
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
	s.subjects = append(s.subjects, tok.AccountID+":"+tok.Subject)
	return nil
}

// setAccountIdentity writes the address and the user id.
func (s *fakeStore) setAccountIdentity(id, email, userID string) error {
	for i := range s.rows {
		if s.rows[i].ID == id {
			s.rows[i].Email = email
			s.rows[i].UserID = userID
		}
	}
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
//
// 真的库里比的时候两边都折成小写、去空格：清单上写的是哪个样子，库里
// 存的是哪个样子，人看着是同一个就是同一个。
func (s *fakeStore) lookup(email string) (store.Account, error) {
	for _, r := range s.rows {
		if strings.EqualFold(strings.TrimSpace(r.Email), strings.TrimSpace(email)) {
			return r, nil
		}
	}
	return store.Account{}, fmt.Errorf("get the account by email %s: %w", email, store.ErrNotFound)
}

// lookupSubject gives the account whose token carries that subject.
//
// 真的库里是顺着令牌那一行找的：令牌里的那个 sub 是站点账号自己的 id。
// 找到好几行的时候取最早的那一行（先建的先算），跟真的库一个样子。
func (s *fakeStore) lookupSubject(subject string) (store.Account, error) {
	var found store.Account
	have := false
	for _, t := range s.subjects {
		at := strings.LastIndex(t, ":")
		if at < 0 || t[at+1:] != subject {
			continue
		}
		id := t[:at]
		for _, r := range s.rows {
			if r.ID != id {
				continue
			}
			if !have || r.CreatedAt.Before(found.CreatedAt) ||
				(r.CreatedAt.Equal(found.CreatedAt) && r.ID < found.ID) {
				found, have = r, true
			}
		}
	}
	if !have {
		return store.Account{}, fmt.Errorf("get the account by subject %s: %w", subject, store.ErrNotFound)
	}
	return found, nil
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
	// probeErrs are the errors the probe step gives, one per call.
	probeErrs []error
	// passwordErrs are the errors the password step gives, one per call.
	passwordErrs []error
}

func (w *fakeWeb) RegisterDevice(ctx context.Context) (string, error) {
	return w.deviceID, nil
}

func (w *fakeWeb) Probe(ctx context.Context, email, deviceID, redirect string) error {
	w.probed = append(w.probed, email)
	if len(w.probeErrs) > 0 {
		err := w.probeErrs[0]
		w.probeErrs = w.probeErrs[1:]
		return err
	}
	w.email = email
	w.redirect = redirect
	return nil
}

func (w *fakeWeb) Password(ctx context.Context, email, password, deviceID, redirect string) error {
	w.signed = append(w.signed, email)
	if len(w.passwordErrs) > 0 {
		err := w.passwordErrs[0]
		w.passwordErrs = w.passwordErrs[1:]
		return err
	}
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
//
// It takes the Store interface: the fake one and the real library both fit,
// so a test can run the same chain against either.
func aChain(st Store) *Chain {
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
	err := c.webError(context.Background(), StagePassword, &webauth.SiteError{Code: 1200046})

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
	err := c.webError(context.Background(), StageApprove, &webauth.SiteError{Reason: "login_required"})

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
	err := c.webError(context.Background(), StagePassword, &webauth.SiteError{Code: 1200007})

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
	err := c.webError(context.Background(), StageProbe, errors.New("connection reset"))

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
	// 轮询时的一次连接断了：补轮询那一步就行，整条链不用重来。
	st := &fakeStore{}
	c := aChain(st)
	starts := 0
	c.Start = func(ctx context.Context) (*oauth.Device, error) {
		starts++
		return &oauth.Device{UserCode: "ABCD"}, nil
	}
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
	// 整条链没有重来：重来一遍等于重新登一次密码，站点那边看得到。
	if starts != 1 {
		t.Fatalf("the chain ran %d times, want 1", starts)
	}
}

// TestTheStepThatJittersIsTriedAgainAndOnlyThatStep says a step that
// hiccups is tried again, and only that step.
//
// 网络抖一下不该把整条链重跑一遍：重跑一遍意味着重新登一次密码，站点那
// 边看得到（密码错了会算尝试次数，可能锁号）。
func TestTheStepThatJittersIsTriedAgainAndOnlyThatStep(t *testing.T) {
	st := &fakeStore{}
	c := aChain(st)
	web := c.Web.(*fakeWeb)
	web.passwordErrs = []error{errors.New("connection reset by peer")}
	starts := 0
	c.Start = func(ctx context.Context) (*oauth.Device, error) {
		starts++
		return &oauth.Device{UserCode: "ABCD"}, nil
	}

	res, err := c.Import(context.Background(), "a@example.com", "hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Fatalf("res = %+v", res)
	}
	// 密码那一步试了两次。
	if len(web.signed) != 2 {
		t.Fatalf("signed = %v, want the password step tried twice", web.signed)
	}
	// 前面那几步没有跟着重来：整条链只跑了一遍。
	if starts != 1 {
		t.Fatalf("the chain ran %d times, want 1", starts)
	}
	if len(web.probed) != 1 {
		t.Fatalf("probed = %v, want the probe step not to run again", web.probed)
	}
	if len(web.approved) != 1 {
		t.Fatalf("approved = %v, want the approve step not to run again", web.approved)
	}
}

// TestTheStepIsNotTriedEndlessly says a step is tried at most three
// times.
//
// 试一次加两次：网络抖一下值得再来一次，密码错了不值得，三次足够把这两
// 种分开，又不至于把站点敲烦。
func TestTheStepIsNotTriedEndlessly(t *testing.T) {
	st := &fakeStore{}
	c := aChain(st)
	web := c.Web.(*fakeWeb)
	for i := 0; i < 5; i++ {
		web.passwordErrs = append(web.passwordErrs, errors.New("connection reset"))
	}
	c.Sleep = func(context.Context, time.Duration) error { return nil }

	if _, err := c.Import(context.Background(), "a@example.com", "hunter2"); err == nil {
		t.Fatal("no error")
	}
	if len(web.signed) != 3 {
		t.Fatalf("signed = %v, want the step tried 3 times", web.signed)
	}
}

// TestTheWaitsAreTheOnesTheDesignSays says the step waits a second and
// then four.
//
// 设计文档 §5.3：网络错误、5xx、超时退避重试 2 次（1s、4s）。
func TestTheWaitsAreTheOnesTheDesignSays(t *testing.T) {
	st := &fakeStore{}
	c := aChain(st)
	web := c.Web.(*fakeWeb)
	web.passwordErrs = []error{errors.New("connection reset"), errors.New("connection reset")}
	var waits []time.Duration
	c.Sleep = func(ctx context.Context, d time.Duration) error {
		waits = append(waits, d)
		return nil
	}

	if _, err := c.Import(context.Background(), "a@example.com", "hunter2"); err != nil {
		t.Fatal(err)
	}
	if len(waits) != 2 || waits[0] != time.Second || waits[1] != 4*time.Second {
		t.Fatalf("waits = %v, want 1s then 4s", waits)
	}
}

// TestASiteThatIsLimitingIsWaitedForOnce says a site that is limiting is
// waited for and tried once more.
//
// 429：等久一点（30s）再来一次，试一次就够 —— 再去问只是让它更烦。
func TestASiteThatIsLimitingIsWaitedForOnce(t *testing.T) {
	st := &fakeStore{}
	c := aChain(st)
	polls := 0
	c.Poll = func(ctx context.Context, d *oauth.Device) (*oauth.Token, error) {
		polls++
		return nil, &oauth.Error{Code: "too_many_requests", HTTPStatus: 429, What: oauth.WhatSignIn}
	}
	var waits []time.Duration
	c.Sleep = func(ctx context.Context, d time.Duration) error {
		waits = append(waits, d)
		return nil
	}

	if _, err := c.Import(context.Background(), "a@example.com", "hunter2"); err == nil {
		t.Fatal("no error")
	}
	if polls != 2 {
		t.Fatalf("polled %d times, want 2", polls)
	}
	if len(waits) != 1 || waits[0] != 30*time.Second {
		t.Fatalf("waits = %v, want one wait of 30s", waits)
	}
}

// TestTheChainIsRunAgainWhenTheDeviceCodeIsDead says the whole chain is
// run again when the device code is dead.
//
// 设备码废了（过期、被拒）的时候，换一张新码才有意义 —— 这是唯一一处整
// 条链重来的地方。别的地方都是补那一步：整条链重来是「重登」那个动作的
// 事（mmxcode relogin、调度器），不是导入链内部的兜底。
func TestTheChainIsRunAgainWhenTheDeviceCodeIsDead(t *testing.T) {
	st := &fakeStore{}
	c := aChain(st)
	starts := 0
	c.Start = func(ctx context.Context) (*oauth.Device, error) {
		starts++
		return &oauth.Device{UserCode: "ABCD"}, nil
	}
	polls := 0
	c.Poll = func(ctx context.Context, d *oauth.Device) (*oauth.Token, error) {
		polls++
		if polls == 1 {
			return nil, &oauth.Error{Code: "expired_token", What: oauth.WhatSignIn}
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
	if starts != 2 {
		t.Fatalf("the chain ran %d times, want 2", starts)
	}
}

// TestTheChainIsNotRunAgainWhenTheSiteRefuses says a refusal is not run
// again.
//
// 密码错、账号要设密码、没绑手机：再来一次还是这个答案，而且整条链重来等
// 于重新登一次密码 —— 站点那边会记尝试次数。
func TestTheChainIsNotRunAgainWhenTheSiteRefuses(t *testing.T) {
	st := &fakeStore{}
	c := aChain(st)
	web := c.Web.(*fakeWeb)
	web.passwordErrs = []error{&webauth.SiteError{Code: 1200046}}
	starts := 0
	c.Start = func(ctx context.Context) (*oauth.Device, error) {
		starts++
		return &oauth.Device{UserCode: "ABCD"}, nil
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
	if len(out) != 1 || out[0].OK {
		t.Fatalf("out = %+v", out)
	}
	if starts != 1 {
		t.Fatalf("the chain ran %d times, want 1", starts)
	}
	if len(web.signed) != 1 {
		t.Fatalf("signed = %v, want the password step tried once", web.signed)
	}
}

// TestTheWritingIsTriedAgainWhenTheLibraryIsBusy says the writing is
// tried again when the library is busy.
//
// 真跑出来过：令牌换新了，库说「database is locked」，写不进去。那是写库
// 那一步的事 —— 补写库那一步就行，不用把登录重来一遍。
func TestTheWritingIsTriedAgainWhenTheLibraryIsBusy(t *testing.T) {
	st := &fakeStore{lockFailures: 1}
	c := aChain(st)
	web := c.Web.(*fakeWeb)

	res, err := c.Import(context.Background(), "a@example.com", "hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Fatalf("res = %+v", res)
	}
	if len(st.accounts) != 1 {
		t.Fatalf("accounts = %v", st.accounts)
	}
	if len(st.tokens) != 1 {
		t.Fatalf("tokens = %v", st.tokens)
	}
	// 登录没有重来：重来一遍是重登那个动作的事。
	if len(web.signed) != 1 {
		t.Fatalf("signed = %v, want the sign-in done once", web.signed)
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
	//
	// 而且动的得是原来那一行，不是新来的一行：新来的一行号不一样，
	// 原来那条链也没有被换掉。这两样都要看得见——「还剩一行」是看不
	// 出来的（新来的一行建了又删，看着也像没动过）。
	st := &fakeStore{}
	c := aChain(st)
	// 两次导入给两个不同的号：新来的一行一眼就看得出来。
	ids := 0
	c.NewID = func() string {
		ids++
		return fmt.Sprintf("acc_%d", ids)
	}
	// 两次登进去的令牌不一样：换了令牌一眼就看得出来。
	polls := 0
	c.Poll = func(ctx context.Context, d *oauth.Device) (*oauth.Token, error) {
		polls++
		return &oauth.Token{Access: "acc", Refresh: fmt.Sprintf("ref%d", polls), Subject: "sub"}, nil
	}

	res, err := c.Import(context.Background(), "a@example.com", "hunter2")
	if err != nil {
		t.Fatal(err)
	}
	first := res.AccountID
	firstToken := st.tokens[0]

	st.failToken = true
	res, err = c.Import(context.Background(), "a@example.com", "hunter2")
	if err == nil {
		t.Fatal("no error for a token that could not be saved")
	}
	if res.OK {
		t.Fatalf("res = %+v", res)
	}
	if len(st.accounts) != 1 || st.accounts[0] != first+":a@example.com" {
		t.Fatalf("accounts = %v, want the original row %q", st.accounts, first)
	}
	if len(st.rows) != 1 || st.rows[0].ID != first {
		t.Fatalf("rows = %v, want the original row %q", st.rows, first)
	}
	// 令牌没有换：写失败的时候不能换。
	if len(st.tokens) != 1 || st.tokens[0] != firstToken {
		t.Fatalf("tokens = %v, want the token that is there", st.tokens)
	}
	// 第二次导入没有另起一行：它用的就是原来那一行。
	if ids != 1 {
		t.Fatalf("the import made %d ids, want 1", ids)
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

// TestAnAccountWithItsOwnAddressGoesOutOfTheGroupItWasImportedWith
// says the group the import carries wins over the address on the
// account.
//
// 出口是按「先看地址、再看组」找的：地址留着，账号还是从那个地方出去，
// 组写着是 hk 也没用。导入是人明确做的一件事，它带的组说了算，所以那个
// 地址清掉，而且要说出来——地址没了，人得知道。
func TestAnAccountWithItsOwnAddressGoesOutOfTheGroupItWasImportedWith(t *testing.T) {
	st := &fakeStore{}
	c := aChain(st)
	c.ProxyGroup = "hk"

	res, err := c.Import(context.Background(), "a@example.com", "hunter2")
	if err != nil {
		t.Fatal(err)
	}
	// 人给这个账号手设了一个地址。
	if err := st.SetAccountProxy(context.Background(), res.AccountID, "hk",
		"socks5h://127.0.0.1:1080", time.Now()); err != nil {
		t.Fatal(err)
	}
	if st.rows[0].ProxyURL == "" {
		t.Fatalf("the address was not set: %+v", st.rows[0])
	}

	res, err = c.Import(context.Background(), "a@example.com", "hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if st.rows[0].ProxyURL != "" {
		t.Fatalf("the address is still there: %+v", st.rows[0])
	}
	if st.rows[0].ProxyGroup != "hk" {
		t.Fatalf("group = %q, want hk", st.rows[0].ProxyGroup)
	}
	if !strings.Contains(res.Message, "地址") {
		t.Fatalf("message = %q, want it to say the address was cleared", res.Message)
	}
	// 被清掉的是哪个地址，得写出来：那是一个人自己设的地址，没了就得能
	// 照着填回去。
	if !strings.Contains(res.Message, "socks5h://127.0.0.1:1080") {
		t.Fatalf("message = %q, want it to name the address that was cleared", res.Message)
	}
}

// TestAnImportWithoutAGroupKeepsTheGroupThatIsThere says an import
// without a group does not wipe the group the account has.
//
// 组是人自己设的：一次没带组的导入不能把它抹掉——抹掉了账号的出口就变
// 了，而导入的人根本没有说要变。
func TestAnImportWithoutAGroupKeepsTheGroupThatIsThere(t *testing.T) {
	st := &fakeStore{}
	c := aChain(st)
	c.ProxyGroup = "hk"

	if _, err := c.Import(context.Background(), "a@example.com", "hunter2"); err != nil {
		t.Fatal(err)
	}
	if st.rows[0].ProxyGroup != "hk" {
		t.Fatalf("group = %q, want hk", st.rows[0].ProxyGroup)
	}
	// 第二趟没带组。
	c.ProxyGroup = ""
	if _, err := c.Import(context.Background(), "a@example.com", "hunter2"); err != nil {
		t.Fatal(err)
	}
	if st.rows[0].ProxyGroup != "hk" {
		t.Fatalf("group = %q, want hk: an import without a group must not wipe it",
			st.rows[0].ProxyGroup)
	}
	// 而且出口那一行根本没动过。
	if len(st.groups) != 0 {
		t.Fatalf("the outlet was written %d times, want none", len(st.groups))
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

// TestAnAccountIsFoundByItsSubjectEvenWhenTheAddressIsDifferent says
// the token's subject is the key, not the address.
//
// 清单上写的地址可能是别名，站点那边报的地址又可能一时问不出来（站点
// 503 了）。靠地址去重就会漏：同一个站点账号在库里会有两行都是活的，
// 两个都在轮转池里，其中一个的令牌迟早被另一个的重登作废。
//
// 令牌里带的那个 sub 是站点账号自己的 id：一个站点账号一个，重登多少
// 次都是它。
func TestAnAccountIsFoundByItsSubjectEvenWhenTheAddressIsDifferent(t *testing.T) {
	st := &fakeStore{}
	c := aChain(st)

	if _, err := c.Import(context.Background(), "a@example.com", "hunter2"); err != nil {
		t.Fatal(err)
	}
	if len(st.rows) != 1 {
		t.Fatalf("rows = %v", st.rows)
	}

	// 第二趟：站点那边问不出身份了（503），清单上的地址也换了。
	c.Identity = func(ctx context.Context, access, subject string) (account.Identity, error) {
		return account.Identity{}, errors.New("the site is busy")
	}
	res, err := c.Import(context.Background(), "other@example.com", "hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if len(st.rows) != 1 {
		t.Fatalf("rows = %v, want one row for one site account", st.rows)
	}
	if res.AccountID != "acc_1" {
		t.Fatalf("account id = %q, want the row that was already there", res.AccountID)
	}
}

// TestTheAddressIsTrimmed says the address is written without the
// spaces around it.
//
// 地址是去重的键之一：同一个地址写成两种样子（一边带空格一边不带），
// 库里就会看成两个账号。
func TestTheAddressIsTrimmed(t *testing.T) {
	st := &fakeStore{}
	c := aChain(st)
	// 站点那边报的地址两头带着空格。
	c.Identity = func(ctx context.Context, access, subject string) (account.Identity, error) {
		return account.Identity{Email: "  real@example.com  "}, nil
	}
	if _, err := c.Import(context.Background(), "alias@example.com", "hunter2"); err != nil {
		t.Fatal(err)
	}
	if st.rows[0].Email != "real@example.com" {
		t.Fatalf("email = %q, want it trimmed", st.rows[0].Email)
	}

	// 清单上的地址两头带着空格，站点那边又问不出身份。
	st2 := &fakeStore{}
	c2 := aChain(st2)
	c2.Identity = nil
	if _, err := c2.Import(context.Background(), "  alias@example.com  ", "hunter2"); err != nil {
		t.Fatal(err)
	}
	if st2.rows[0].Email != "alias@example.com" {
		t.Fatalf("email = %q, want it trimmed", st2.rows[0].Email)
	}
}

// TestTheAddressIsNotTakenOverFromAnotherAccount says an address that
// another account already has is not written onto this one.
//
// 两个账号共用一个地址不是好事：掉线以后重登，分不清该拿哪个密码去登
// ——密码是对着其中一个的，登错了就是把这个账号也登坏了。
//
// 这一行是按令牌里那个 id 找到的（那是站点账号自己的 id），站点那边报
// 出来的地址却是另一个账号的：这时候地址不能换过去。
func TestTheAddressIsNotTakenOverFromAnotherAccount(t *testing.T) {
	st := &fakeStore{
		rows: []store.Account{
			{ID: "acc_a", Name: "a", Email: "taken@example.com", Status: store.StatusActive},
			{ID: "acc_b", Name: "b", Email: "b@example.com", Status: store.StatusActive},
		},
		subjects: []string{"acc_b:sub-b"},
	}
	c := aChain(st)
	c.NewID = func() string { return "acc_new" }
	c.Poll = func(ctx context.Context, d *oauth.Device) (*oauth.Token, error) {
		return &oauth.Token{Access: "acc", Refresh: "ref", Subject: "sub-b"}, nil
	}
	c.Identity = func(ctx context.Context, access, subject string) (account.Identity, error) {
		return account.Identity{Email: "taken@example.com", RealUserID: "u9"}, nil
	}

	res, err := c.Import(context.Background(), "b@example.com", "hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if res.AccountID != "acc_b" {
		t.Fatalf("account id = %q, want acc_b", res.AccountID)
	}
	if st.rows[1].Email != "b@example.com" {
		t.Fatalf("email = %q, want the address the account already had", st.rows[1].Email)
	}
	if !strings.Contains(res.Message, "acc_a") {
		t.Fatalf("message = %q, want it to name the account that has the address", res.Message)
	}
}

// TestTheRealLibraryHasOneRowForOneSiteAccount says the real library
// keeps one row for one site account, even when the address on the list
// is not the one the site knows.
//
// 假的库和真的库不是一回事：真的库按令牌里那个 sub 顺着令牌那一行找，假
// 的那套是自己的一套规矩。这一条拿真的库走一遍：清单上的地址换了，站点
// 那边一时又问不出身份，库里还是只有一行。
func TestTheRealLibraryHasOneRowForOneSiteAccount(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	c := aChain(s)
	// 第一趟：站点那边报了地址，那一行上的地址是站点认的那个。
	if _, err := c.Import(ctx, "alias@example.com", "hunter2"); err != nil {
		t.Fatal(err)
	}
	// 第二趟：清单上的地址换了，站点那边问不出身份了。
	c.Identity = nil
	if _, err := c.Import(ctx, "other@example.com", "hunter2"); err != nil {
		t.Fatal(err)
	}

	rows, err := s.ListAccounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("accounts = %v, want one row for one site account", rows)
	}
	if rows[0].Email != "a@example.com" {
		t.Fatalf("email = %q, want the one the site says", rows[0].Email)
	}
}

// TestTheRowIsBroughtInLineWithWhatTheSiteSays says the address and
// the user id on the row are the ones the site says.
//
// 去重的时候邮箱是键之一：键停在旧的地方，下一次导入就会把这个账号
// 当成新账号，再插一行。站点那边报出来的真地址知道了，就得写上去。
func TestTheRowIsBroughtInLineWithWhatTheSiteSays(t *testing.T) {
	st := &fakeStore{}
	c := aChain(st)
	// 第一趟：身份没问，邮箱只能拿清单上的。
	c.Identity = nil
	if _, err := c.Import(context.Background(), "alias@example.com", "hunter2"); err != nil {
		t.Fatal(err)
	}
	if st.rows[0].Email != "alias@example.com" {
		t.Fatalf("email = %q, want the one from the list", st.rows[0].Email)
	}

	// 第二趟：站点那边说了它是谁。
	c.Identity = func(ctx context.Context, access, subject string) (account.Identity, error) {
		return account.Identity{Email: "real@example.com", RealUserID: "u9"}, nil
	}
	res, err := c.Import(context.Background(), "alias@example.com", "hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if len(st.rows) != 1 {
		t.Fatalf("rows = %v, want one row", st.rows)
	}
	if st.rows[0].Email != "real@example.com" {
		t.Fatalf("email = %q, want the one the site says", st.rows[0].Email)
	}
	if st.rows[0].UserID != "u9" {
		t.Fatalf("user id = %q, want the one the site says", st.rows[0].UserID)
	}
	// 换了地址要说：掉线以后重登用的是这一栏。
	if !strings.Contains(res.Message, "real@example.com") {
		t.Fatalf("message = %q, want it to say the address changed", res.Message)
	}
}

// TestTheAddressOnTheRowIsNotThrownAwayWhenTheSiteSaysNothing says a
// row that is already there keeps its address when the site does not
// say one.
//
// 站点那边一时问不出身份，手里的地址就只剩原来那一行的：那是站点自己
// 认过的，比清单上的更靠得住。
//
// 这一条要能分清两件事：地址留在原来那一行，和地址换成清单上那个。这两
// 件事在清单上的地址跟原来那一行一样的时候是一样的事——所以这里清单上
// 的地址跟那一行的不一样，换成清单上那个是看得出来的。
func TestTheAddressOnTheRowIsNotThrownAwayWhenTheSiteSaysNothing(t *testing.T) {
	st := &fakeStore{}
	c := aChain(st)
	// 第一趟：站点那边说了它是谁，那一行上的地址是站点认的那个。
	if _, err := c.Import(context.Background(), "alias@example.com", "hunter2"); err != nil {
		t.Fatal(err)
	}
	if st.rows[0].Email != "a@example.com" {
		t.Fatalf("email = %q, want the one the site says", st.rows[0].Email)
	}

	// 第二趟：站点那边问不出身份了，清单上的地址是另一个。
	c.Identity = nil
	if _, err := c.Import(context.Background(), "alias@example.com", "hunter2"); err != nil {
		t.Fatal(err)
	}
	if st.rows[0].Email != "a@example.com" {
		t.Fatalf("email = %q, want the one that was already there", st.rows[0].Email)
	}
}

// TestTwoRowsForOneSiteAccountAreSaidOut says the import says so when
// the library already has two rows for one site account.
//
// 那不是这次导入造成的，但两行都在轮转池里，页面把它列两遍，重登一次
// 会把另一个的令牌作废。人得知道这件事。
func TestTwoRowsForOneSiteAccountAreSaidOut(t *testing.T) {
	st := &fakeStore{}
	// 库里已经有同一个站点账号的两行（老数据）。
	st.rows = append(st.rows,
		store.Account{ID: "acc_1", Email: "a@example.com", UserID: "u1", Status: store.StatusActive},
		store.Account{ID: "acc_2", Email: "b@example.com", UserID: "u2", Status: store.StatusActive})
	st.subjects = append(st.subjects, "acc_1:sub", "acc_2:sub")
	c := aChain(st)

	res, err := c.Import(context.Background(), "b@example.com", "hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Message, "两行") {
		t.Fatalf("message = %q, want it to say the library has two rows", res.Message)
	}
}

// TestAFailureIsNotWorthTryingAgainWhenTheRunIsOver says a finished
// run does not get a failure that says "try again".
//
// 这一趟已经结束了：再试也是白试，等的那一下也是白等。说了「值得再试」
// 只是让结果表上的那一行写着它做不到的事。
func TestAFailureIsNotWorthTryingAgainWhenTheRunIsOver(t *testing.T) {
	c := &Chain{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := c.webError(ctx, StageProbe, errors.New("connection reset"))
	var pe *Error
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v", err)
	}
	if pe.Retry {
		t.Fatalf("err = %+v, want it not to be tried again", pe)
	}
	if pe.Message != "connection reset" {
		t.Fatalf("message = %q, want what went wrong", pe.Message)
	}
}

// TestAnAccountIsFoundByItsUserIDEvenWhenTheAddressIsDifferent says the
// site user id finds the account.
//
// 有的账号站点那边报不出邮箱：清单上写的地址是别名，令牌里也没有 sub。
// 只有站点给的那个真的 user id 认得出是哪一个站点账号——少了这一步，再
// 导一次就多一行活的，同一个站点账号两行都在轮转池里。
func TestAnAccountIsFoundByItsUserIDEvenWhenTheAddressIsDifferent(t *testing.T) {
	st := &fakeStore{}
	c := aChain(st)
	// 站点那边报不出邮箱，也没有别的线索，只有那个 user id。
	c.Identity = func(ctx context.Context, access, subject string) (account.Identity, error) {
		return account.Identity{RealUserID: "u1"}, nil
	}
	c.Poll = func(ctx context.Context, d *oauth.Device) (*oauth.Token, error) {
		return &oauth.Token{Access: "acc", Refresh: "ref"}, nil
	}
	if _, err := c.Import(context.Background(), "a@example.com", "hunter2"); err != nil {
		t.Fatal(err)
	}
	if len(st.rows) != 1 {
		t.Fatalf("rows = %v", st.rows)
	}

	// 第二趟：清单上的地址换了，站点那边报的还是同一个 user id。
	res, err := c.Import(context.Background(), "other@example.com", "hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if len(st.rows) != 1 {
		t.Fatalf("rows = %v, want one row for one site account", st.rows)
	}
	if res.AccountID != "acc_1" {
		t.Fatalf("account id = %q, want the row that was already there", res.AccountID)
	}
}
