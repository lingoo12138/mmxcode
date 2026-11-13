package main

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"mmxcode/internal/checkin"
	"mmxcode/internal/store"
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
				tok, err := e.tokenFor(ctx, a.ID)
				if err != nil {
					fmt.Printf("%s\t%s\n", a.Name, err)
					failed = true
					continue
				}
				// 每个账号走自己的出口
				acc, err := e.accFor(a)
				if err != nil {
					fmt.Printf("%s\t失败：%v\n", a.Name, err)
					failed = true
					continue
				}
				out, err := checkin.New(acc).Run(ctx, tok.Access, a.UserID)
				if err != nil {
					fmt.Printf("%s\t失败：%v\n", a.Name, err)
					failed = true
					continue
				}
				fmt.Printf("%s\t%s\t积分 %g\t连续 %d 天\n", a.Name, out.Outcome, out.Credit, out.Streak)
				day := time.Now().In(time.FixedZone("CST", 8*3600)).Format("2006-01-02")
				if err := e.store.SaveCheckin(ctx, store.Checkin{
					AccountID: a.ID, Day: day, Outcome: out.Outcome,
					Credit: out.Credit, Streak: out.Streak, Message: out.Message,
					CreatedAt: time.Now(),
				}); err != nil {
					return err
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
