package main

import (
	"context"
	"fmt"
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
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "账号标签")
	return cmd
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
