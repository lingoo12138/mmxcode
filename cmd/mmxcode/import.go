package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"mmxcode/internal/account"
	"mmxcode/internal/manager"
	"mmxcode/internal/oauth"
	"mmxcode/internal/provision"
	"mmxcode/internal/secret"
	"mmxcode/internal/site"
	"mmxcode/internal/store"
	"mmxcode/internal/upstream"
	"mmxcode/internal/webauth"
)

// PasswordKeyEnv is the environment variable that holds the key the
// imported passwords are encrypted with.
//
// The key is not in the configuration file: the file and the database
// usually sit in the same volume, and a key next to the cipher text
// is no key at all.
const PasswordKeyEnv = "MMXCODE_PASSWORD_KEY"

// webTimeout is how long one request to the account site may take.
//
// The site answers in seconds: the device code, the sign-in and the
// approval are all quick. A request that has not answered in half a
// minute is not going to.
const webTimeout = 30 * time.Second

func newImportCmd(e *env) *cobra.Command {
	var file string
	var group string
	var interval time.Duration
	var noStore bool
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "import",
		Short: "按清单批量登录账号",
		Long: "读一份每行「邮箱|密码」的清单，把这些账号自动登录进来。\n" +
			"密码登录之后默认加密存进库里（密钥在 " + PasswordKeyEnv + "），\n" +
			"没有这个环境变量就登录一次就丢。",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if timeout > 0 {
				// 整趟导入有个时限：清单很大的时候，人不愿意等到
				// 天亮才知道它卡住了。
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, timeout)
				defer cancel()
			}
			return runImport(ctx, e, cmd.OutOrStdout(), file, group, interval, noStore)
		},
	}
	cmd.Flags().StringVar(&file, "file", "", "账号清单（每行 邮箱|密码）")
	cmd.Flags().StringVar(&group, "group", "", "每个账号走哪个出口组；账号自己那个出口地址会被这个组换掉")
	cmd.Flags().DurationVar(&interval, "interval", 3*time.Second, "每个账号之间的间隔")
	cmd.Flags().BoolVar(&noStore, "no-store-password", false, "登录成功就丢密码，不入库")
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "整趟导入的时限（0 = 不限）")
	_ = cmd.MarkFlagRequired("file")
	return cmd
}

// runImport brings in the accounts of a list.
func runImport(ctx context.Context, e *env, out io.Writer, file, group string, interval time.Duration, noStore bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	entries, bad := provision.Parse(bytes.NewReader(raw))
	fmt.Fprintf(out, "读入 %d 个账号。\n", len(entries))
	for _, b := range bad {
		// 坏行的那句话里没有密码：清单的解析器把密码摘掉了，
		// 这一行是要写进数据库的导入留痕里的。
		fmt.Fprintf(out, "第 %d 行：%s（%s）\n", b.Line, b.Reason, b.Text)
	}
	if len(entries) == 0 {
		return fmt.Errorf("清单里没有能用的账号")
	}

	keeper, err := passwordKeeper(noStore)
	if err != nil {
		return err
	}
	runner, err := e.runner(group, interval, keeper)
	if err != nil {
		return err
	}

	runID := newRunID()
	if e.store != nil {
		if err := e.store.CreateImportRun(ctx, store.ImportRun{
			ID: runID, Source: "cli", StartedAt: time.Now(), Total: len(entries),
		}); err != nil {
			return err
		}
	}

	results, runErr := runner.Run(ctx, entries)
	// 清单里的第几行：出错的账号要能找回去。
	lines := map[string]int{}
	for _, en := range entries {
		lines[strings.ToLower(en.Email)] = en.Line
	}
	ok := 0
	for _, r := range results {
		if r.OK {
			ok++
			fmt.Fprintf(out, "  ✔ %s\t入库成功", r.Email)
			// 账号进来了但密码没存下，也要说：不然下一次掉线时，
			// 这个账号只能等人手工重新入号。
			if r.Message != "" {
				fmt.Fprintf(out, "（%s）", r.Message)
			}
			fmt.Fprintln(out)
			// 刚进来的账号马上读一次账本：不等调度器那一趟（15 分
			// 钟），人才看得到它有多少积分。读不到不算导入失败——
			// 账号是进来了的，账本读不到是另一件事。
			if e.mgr != nil && r.AccountID != "" {
				if _, err := e.mgr.RefreshUsage(ctx, r.AccountID); err != nil {
					fmt.Fprintf(out, "      积分读不到：%v\n", err)
				}
			}
			continue
		}
		fmt.Fprintf(out, "  ✘ %s\t%s：%s", r.Email, r.Stage, r.Message)
		if line, known := lines[strings.ToLower(r.Email)]; known {
			fmt.Fprintf(out, "（第 %d 行", line)
		} else {
			fmt.Fprint(out, "（")
		}
		if r.Code != 0 {
			fmt.Fprintf(out, "错误码 %d", r.Code)
		}
		fmt.Fprintln(out, "）")
	}
	fmt.Fprintf(out, "\n已处理 %d 个，成功 %d，失败 %d。\n", len(results), ok, len(results)-ok)
	switch {
	case keeper != nil:
		fmt.Fprintf(out, "密码已加密入库（%s）。\n", PasswordKeyEnv)
	case noStore:
		fmt.Fprintln(out, "密码用完就丢（不入库）。")
	default:
		// 只有「没设密钥」才要说这件事：--no-store-password 是人自己
		// 说的，说他「没有密钥」是冤枉他。
		fmt.Fprintf(out, "密码没有存进库里（没有 %s）。\n", PasswordKeyEnv)
	}
	if runErr != nil {
		// 说清楚这趟是被掐断的：不是所有账号都失败了，是有人不想让它
		// 再跑下去了。已经进来的那些在上面写着。
		fmt.Fprintf(out, "清单里共 %d 个，还有 %d 个没轮到：这趟导入被中途截断了（%v）。\n",
			len(entries), len(entries)-len(results), runErr)
	}
	if e.store != nil {
		// 收尾的这一笔不能被掐断的上下文掐掉：时限到了的时候，已经
		// 进来的那些账号要留在记录里，不能白跑一场。
		if err := e.store.FinishImportRun(context.WithoutCancel(ctx), runID, time.Now(),
			ok, len(results)-ok, provision.ResultsJSON(results)); err != nil {
			// 收尾没写成也是错，但截断的原因不能跟着它一起没了。
			return errors.Join(runErr, err)
		}
	}
	return runErr
}

// passwordKeeper gives the function that encrypts a password, or nil
// when the password is not to be kept.
//
// The key is not set: no key, no password kept. The key is set but
// wrong: that is a mistake, and the import stops on it -- quietly
// throwing the password away would leave a database of accounts that
// nobody can sign in again.
func passwordKeeper(noStore bool) (func(string) (string, error), error) {
	if noStore {
		return nil, nil
	}
	if strings.TrimSpace(os.Getenv(PasswordKeyEnv)) == "" {
		return nil, nil
	}
	key, err := secret.FromEnv(PasswordKeyEnv)
	if err != nil {
		return nil, err
	}
	return key.Seal, nil
}

// reloginFn makes the function that signs an account in again.
//
// The account is already in the database: only the token is new. The
// way out is the account's own proxy group, so that the sign-in comes
// from where the account usually comes from — unless the person
// asked for another group.
func (e *env) reloginFn(group string) manager.ReloginFunc {
	return func(ctx context.Context, acc store.Account, password string) error {
		if group != "" {
			// 人指名的组得是真的：组名写错而默默走全局默认出口，
			// 看着跑通了，其实请求根本没走要去的那条路。
			if _, ok := e.cfg.Proxy.Groups[group]; !ok {
				return fmt.Errorf("no such outlet group %q; known: %s", group, groupNames(e.cfg.Proxy.Groups))
			}
		}
		g := group
		if g == "" {
			g = acc.ProxyGroup
		}
		chain := e.chain(g, nil)
		tok, err := chain.SignIn(ctx, acc.Email, password)
		if err != nil {
			return err
		}
		return e.store.SaveToken(ctx, store.Token{
			AccountID: acc.ID,
			Access:    tok.Access,
			Refresh:   tok.Refresh,
			ExpiresAt: tok.Expires,
			Subject:   tok.Subject,
			UpdatedAt: time.Now(),
		})
	}
}

// runner makes the batch driver.
//
// The way out is settled once, here, and not once per account: an
// address that does not work is a mistake in the configuration, and it
// is said once rather than once per account.
func (e *env) runner(group string, interval time.Duration, keeper func(string) (string, error)) (*provision.Runner, error) {
	if e.makeRunner != nil {
		return e.makeRunner(group, interval, keeper), nil
	}
	// 出口组先查一遍：组名写错而默默走全局默认出口，是最坏的一种
	// 错——看着跑通了，其实请求根本没走要去的那条路。
	if group != "" {
		if _, ok := e.cfg.Proxy.Groups[group]; !ok {
			return nil, fmt.Errorf("no such outlet group %q; known: %s", group, groupNames(e.cfg.Proxy.Groups))
		}
	}
	if _, err := e.egress(group); err != nil {
		return nil, err
	}
	return &provision.Runner{
		Importer: &provision.Importer{
			Import: func(ctx context.Context, email, password string) (provision.Result, error) {
				chain := e.chain(group, keeper)
				return chain.Import(ctx, email, password)
			},
		},
		Interval: interval,
	}, nil
}

// egress settles the way out of a group.
//
// The group says which way the traffic goes: an empty group is the
// global default.
func (e *env) egress(group string) (http.RoundTripper, error) {
	proxyURL := e.egr.Resolve(store.Account{ProxyGroup: group})
	return e.egr.Transport(proxyURL)
}

// chain makes the thing that does the real work.
//
// The keeper encrypts the password so that it can be kept; a nil one
// throws the password away after the sign-in.
//
// Every call makes a new chain, and a new chain has a new web client:
// the site keeps the session in a cookie, and two accounts sharing one
// session would be signing in as each other.
func (e *env) chain(group string, keeper func(string) (string, error)) *provision.Chain {
	tr, trErr := e.egress(group)
	up := e.up
	hc := &http.Client{Timeout: webTimeout}
	if trErr == nil {
		// The sign-in, the device code and the token all go out the
		// same way: the site sees one account from one address.
		hc.Transport = tr
		up = upstream.New(upstream.Options{Timeout: webTimeout, Transport: tr})
	}
	web := webauth.New(webauth.Options{
		BaseURL: site.AccountHost,
		HTTP:    hc,
		Lang:    site.Lang,
		// 网页那一套的公共 query 里有 timezone_offset：东八区是
		// 28800 秒，正是浏览器给的那个数。
		TZOffset: localTZOffset(),
	})
	c := &provision.Chain{
		Web: web,
		Start: func(ctx context.Context) (*oauth.Device, error) {
			if trErr != nil {
				return nil, trErr
			}
			return oauth.StartDevice(ctx, up, site.AccountURL(site.DeviceCodePath))
		},
		Poll: func(ctx context.Context, d *oauth.Device) (*oauth.Token, error) {
			return d.Poll(ctx, up, site.AccountURL(site.TokenPath))
		},
		Identity: func(ctx context.Context, access, subject string) (account.Identity, error) {
			// 账户信息也从这条路走：换了出口组却用旧出口读账户，
			// 站点看到的是两个地方来的同一个号。
			return e.acc.With(up).Identity(ctx, access, subject)
		},
		Keeper: keeper,
		// 账号从哪个口登进去的，以后它的活就从哪个口出：站点看到的
		// 是同一个账号从同一个地方来，而不是两个地方。
		ProxyGroup: group,
		Now:        time.Now,
		NewID:      newBatchAccountID,
	}
	if e.store != nil {
		c.Store = e.store
	}
	return c
}

// localTZOffset gives the timezone offset in seconds east of UTC.
//
// It is the number the browser sends: the site's web query carries
// -60 × getTimezoneOffset(), and JavaScript counts the minutes west
// of UTC, so the sign is the other way round.
func localTZOffset() int {
	_, offset := time.Now().Zone()
	return offset
}

// newRunID makes the name of one import run.
//
// The time alone is not enough: two runs that start in the same
// millisecond would write the same row, and the second one would be
// refused.
func newRunID() string {
	if r := randomHex(4); r != "" {
		return fmt.Sprintf("run_%d_%s", time.Now().UnixMilli(), r)
	}
	return fmt.Sprintf("run_%d", time.Now().UnixMilli())
}

// newBatchAccountID makes an account id for a batch import.
//
// The time alone is not enough: a batch signs in many accounts in a
// row, and two of them can land in the same millisecond.
func newBatchAccountID() string {
	if r := randomHex(4); r != "" {
		return fmt.Sprintf("acc_%d_%s", time.Now().UnixMilli(), r)
	}
	return fmt.Sprintf("acc_%d", time.Now().UnixMilli())
}

// randomHex gives a few random bytes as hex, or an empty string when
// there are none to give.
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return hex.EncodeToString(b)
}
