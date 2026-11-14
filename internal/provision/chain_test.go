package provision

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"mmxcode/internal/account"
	"mmxcode/internal/oauth"
	"mmxcode/internal/store"
	"mmxcode/internal/webauth"
)

// fakeStore keeps what it is given.
type fakeStore struct {
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
}

func (s *fakeStore) CreateAccount(ctx context.Context, a store.Account) error {
	s.accounts = append(s.accounts, a.ID+":"+a.Email)
	s.rows = append(s.rows, a)
	return nil
}

// AccountByEmail gives the account with that email.
func (s *fakeStore) AccountByEmail(ctx context.Context, email string) (store.Account, error) {
	for _, r := range s.rows {
		if strings.EqualFold(r.Email, email) {
			return r, nil
		}
	}
	return store.Account{}, fmt.Errorf("get the account by email %s: %w", email, store.ErrNotFound)
}

// SetAccountName writes the account's name.
func (s *fakeStore) SetAccountName(ctx context.Context, id, name string, updatedAt time.Time) error {
	s.names = append(s.names, id+":"+name)
	for i := range s.rows {
		if s.rows[i].ID == id {
			s.rows[i].Name = name
		}
	}
	return nil
}

// UpdateAccountStatus writes the account's state.
func (s *fakeStore) UpdateAccountStatus(ctx context.Context, id, status, lastError string, updatedAt time.Time) error {
	s.statuses = append(s.statuses, id+":"+status)
	for i := range s.rows {
		if s.rows[i].ID == id {
			s.rows[i].Status = status
		}
	}
	return nil
}

func (s *fakeStore) SaveToken(ctx context.Context, t store.Token) error {
	if s.failToken {
		return errors.New("the disk is full")
	}
	s.tokens = append(s.tokens, t.AccountID+":"+t.Refresh)
	return nil
}

func (s *fakeStore) SetPassword(ctx context.Context, id, cipher string, updatedAt time.Time) error {
	s.password = append(s.password, id+":"+cipher)
	return nil
}

// DeleteAccount takes an account back out.
func (s *fakeStore) DeleteAccount(ctx context.Context, id string) error {
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
	if !pe.Retry || pe.Wait <= 0 {
		t.Fatalf("err = %+v", pe)
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
