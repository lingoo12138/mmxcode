package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"mmxcode/internal/gateway"
	"mmxcode/internal/manager"
	"mmxcode/internal/provision"
	"mmxcode/internal/scheduler"
	"mmxcode/internal/webui"
)

func newServeCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "起 HTTP 服务、WebUI 与定时任务",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			// 只有国际站。中国站是另一套主机，token 不通用。
			if s := e.cfg.Upstream.Site; s != "" && s != "global" {
				return fmt.Errorf("upstream.site = %q; only \"global\" is supported", s)
			}

			// 签到时间写错的话，调度器会默默按 09:00 跑。用户以为自己
			// 配了别的点，就永远等不到那个点的签到。起不来比悄悄跑错好。
			if _, err := time.Parse("15:04", e.cfg.Schedule.CheckinAt); err != nil {
				return fmt.Errorf("schedule.checkin_at = %q; want HH:MM", e.cfg.Schedule.CheckinAt)
			}

			// 没配 key 时只听本机，不然就是把模型白送出去
			listen := e.cfg.Server.Listen
			if len(e.cfg.Server.APIKeys) == 0 && !isLoopback(listen) {
				return fmt.Errorf("no api key is configured, so the gateway would listen on %s for everyone; put api_keys in the config or listen on 127.0.0.1", listen)
			}

			ui := webui.New(e.mgr, e.cfg, e.store)
			e.wireImport(ui)
			gw := gateway.New(e.mgr, e.cfg, e.store, ui)
			sc := scheduler.New(e.mgr, e.cfg)

			srv := &http.Server{
				Addr:              listen,
				Handler:           gw,
				ReadHeaderTimeout: 10 * time.Second,
				// 聊天会流很久，不给写超时
			}

			go sc.Run(ctx)

			errc := make(chan error, 1)
			go func() {
				fmt.Printf("网关在 %s 上\n", listen)
				errc <- srv.ListenAndServe()
			}()

			sig := make(chan os.Signal, 1)
			signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
			select {
			case err := <-errc:
				if err != nil && err != http.ErrServerClosed {
					return err
				}
			case <-sig:
				fmt.Println("正在关掉……")
			}
			cancel()
			shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer shutCancel()
			return srv.Shutdown(shutCtx)
		},
	}
}

// wireImport puts the batch import and the re-login on the page.
//
// 页面上传和命令行导入走的是同一件事：批量入号和重登。
//
// 密钥有问题的时候导入要停下来：悄悄把密码丢掉，会留下一库谁也登不
// 回去的账号。
//
// 没有密钥，密码就不入库。页面上得说清楚：账号掉线以后没有人能自己登
// 回去，人得知道这件事。
//
// 这一段单独拿出来，是为了能测它：那句提醒是接线上的，不是模板里的。
func (e *env) wireImport(ui *webui.WebUI) {
	keeper, keeperErr := passwordKeeper(false)
	ui.PasswordNote = passwordNote(keeper, keeperErr)
	ui.Import = func(ctx context.Context, entries []provision.Entry) ([]provision.Result, error) {
		if keeperErr != nil {
			return nil, keeperErr
		}
		runner, err := e.runner(e.cfg.Batch.Group, e.cfg.Batch.Interval.Duration(), keeper)
		if err != nil {
			return nil, err
		}
		return runner.Run(ctx, entries)
	}
	ui.Relogin = func(ctx context.Context) (manager.ReloginResult, error) {
		return e.mgr.ReloginAll(ctx, e.cfg.Batch.Relogin.MaxAccounts)
	}
}

// passwordNote says what happens to the passwords, when they are not
// kept.
//
// 没有密钥的时候密码就不入库，页面上得说清楚：账号掉线以后没有人能
// 自己登回去。密钥有问题的时候导入自己就停了，页面不用多说。
func passwordNote(keeper func(string) (string, error), keeperErr error) string {
	if keeperErr != nil || keeper != nil {
		return ""
	}
	return "密码没有存进库里（没有 " + PasswordKeyEnv + "）：账号掉线以后没有人能自己登回去。"
}

// isLoopback says a listen address is only on this machine.
//
// ":8080" and "0.0.0.0:8080" are not: they mean every interface of the
// machine, so anyone who can reach it can use the accounts.
func isLoopback(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		host = listen
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
