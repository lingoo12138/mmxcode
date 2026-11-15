package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"mmxcode/internal/oauth"
	"mmxcode/internal/site"
	"mmxcode/internal/store"
)

func newLoginCmd(e *env) *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "login",
		Short: "登录一个 MiniMax 账号",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			d, err := oauth.StartDevice(ctx, e.up, site.AccountURL(site.DeviceCodePath))
			if err != nil {
				return err
			}
			fmt.Printf("打开 %s\n", d.URL)
			fmt.Printf("输入验证码：%s\n", d.UserCode)
			fmt.Println("等待登录……")

			tok, err := d.Poll(ctx, e.up, site.AccountURL(site.TokenPath))
			if err != nil {
				return err
			}

			id, err := e.acc.Identity(ctx, tok.Access, tok.Subject)
			if err != nil {
				fmt.Printf("登录成功，但读不到账户信息：%v\n", err)
			}

			now := time.Now()
			// 库里已经有这个站点账号了（多半是批量导入进来的）：改那一行，
			// 不再另起一行——同一个站点账号两行都是活的，轮转会挑到两次。
			acc, err := e.store.SignIn(ctx, store.Account{
				ID:        newAccountID(),
				Name:      name,
				UserID:    firstNonEmpty(id.RealUserID, tok.Subject),
				Email:     id.Email,
				Status:    store.StatusActive,
				CreatedAt: now,
				UpdatedAt: now,
			}, store.Token{
				Access:    tok.Access,
				Refresh:   tok.Refresh,
				ExpiresAt: tok.Expires,
				Subject:   tok.Subject,
			})
			if err != nil {
				return err
			}
			fmt.Printf("已登录：%s (%s)\n", acc.Name, acc.ID)
			readCredits(ctx, e, cmd.OutOrStdout(), acc.ID)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "账号标签")
	return cmd
}

// readCredits reads one account's credits now.
//
// 刚登进来的账号马上读一次账本：不等调度器那一趟（usage.refresh.scan_every，
// 默认 30 秒；没配它的时候才用老的 schedule.usage_refresh），人才看得到
// 它有多少积分。读不到不算登录失败——账号是进来了的，账本读不到是另一件
// 事，但要说出来。
func readCredits(ctx context.Context, e *env, out io.Writer, accountID string) {
	if e.mgr == nil || accountID == "" {
		return
	}
	snap, err := e.mgr.RefreshUsage(ctx, accountID)
	if err != nil {
		fmt.Fprintf(out, "积分读不到：%v\n", err)
		return
	}
	if !snap.HasAmounts {
		fmt.Fprintln(out, "积分读不到。")
		return
	}
	fmt.Fprintf(out, "积分：剩 %.2f（发了 %.2f，花了 %.2f）\n",
		snap.Remaining, snap.Granted, snap.Consumed)
}

// newAccountID makes an account id.
//
// The time is enough here: an account is signed in by hand, one at a
// time, and the second one a second later would not collide.
func newAccountID() string {
	return fmt.Sprintf("acc_%d", time.Now().UnixMilli())
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
