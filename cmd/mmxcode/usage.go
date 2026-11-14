package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"mmxcode/internal/usage"
)

func newUsageCmd(e *env) *cobra.Command {
	var asJSON, refresh bool
	cmd := &cobra.Command{
		Use:   "usage",
		Short: "网关内账号的积分汇总",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			if refresh {
				// 现读一遍：页面上和这里看到的数都是上一次读数时的数，
				// 想看现在的就得说一声。
				res, err := e.mgr.RefreshAllUsage(ctx)
				if err != nil {
					return err
				}
				fmt.Printf("读了 %d 个账号，失败 %d\n", res.Refreshed, res.Failed)
				for _, reason := range res.Reasons {
					fmt.Println(reason)
				}
			}
			snaps, err := e.store.LatestUsageSnapshots(ctx)
			if err != nil {
				return err
			}
			accounts, err := e.store.ListAccounts(ctx)
			if err != nil {
				return err
			}
			s := usage.Aggregate(snaps, accounts, nowUTC())
			if asJSON {
				b, err := json.MarshalIndent(s, "", "  ")
				if err != nil {
					return err
				}
				fmt.Println(string(b))
				return nil
			}
			fmt.Printf("总积分 %.2f\t总消耗 %.2f\t总剩余 %.2f\n",
				s.TotalGranted, s.TotalConsumed, s.TotalRemaining)
			if s.ExpiresAt != nil {
				fmt.Printf("最近过期 %s\n", s.ExpiresAt.Format("2006-01-02 15:04"))
			}
			if s.AccountsUnavailable > 0 {
				fmt.Printf("有 %d 个账号的积分读不到\n", s.AccountsUnavailable)
			}
			if s.AccountsUnread > 0 {
				fmt.Printf("有 %d 个账号的积分还没读过\n", s.AccountsUnread)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "输出 JSON")
	cmd.Flags().BoolVar(&refresh, "refresh", false, "现读一遍账本，不看上次读数")
	return cmd
}
