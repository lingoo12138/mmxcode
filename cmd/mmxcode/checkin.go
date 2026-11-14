package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"mmxcode/internal/checkin"
)

func newCheckinCmd(e *env) *cobra.Command {
	var accountID string
	cmd := &cobra.Command{
		Use:   "checkin",
		Short: "签到",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			list, err := e.store.ListAccounts(ctx)
			if err != nil {
				return err
			}
			failed := false
			for _, a := range list {
				if accountID != "" && a.ID != accountID {
					continue
				}
				// 走 manager 那条路：它先续期，再用账号自己的出口，
				// 签到记录也由它写（日子是站点的那一天）。
				//
				// 命令自己读库里的令牌是不续期的。一个 90 分钟没续过
				// 的令牌发过去，站点回 401，签到根本没发生，而命令
				// 还会说成功。
				out, err := e.mgr.Checkin(ctx, a.ID)
				if err != nil {
					fmt.Printf("%s\t失败：%v\n", a.Name, err)
					failed = true
					continue
				}
				fmt.Printf("%s\t%s\t积分 %g\t连续 %d 天\n", a.Name, out.Outcome, out.Credit, out.Streak)
				if out.Outcome == checkin.OutcomeFailed {
					// 被拒的签到不是签到成功。命令要失败，cron 才
					// 知道今天什么都没领到。
					msg := out.Message
					if msg == "" {
						msg = "the sign-in was refused"
					}
					fmt.Printf("%s\t失败：%s\n", a.Name, msg)
					failed = true
				}
			}
			if failed {
				return fmt.Errorf("some accounts failed")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&accountID, "account", "", "只签到这一个账号")
	return cmd
}
