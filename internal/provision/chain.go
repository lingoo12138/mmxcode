package provision

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"mmxcode/internal/account"
	"mmxcode/internal/oauth"
	"mmxcode/internal/site"
	"mmxcode/internal/store"
	"mmxcode/internal/webauth"
)

// Store is where an imported account goes.
type Store interface {
	CreateAccount(ctx context.Context, a store.Account) error
	SaveToken(ctx context.Context, t store.Token) error
	SetPassword(ctx context.Context, id, cipher string, updatedAt time.Time) error
}

// Web is the part of the account site the chain talks to.
//
// It is an interface so that a test can bring an account in without a
// network: the real one is a webauth.Client, and the session it keeps
// belongs to one account.
type Web interface {
	RegisterDevice(ctx context.Context) (string, error)
	Probe(ctx context.Context, email, deviceID, redirect string) error
	Password(ctx context.Context, email, password, deviceID, redirect string) error
	Approve(ctx context.Context, userCode string) error
}

// The real ones fit the interfaces, so that a caller can put them in
// without an adapter.
var (
	_ Web   = (*webauth.Client)(nil)
	_ Store = (*store.Store)(nil)
)

// Chain is the importer that does the real work.
//
// It signs in to the account site with the email and the password,
// uses that session to approve a device code, and puts the account in
// the database.
type Chain struct {
	// Store is where the account goes.
	Store Store
	// Web signs in to the account site. It is one per account: the
	// site keeps the session in a cookie, and two accounts sharing one
	// session would be signing in as each other.
	Web Web
	// Start begins a device authorization.
	Start func(ctx context.Context) (*oauth.Device, error)
	// Poll waits for the sign-in to finish.
	Poll func(ctx context.Context, d *oauth.Device) (*oauth.Token, error)
	// Identity reads who the account is. A nil one means the account's
	// email is the one from the list and its user id is the token's
	// subject.
	Identity func(ctx context.Context, access, subject string) (account.Identity, error)
	// Keeper encrypts the password so that it can be kept. A nil one
	// means the password is not kept: it is used once and thrown away.
	Keeper func(password string) (string, error)
	// Now is the clock.
	Now func() time.Time
	// NewID makes an account id.
	NewID func() string
}

// SignIn signs one account in and gives back the token.
//
// The steps are the ones the design document lists: a device code, a
// sign-in, an approval, and then the token.
//
// An account that is already in the database only needs the token; an
// account that is new also needs a row of its own. Import does both,
// SignIn only the first.
func (c *Chain) SignIn(ctx context.Context, email, password string) (*oauth.Token, error) {
	if c.Web == nil {
		return nil, &Error{Stage: StageProbe, Message: "没有站点可说话"}
	}
	if c.Start == nil {
		return nil, &Error{Stage: StageToken, Message: "没有设备码可等"}
	}
	d, err := c.Start(ctx)
	if err != nil {
		return nil, &Error{Stage: StageToken, Message: err.Error(), Retry: true, Wait: 5 * time.Second}
	}
	deviceID, err := c.Web.RegisterDevice(ctx)
	if err != nil {
		return nil, &Error{Stage: StageProbe, Message: err.Error(), Retry: true, Wait: 5 * time.Second}
	}
	// The page the sign-in sends the browser back to is the page that
	// shows the code. The site builds this path itself when it says a
	// login is needed.
	redirect := "/oauth-authorize?client_id=" + url.QueryEscape(site.ClientID) +
		"&user_code=" + url.QueryEscape(d.UserCode)

	if err := c.Web.Probe(ctx, email, deviceID, redirect); err != nil {
		return nil, c.webError(StageProbe, err)
	}
	if err := c.Web.Password(ctx, email, password, deviceID, redirect); err != nil {
		return nil, c.webError(StagePassword, err)
	}
	if err := c.Web.Approve(ctx, d.UserCode); err != nil {
		if errors.Is(err, webauth.ErrNoDefaultGroup) {
			// 站点没有默认组可批准，重试多少次都一样。
			return nil, &Error{Stage: StageApprove, Message: err.Error()}
		}
		return nil, c.webError(StageApprove, err)
	}
	if c.Poll == nil {
		return nil, &Error{Stage: StageToken, Message: "没有地方等这个令牌"}
	}
	tok, err := c.Poll(ctx, d)
	if err != nil {
		return nil, &Error{Stage: StageToken, Message: err.Error()}
	}
	return tok, nil
}

// Import signs one account in and puts it in the database.
func (c *Chain) Import(ctx context.Context, email, password string) (Result, error) {
	tok, err := c.SignIn(ctx, email, password)
	if err != nil {
		return Result{Email: email}, err
	}
	if c.Store == nil {
		return Result{Email: email}, &Error{Stage: StageStore, Message: "没有数据库可放"}
	}

	id := account.Identity{}
	var notes []string
	if c.Identity != nil {
		got, err := c.Identity(ctx, tok.Access, tok.Subject)
		if err == nil {
			id = got
		} else {
			// 身份没问出来，账号还是在的，只是不知道站点那边是不是就认
			// 这一份。这件事得说出来：不然人以为每一条都对，而邮箱要是
			// 也空了，这个号以后就再也登不回来了。
			notes = append(notes, "没能确认账号身份："+err.Error())
		}
	}
	now := c.now()
	// 邮箱从清单里兜底：站点那边一时问不出来身份，账号的邮箱也不能
	// 就这么空着。没有邮箱的账号落线以后是登不回来的——手里有密码
	// 也用不上，因为不知道该拿它去登谁。
	mail := id.Email
	if mail == "" {
		mail = email
	}
	name := mail
	userID := id.RealUserID
	if userID == "" {
		userID = tok.Subject
	}
	acc := store.Account{
		ID:        c.newID(),
		Name:      name,
		UserID:    userID,
		Email:     mail,
		Status:    store.StatusActive,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := c.Store.CreateAccount(ctx, acc); err != nil {
		return Result{Email: email}, &Error{Stage: StageStore, Message: err.Error()}
	}
	if err := c.Store.SaveToken(ctx, store.Token{
		AccountID: acc.ID,
		Access:    tok.Access,
		Refresh:   tok.Refresh,
		ExpiresAt: tok.Expires,
		Subject:   tok.Subject,
		UpdatedAt: now,
	}); err != nil {
		// 账号的行已经写进去了，令牌却没有：这样的账号是残的，
		// 把它拿掉，导入才能干净地再来一次。
		c.undo(ctx, acc.ID)
		return Result{Email: email}, &Error{Stage: StageStore, Message: err.Error()}
	}
	if c.Keeper != nil {
		cipher, err := c.Keeper(password)
		if err != nil {
			// The account is in; only the password is lost. Say so,
			// but do not call the import a failure.
			notes = append(notes, "密码没能加密存下："+err.Error())
			return Result{Email: email, OK: true, AccountID: acc.ID,
				Stage: StageStore, Message: strings.Join(notes, "；")}, nil
		}
		if err := c.Store.SetPassword(ctx, acc.ID, cipher, now); err != nil {
			notes = append(notes, "密码没能存下："+err.Error())
			return Result{Email: email, OK: true, AccountID: acc.ID,
				Stage: StageStore, Message: strings.Join(notes, "；")}, nil
		}
	}
	return Result{Email: email, OK: true, AccountID: acc.ID, Message: strings.Join(notes, "；")}, nil
}

// deleter is a store that can take an account back out.
//
// It is not part of the Store interface: taking the row back out is
// only possible with a store that has DeleteAccount, and the chain
// works without it.
type deleter interface {
	DeleteAccount(ctx context.Context, id string) error
}

// undo takes the account row back out.
//
// A store that cannot take the row out is left alone: the row stays
// where it is, broken but visible, and the message says what happened
// to it.
func (c *Chain) undo(ctx context.Context, id string) {
	d, ok := c.Store.(deleter)
	if !ok {
		return
	}
	_ = d.DeleteAccount(ctx, id)
}

// webError turns what the site said into what the runner needs to
// know: the stage, the site's own code, and whether trying again could
// work.
//
// A wrong password is not worth trying again. A dropped connection is.
func (c *Chain) webError(stage Stage, err error) error {
	var se *webauth.SiteError
	if errors.As(err, &se) {
		pe := &Error{
			Stage: stage,
			Code:  se.Code,
			// ExplainText 是一句话的说法：有错误码就用错误码，没有
			// 就用站点自己那个词（login_required 那种）。
			Message: se.ExplainText(),
		}
		if retryableCode(se.Code) {
			pe.Retry = true
			pe.Wait = 5 * time.Second
		}
		return pe
	}
	return &Error{Stage: stage, Message: err.Error(), Retry: true, Wait: 5 * time.Second}
}

// retryableCode says whether the site's own code is worth trying again.
//
// Most of them are not: a password that is wrong will be wrong the
// next time as well. The one that is different is the login that has
// gone stale -- a new sign-in may well work.
func retryableCode(code int) bool {
	switch code {
	case 1200007:
		// UNAUTHENTICATED: 登录状态无效，重试一次。
		return true
	}
	return false
}

// now is the clock.
func (c *Chain) now() time.Time {
	if c.Now == nil {
		return time.Now()
	}
	return c.Now()
}

// newID makes an account id.
//
// The time alone is not enough: two accounts that are brought in in the
// same millisecond get the same id, and the second one is refused by
// the primary key. So the id has a random part too.
func (c *Chain) newID() string {
	if c.NewID != nil {
		return c.NewID()
	}
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		// 没有随机数的时候只能用时间：撞车的机会很小，撞上了也
		// 只是这一个账号白做。
		return fmt.Sprintf("acc_%d", c.now().UnixMilli())
	}
	return "acc_" + hex.EncodeToString(b)
}
