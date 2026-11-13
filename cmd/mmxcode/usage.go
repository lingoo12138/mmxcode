package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"mmxcode/internal/usage"
)

func newUsageCmd(e *env) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "usage",
		Short: "网关内账号的积分汇总",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			snaps, err := e.store.LatestUsageSnapshots(ctx)
			if err != nil {
				return err
			}
			accounts, err := e.store.ListAccounts(ctx)
			if err != nil {
				return err
			}
			emails := map[string]string{}
			for _, a := range accounts {
				emails[a.ID] = a.Email
			}
			s := usage.Aggregate(snaps, nowUTC())
			for i := range s.Accounts {
				s.Accounts[i].Email = emails[s.Accounts[i].ID]
			}
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
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "输出 JSON")
	return cmd
}
