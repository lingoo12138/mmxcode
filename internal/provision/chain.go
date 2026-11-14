package provision

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
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
	// AccountByEmail gives the account with that email, when there is
	// one. It says store.ErrNotFound when there is none.
	AccountByEmail(ctx context.Context, email string) (store.Account, error)
	// SetAccountName writes the account's name.
	SetAccountName(ctx context.Context, id, name string, updatedAt time.Time) error
	// UpdateAccountStatus writes the account's state and its last
	// error.
	UpdateAccountStatus(ctx context.Context, id, status, lastError string, updatedAt time.Time) error
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
		return nil, &Error{Stage: StageToken, Message: err.Error(), Retry: true}
	}
	deviceID, err := c.Web.RegisterDevice(ctx)
	if err != nil {
		return nil, &Error{Stage: StageProbe, Message: err.Error(), Retry: true}
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
		return nil, c.pollError(err)
	}
	return tok, nil
}

// Import signs one account in and puts it in the database.
//
// An account that is already in the database is signed in again and its
// row is updated: one site account must not have two live rows.
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
	userID := id.RealUserID
	if userID == "" {
		userID = tok.Subject
	}

	// 这个账号是不是已经在库里了？重跑一次导入是常事：上一批有一部分
	// 失败了，重来一遍是补救的办法。库里已经有这一行的时候再建一行，
	// 同一个站点账号就有两个活着的行——两个都在轮转池里，页面把它列
	// 两遍，而且其中一个的令牌迟早被另一个的重登作废。
	existing, err := c.find(ctx, mail, email)
	if err != nil {
		return Result{Email: email}, &Error{Stage: StageStore, Message: err.Error()}
	}
	if existing.ID != "" {
		return c.update(ctx, email, password, existing, tok, id.Email, now, notes)
	}

	acc := store.Account{
		ID:        c.newID(),
		Name:      mail,
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
	notes, stored := c.keep(ctx, acc.ID, password, now, notes)
	res := Result{Email: email, OK: true, AccountID: acc.ID, Message: strings.Join(notes, "；")}
	if !stored {
		res.Stage = StageStore
	}
	return res, nil
}

// find gives the account row with that email, when there is one.
//
// 两个邮箱都问一遍：站点那边认的邮箱和清单上写的不一定一样（清单上
// 写的可能是别名），哪一栏写着这个号，就按哪一栏找。
//
// 找不到不是错：那就是一个新账号。
func (c *Chain) find(ctx context.Context, mail, email string) (store.Account, error) {
	acc, err := c.Store.AccountByEmail(ctx, mail)
	if err == nil {
		return acc, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return store.Account{}, err
	}
	if mail == email {
		return store.Account{}, nil
	}
	acc, err = c.Store.AccountByEmail(ctx, email)
	if errors.Is(err, store.ErrNotFound) {
		return store.Account{}, nil
	}
	if err != nil {
		return store.Account{}, err
	}
	return acc, nil
}

// update signs the account in again and puts the new sign-in on the row
// that is already there.
//
// 令牌先写：写失败的时候库里那一行没动过，它原来那条链还是好的，报错
// 就是，不用把这一行删掉——删掉就是毁掉一个还能用的账号。
//
// 名字只在真的问出身份的时候才改：问不出来的时候手里没有真名字，
// 拿清单上的邮箱把原来的名字盖掉，是把知道的东西换成不知道的。
func (c *Chain) update(ctx context.Context, email, password string, acc store.Account,
	tok *oauth.Token, knownName string, now time.Time, notes []string) (Result, error) {
	if err := c.Store.SaveToken(ctx, store.Token{
		AccountID: acc.ID,
		Access:    tok.Access,
		Refresh:   tok.Refresh,
		ExpiresAt: tok.Expires,
		Subject:   tok.Subject,
		UpdatedAt: now,
	}); err != nil {
		return Result{Email: email}, &Error{Stage: StageStore,
			Message: "令牌没能存下，库里原来那一行没动：" + err.Error()}
	}
	// 它刚刚登进去了：状态回到 active。这笔写不进去不是导入失败——
	// 令牌已经是新的了，只是状态还没跟上，要说出来。
	if err := c.Store.UpdateAccountStatus(ctx, acc.ID, store.StatusActive, "", now); err != nil {
		notes = append(notes, "令牌换新了，但状态没能写成 active："+err.Error())
	}
	if knownName != "" && knownName != acc.Name {
		if err := c.Store.SetAccountName(ctx, acc.ID, knownName, now); err != nil {
			notes = append(notes, "名字没能更新："+err.Error())
		}
	}
	notes, _ = c.keep(ctx, acc.ID, password, now, notes)
	notes = append(notes, "这个账号已经在库里了，更新了原来那一行")
	return Result{Email: email, OK: true, AccountID: acc.ID, Message: strings.Join(notes, "；")}, nil
}

// keep encrypts the password and writes it down, when there is a
// keeper.
//
// It says whether the password made it: an account that is in without a
// password cannot sign itself back in, and the person has to know.
func (c *Chain) keep(ctx context.Context, id, password string, now time.Time, notes []string) ([]string, bool) {
	if c.Keeper == nil {
		return notes, true
	}
	cipher, err := c.Keeper(password)
	if err != nil {
		return append(notes, "密码没能加密存下："+err.Error()), false
	}
	if err := c.Store.SetPassword(ctx, id, cipher, now); err != nil {
		return append(notes, "密码没能存下："+err.Error()), false
	}
	return notes, true
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
		}
		return pe
	}
	return &Error{Stage: stage, Message: err.Error(), Retry: true}
}

// pollError turns what the token endpoint said into what the runner
// needs to know.
//
// The refusal and the transport failure are told apart: a device code
// that is used up or refused will be refused again, and a dropped
// connection is worth another go. The same goes for a failure one step
// earlier in the chain, and the two are told apart the same way.
//
// 设计文档 §5.3：网络错误、5xx、超时退避重试 2 次（1s、4s）；429 退避
// 30s 重试 1 次；设备码过期不重试。
func (c *Chain) pollError(err error) error {
	// 上下文没了：再试也是白试，等的那一下也是白等。
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return &Error{Stage: StageToken, Message: err.Error()}
	}
	var oe *oauth.Error
	if errors.As(err, &oe) {
		switch oe.Code {
		case "expired_token", "access_denied":
			// 设备码过期了，或者站点不让这个码进来：再来一次也是这个
			// 答案，下一个账号重新开一个设备码就是了。
			return &Error{Stage: StageToken, Message: oe.Error()}
		case "missing_refresh_token":
			// 没有刷新令牌的账号活不过一小时：这是账号的事，不是连接
			// 的事，再来一次也是没有。
			return &Error{Stage: StageToken, Message: oe.Error()}
		}
		if oe.HTTPStatus == http.StatusTooManyRequests {
			// 站点在限流：等久一点再来一次。这样的错试三次没有意义，
			// 去了只是让它更烦。
			return &Error{Stage: StageToken, Message: oe.Error(), Retry: true,
				Wait: rateLimitWait, Tries: 2}
		}
		// 5xx 和别的站点自己的错：再来一次可能就成了。
		return &Error{Stage: StageToken, Message: oe.Error(), Retry: true}
	}
	// 传输上的问题：连接断了、读超时了。
	return &Error{Stage: StageToken, Message: err.Error(), Retry: true}
}

// rateLimitWait is how long to wait when the site says it is limiting
// how much it answers.
//
// 设计文档 §5.3：429、站点风控退避更久（30s）重试 1 次。
const rateLimitWait = 30 * time.Second

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
