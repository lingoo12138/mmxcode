package main

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

func newReloginCmd(e *env) *cobra.Command {
	var all bool
	var accountID string
	cmd := &cobra.Command{
		Use:   "relogin",
		Short: "把签出的账号登回去",
		Long: "用存着的密码，把签出（signed_out）的账号重新登一遍。\n" +
			"没有存密码的账号登不回去——那个键没有就是没有。\n" +
			"同一个账号不会同时登两遍，不过这只在一个进程里算数：这个命令跟\n" +
			"一个正在跑着的 mmxcode serve 用同一个库的时候，两个进程可能同时登\n" +
			"同一个账号。要是不想撞上，就别一边开着 serve 一边跑这一条。",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRelogin(cmd.Context(), e, cmd.OutOrStdout(), accountID, all)
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "重登所有签出且有密码的账号")
	cmd.Flags().StringVar(&accountID, "account", "", "只重登这一个账号")
	return cmd
}

// runRelogin signs the signed-out accounts in again.
//
// Either one account or all of them: "all of them" without saying so
// is a surprise waiting to happen, and a named account is not a
// surprise at all.
func runRelogin(ctx context.Context, e *env, out io.Writer, accountID string, all bool) error {
	if accountID == "" && !all {
		return fmt.Errorf("要么说 --all，要么给一个 --account")
	}
	if accountID != "" {
		if err := e.mgr.Relogin(ctx, accountID); err != nil {
			return err
		}
		fmt.Fprintf(out, "重登完成：%s\n", accountID)
		return nil
	}
	res, err := e.mgr.ReloginAll(ctx, 0)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "重登完成：成功 %d，失败 %d。\n", res.Fixed, res.Failed)
	for _, r := range res.Reasons {
		fmt.Fprintf(out, "  %s\n", r)
	}
	if res.Failed > 0 {
		return fmt.Errorf("有 %d 个账号没登回去", res.Failed)
	}
	return nil
}
