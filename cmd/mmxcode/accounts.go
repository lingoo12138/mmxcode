package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/lingoo12138/mmxcode/internal/egress"
	"github.com/lingoo12138/mmxcode/internal/store"
)

func newAccountsCmd(e *env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "accounts",
		Short: "账号管理",
	}
	cmd.AddCommand(newAccountsListCmd(e))
	cmd.AddCommand(newAccountsRemoveCmd(e))
	cmd.AddCommand(newAccountsProxyCmd(e))
	return cmd
}

func newAccountsListCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "列出账号",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			list, err := e.store.ListAccounts(ctx)
			if err != nil {
				return err
			}
			if len(list) == 0 {
				fmt.Println("还没有账号。先跑 mmxcode login。")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "ID\t标签\t邮箱\t状态\t出口组\t代理")
			for _, a := range list {
				// 代理地址里的账号和密码是秘密，而这份输出会进到终端的
				// 回滚里，也会进到 CI 的日志里。egress.Redacted 就是
				// 为了这个。账号没有自己的地址就写全局默认那个。
				proxy := egress.Redacted(a.ProxyURL)
				if proxy == "" {
					proxy = egress.Redacted(e.cfg.Proxy.Default)
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
					a.ID, a.Name, a.Email, a.Status, a.ProxyGroup, proxy)
			}
			return w.Flush()
		},
	}
}

func newAccountsRemoveCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "remove <id>",
		Short: "删除账号",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			err := e.store.DeleteAccount(ctx, args[0])
			if errors.Is(err, store.ErrNotFound) {
				// 存储层说的是"没有这一行"，人说的是"没有这个账号"。
				return fmt.Errorf("no such account: %s", args[0])
			}
			return err
		},
	}
}

func newAccountsProxyCmd(e *env) *cobra.Command {
	var url, group string
	cmd := &cobra.Command{
		Use:   "proxy <id>",
		Short: "给账号指定单独的代理地址或者出口组",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			a, err := e.store.GetAccount(ctx, args[0])
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return fmt.Errorf("no such account: %s", args[0])
				}
				return err
			}
			// 地址和组是二选一：账号只有一条出口。
			if cmd.Flags().Changed("url") && cmd.Flags().Changed("group") {
				return fmt.Errorf("the outlet is an address or a group, not both")
			}
			if cmd.Flags().Changed("url") {
				a.ProxyURL = url
				a.ProxyGroup = ""
			}
			if cmd.Flags().Changed("group") {
				a.ProxyGroup = group
				a.ProxyURL = ""
			}
			// 只写出口那两列。整行写回去会把别的写者在这中间改的东西抹掉：
			// 登出刚写下的状态，会被这一次改代理抹回 active。
			return e.store.SetAccountOutlet(ctx, a.ID, a.ProxyURL, a.ProxyGroup, nowUTC())
		},
	}
	cmd.Flags().StringVar(&url, "url", "", "单独的代理地址")
	cmd.Flags().StringVar(&group, "group", "", "出口组的名字")
	return cmd
}
