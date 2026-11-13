package main

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
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
				proxy := a.ProxyURL
				if proxy == "" {
					proxy = a.ProxyGroup
				}
				if proxy == "" {
					proxy = e.cfg.Proxy.Default
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
			return e.store.DeleteAccount(context.Background(), args[0])
		},
	}
}

func newAccountsProxyCmd(e *env) *cobra.Command {
	var (
		group string
		url   string
	)
	cmd := &cobra.Command{
		Use:   "proxy <id>",
		Short: "给账号指定出口组或单独的代理地址",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			a, err := e.store.GetAccount(ctx, args[0])
			if err != nil {
				return err
			}
			if group != "" {
				// 组名写错会让账号悄悄改走全局默认。挡在这里。
				if _, ok := e.cfg.Proxy.Groups[group]; !ok {
					return fmt.Errorf("no such outlet group %q; known: %s", group, groupNames(e.cfg.Proxy.Groups))
				}
				a.ProxyGroup = group
			}
			if url != "" {
				a.ProxyURL = url
			}
			a.UpdatedAt = nowUTC()
			return e.store.UpdateAccount(ctx, a)
		},
	}
	cmd.Flags().StringVar(&group, "group", "", "出口组名")
	cmd.Flags().StringVar(&url, "url", "", "单独的代理地址")
	return cmd
}
