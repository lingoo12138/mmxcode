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
//
// 找和建要在一个事务里：不然两个并发的导入会各自看见「还没有」这一
// 行，然后各建一行——同一个站点账号两行都是活的。
type Store interface {
	// Tx runs fn inside one write transaction.
	Tx(ctx context.Context, fn func(store.Tx) error) error
	// SetPassword writes an account's encrypted password.
	SetPassword(ctx context.Context, id, cipher string, updatedAt time.Time) error
	// UpdateAccountStatus writes the account's state and its last
	// error.
	UpdateAccountStatus(ctx context.Context, id, status, lastError string, updatedAt time.Time) error
	// SetAccountName writes the account's name.
	SetAccountName(ctx context.Context, id, name string, updatedAt time.Time) error
	// SetAccountProxy writes the account's outlet.
	SetAccountProxy(ctx context.Context, id, group, url string, updatedAt time.Time) error
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
	// ProxyGroup is the outlet group the accounts go out through.
	//
	// 账号从哪个口登进去的，以后它的活就从哪个口出：站点看到的是同一个
	// 账号从同一个地方来。空的是全局默认出口。
	ProxyGroup string
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
		return nil, c.webError(ctx, StageProbe, err)
	}
	if err := c.Web.Password(ctx, email, password, deviceID, redirect); err != nil {
		return nil, c.webError(ctx, StagePassword, err)
	}
	if err := c.Web.Approve(ctx, d.UserCode); err != nil {
		if errors.Is(err, webauth.ErrNoDefaultGroup) {
			// 站点没有默认组可批准，重试多少次都一样。
			return nil, &Error{Stage: StageApprove, Message: err.Error()}
		}
		return nil, c.webError(ctx, StageApprove, err)
	}
	if c.Poll == nil {
		return nil, &Error{Stage: StageToken, Message: "没有地方等这个令牌"}
	}
	tok, err := c.Poll(ctx, d)
	if err != nil {
		return nil, c.pollError(ctx, err)
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

	// 这个账号是不是已经在库里了？重跑一次导入是常事：上一批有一部分
	// 失败了，重来一遍是补救的办法。库里已经有这一行的时候再建一行，
	// 同一个站点账号就有两个活着的行——两个都在轮转池里，页面把它列
	// 两遍，而且其中一个的令牌迟早被另一个的重登作废。
	//
	// 找和写在一个事务里：查了再做的事，两个并发的导入一起做就会做两
	// 遍（网页上传两趟，或者两个进程各跑一趟）。令牌跟这一行一起写：
	// 读者不会看见一行没有令牌的账号。
	var existed bool
	var row store.Account
	err = c.Store.Tx(ctx, func(tx store.Tx) error {
		existing, found, err := findIn(ctx, tx, mail, email, tok.Subject)
		if err != nil {
			return err
		}
		notes = append(notes, found...)
		// 这一行该有的地址和用户 id：站点那边报出来的最准，问不出来就
		// 拿手里有的那个顶上。已经在库里那一行原来就有的，比清单上的
		// 地址更靠得住：那个地址是站点自己认的。
		wantMail := id.Email
		if wantMail == "" {
			wantMail = email
			if existing.ID != "" {
				wantMail = existing.Email
			}
		}
		wantUserID := id.RealUserID
		if wantUserID == "" {
			wantUserID = tok.Subject
			if existing.ID != "" && existing.UserID != "" {
				wantUserID = existing.UserID
			}
		}
		if existing.ID != "" {
			existed = true
			row = existing
			// 站点那边报出来的真邮箱和真用户 id 知道了，就把这一行对齐：
			// 邮箱是去重的键之一，键停在旧的地方，下一次导入就会把这个
			// 账号当成新账号，再插一行。
			if wantMail != row.Email || wantUserID != row.UserID {
				if err := tx.SetAccountIdentity(ctx, row.ID, wantMail, wantUserID, now); err != nil {
					return err
				}
				row.Email = wantMail
				row.UserID = wantUserID
			}
			return tx.SaveToken(ctx, store.Token{
				AccountID: row.ID,
				Access:    tok.Access,
				Refresh:   tok.Refresh,
				ExpiresAt: tok.Expires,
				Subject:   tok.Subject,
				UpdatedAt: now,
			})
		}
		acc := store.Account{
			ID:         c.newID(),
			Name:       mail,
			UserID:     wantUserID,
			Email:      wantMail,
			Status:     store.StatusActive,
			ProxyGroup: c.ProxyGroup,
			CreatedAt:  now,
			UpdatedAt:  now,
		}
		if err := tx.CreateAccount(ctx, acc); err != nil {
			return err
		}
		if err := tx.SaveToken(ctx, store.Token{
			AccountID: acc.ID,
			Access:    tok.Access,
			Refresh:   tok.Refresh,
			ExpiresAt: tok.Expires,
			Subject:   tok.Subject,
			UpdatedAt: now,
		}); err != nil {
			return err
		}
		row = acc
		return nil
	})
	if err != nil {
		msg := err.Error()
		if existed {
			// 原来那一行没动过：它还带着一条能用的链，报错就是，不用
			// 把这一行删掉——删掉就是毁掉一个还能用的账号。
			msg = "令牌没能存下，库里原来那一行没动：" + msg
		}
		return Result{Email: email}, &Error{Stage: StageStore, Message: msg}
	}

	if existed {
		notes = c.refresh(ctx, row, id.Email, now, notes)
		notes = append(notes, "这个账号已经在库里了，更新了原来那一行")
	}
	notes, stored := c.keep(ctx, row.ID, password, now, notes)
	res := Result{Email: email, OK: true, AccountID: row.ID, Message: strings.Join(notes, "；")}
	if !stored {
		res.Stage = StageStore
	}
	return res, nil
}

// refresh puts the new sign-in on the row that is already there.
//
// 状态、名字、出口组这三样在事务外写：它们各写各的列，写不进去不
// 影响别的——令牌已经是新的了，只是状态还没跟上，这种事要说出来，不
// 是导入失败。
//
// 名字只在真的问出身份的时候才改：问不出来的时候手里没有真名字，拿
// 清单上的邮箱把原来的名字盖掉，是把知道的东西换成不知道的。
func (c *Chain) refresh(ctx context.Context, acc store.Account,
	knownName string, now time.Time, notes []string) []string {
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
	// 出口组：导入是从哪个口登的，账号以后就走哪个口。没带组的导入不动
	// 原来那一行的组和地址——那是人自己设的，不能因为一次没带组的导入就没了。
	//
	// 带了组的时候，账号自己那个地址要让位：出口是按「先看地址、再看组」
	// 找的（internal/egress），地址留着就还是从那个地方出去，组写着是 hk
	// 也没用。导入是人明确做的一件事，它带的组说了算，所以那个地址清掉，
	// 而且要说出来——地址没了是件人该知道的事。
	if c.ProxyGroup != "" && (c.ProxyGroup != acc.ProxyGroup || acc.ProxyURL != "") {
		if err := c.Store.SetAccountProxy(ctx, acc.ID, c.ProxyGroup, "", now); err != nil {
			notes = append(notes, "出口组没能更新："+err.Error())
		} else if acc.ProxyURL != "" {
			// 被清掉的是哪个地址，得写出来：那是一个人自己设的地址，
			// 没了就得能照着填回去。
			notes = append(notes, "账号自己那个出口地址（"+acc.ProxyURL+
				"）清掉了，改走导入带的出口组 "+c.ProxyGroup)
		}
	}
	return notes
}

// findIn gives the account row that belongs to this site account.
//
// 三样都问一遍，先问最靠得住的那一样：
//
//   - subject：令牌里带的那个用户 id。一个站点账号一个，重登多少次都是
//     它。清单上的邮箱可能是别名，站点那边报的邮箱又可能一时问不出来，
//     只有它是一直在的。
//   - 站点那边报的邮箱
//   - 清单上写的邮箱
//
// 找到两样却指向两个不一样的行，说明库里本来就有同一个站点账号的两行：
// 那不是这次导入造成的，但要说出来——两行都在轮转池里，页面把它列两遍。
//
// 找不到不是错：那就是一个新账号。
func findIn(ctx context.Context, tx store.Tx, mail, email, subject string) (store.Account, []string, error) {
	var notes []string
	var found store.Account
	have := false
	add := func(acc store.Account) {
		if !have {
			found, have = acc, true
			return
		}
		if acc.ID != found.ID {
			notes = append(notes, "库里已经有同一个站点账号的两行了（"+
				found.ID+" 和 "+acc.ID+"）")
		}
	}
	if subject != "" {
		acc, err := tx.AccountBySubject(ctx, subject)
		switch {
		case err == nil:
			add(acc)
		case errors.Is(err, store.ErrNotFound):
		default:
			return store.Account{}, nil, err
		}
	}
	asked := map[string]bool{}
	for _, key := range []string{mail, email} {
		if key == "" || asked[strings.ToLower(key)] {
			continue
		}
		asked[strings.ToLower(key)] = true
		acc, err := tx.AccountByEmail(ctx, key)
		switch {
		case err == nil:
			add(acc)
		case errors.Is(err, store.ErrNotFound):
		default:
			return store.Account{}, nil, err
		}
	}
	return found, notes, nil
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

// webError turns what the site said into what the runner needs to
// know: the stage, the site's own code, and whether trying again could
// work.
//
// A wrong password is not worth trying again. A dropped connection is.
//
// ctx 是这一趟导入的上下文：它没了的时候这一趟就结束了，再试也是白试，
// 那一条不该再说「值得再试一次」。
func (c *Chain) webError(ctx context.Context, stage Stage, err error) error {
	dead := ctx.Err() != nil
	var se *webauth.SiteError
	if errors.As(err, &se) {
		pe := &Error{
			Stage: stage,
			Code:  se.Code,
			// ExplainText 是一句话的说法：有错误码就用错误码，没有
			// 就用站点自己那个词（login_required 那种）。
			Message: se.ExplainText(),
		}
		if retryableCode(se.Code) && !dead {
			pe.Retry = true
		}
		return pe
	}
	return &Error{Stage: stage, Message: err.Error(), Retry: !dead}
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
//
// ctx 是这一趟导入的上下文：只有它自己没了的时候，超时才算「不用再试
// 了」。客户端自己的超时（http.Client.Timeout）报出来的错也是
// context.DeadlineExceeded，但那时候这个上下文还好好的活着——那是连接
// 那边的事，跟前面几步的连接断了是一回事，值得再试一次。
func (c *Chain) pollError(ctx context.Context, err error) error {
	// 上下文没了：再试也是白试，等的那一下也是白等。
	if ctx.Err() != nil {
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
