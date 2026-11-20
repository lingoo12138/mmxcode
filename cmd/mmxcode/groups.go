package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"mmxcode/internal/egress"
	"mmxcode/internal/store"
)

func newGroupsCmd(e *env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "groups",
		Short: "出口组",
	}
	cmd.AddCommand(newGroupsListCmd(e))
	cmd.AddCommand(newGroupsSetCmd(e))
	cmd.AddCommand(newGroupsRemoveCmd(e))
	return cmd
}

func newGroupsListCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "列出出口组",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			groups, err := e.store.ListProxyGroups(ctx)
			if err != nil {
				return err
			}
			counts, err := e.store.AccountGroups(ctx)
			if err != nil {
				return err
			}
			if len(groups) == 0 {
				fmt.Println("还没有出口组。")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "组\t地址\t账号")
			for _, g := range groups {
				name := g.Name
				if g.Name == store.GlobalGroup {
					name = g.Name + "（全局默认）"
				}
				if len(g.URLs) == 0 {
					fmt.Fprintf(w, "%s\t（空）\t%d\n", name, counts[g.Name])
					continue
				}
				for i, u := range g.URLs {
					label := ""
					if i == 0 {
						label = name
					}
					// 地址里的账号和密码是秘密：打出来的是去了凭据的。
					fmt.Fprintf(w, "%s\t%s\t%d\n", label, egress.Redacted(u), counts[g.Name])
				}
			}
			return w.Flush()
		},
	}
}

func newGroupsSetCmd(e *env) *cobra.Command {
	var (
		name string
		urls []string
	)
	cmd := &cobra.Command{
		Use:   "set",
		Short: "建一个出口组或者改它",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			name = strings.TrimSpace(name)
			if name == "" {
				return fmt.Errorf("the group has no name")
			}
			if err := e.store.SaveProxyGroup(ctx, store.ProxyGroup{
				Name: name, URLs: urls, CreatedAt: nowUTC(), UpdatedAt: nowUTC(),
			}); err != nil {
				return err
			}
			fmt.Printf("the outlet group %q has %d address(es)\n", name, len(urls))
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "组的名字")
	cmd.Flags().StringArrayVar(&urls, "url", nil, "组里的地址（可以给多次，顺序就是顺序）")
	return cmd
}

func newGroupsRemoveCmd(e *env) *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "rm",
		Short: "删掉一个出口组",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			back, err := e.store.DeleteProxyGroup(ctx, name)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return fmt.Errorf("no such group: %s", name)
				}
				return err
			}
			fmt.Printf("the outlet group %q is gone; %d account(s) follow the global group now\n",
				name, back)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "组的名字")
	return cmd
}
