// This file holds the mmxcode command line.
//
// Everything in this directory is package main: the directory is the
// binary, and a directory holds one package.
package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"mmxcode/internal/account"
	"mmxcode/internal/config"
	"mmxcode/internal/egress"
	"mmxcode/internal/manager"
	"mmxcode/internal/oauth"
	"mmxcode/internal/provision"
	"mmxcode/internal/secret"
	"mmxcode/internal/store"
	"mmxcode/internal/upstream"
)

// Execute runs the command line and gives the process its exit code.
func Execute() int {
	if err := newRoot().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	return 0
}

// env is what the commands share.
type env struct {
	cfg   config.Config
	store *store.Store
	up    *upstream.Client
	egr   *egress.Resolver
	acc   *account.Client
	mgr   *manager.Manager

	// makeRunner makes the batch driver. It is a field so that a test
	// can run a batch without a network.
	makeRunner func(group string, interval time.Duration, keeper func(string) (string, error)) *provision.Runner
}

func newRoot() *cobra.Command {
	var (
		configPath string
		dbPath     string
	)
	root := &cobra.Command{
		Use:          "mmxcode",
		Short:        "MiniMax Code 多账号网关",
		SilenceUsage: true,
	}
	root.PersistentFlags().StringVar(&configPath, "config", os.Getenv("MMXCODE_CONFIG"), "配置文件路径")
	root.PersistentFlags().StringVar(&dbPath, "db", "", "数据库路径（覆盖配置）")

	e := &env{}
	ctx := context.Background()

	root.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load(configPath)
		if err != nil {
			return err
		}
		if dbPath != "" {
			cfg.DB = dbPath
		}
		if v := os.Getenv("MMXCODE_PROXY_DEFAULT"); v != "" {
			cfg.Proxy.Default = v
		}
		e.cfg = cfg
		s, err := store.Open(cfg.DB)
		if err != nil {
			return err
		}
		if err := s.Migrate(ctx); err != nil {
			return err
		}
		e.store = s
		e.egr = egress.New(cfg.Proxy.Groups, cfg.Proxy.Default)
		// 登录和账户 API 也走代理。配了不用等于没配：国内的网络
		// 连不上国际站，登录就停在第一步。
		tr, err := e.egr.Transport(e.egr.Resolve(store.Account{}))
		if err != nil {
			return err
		}
		e.up = upstream.New(upstream.Options{Timeout: 30 * time.Second, Transport: tr})
		e.acc = account.New(e.up)
		e.mgr = manager.New(s, e.up, e.acc, e.egr, oauth.NewRenewer())
		// 重登用的密钥：没有它，账号掉线了就只能自己再登一次。
		if key, err := secret.FromEnv(PasswordKeyEnv); err == nil {
			e.mgr.Key = key
		} else if strings.TrimSpace(os.Getenv(PasswordKeyEnv)) != "" {
			// 密钥设了但是不对：说一声，别让人以为密码存下了。
			fmt.Fprintf(os.Stderr, "warning: %s is set but unusable: %v\n", PasswordKeyEnv, err)
		}
		e.mgr.ReloginFn = e.reloginFn("")
		return nil
	}
	root.PersistentPostRun = func(cmd *cobra.Command, args []string) {
		if e.store != nil {
			e.store.Close()
		}
	}

	root.AddCommand(newLoginCmd(e))
	root.AddCommand(newImportCmd(e))
	root.AddCommand(newReloginCmd(e))
	root.AddCommand(newAccountsCmd(e))
	root.AddCommand(newCheckinCmd(e))
	root.AddCommand(newUsageCmd(e))
	root.AddCommand(newModelsCmd(e))
	root.AddCommand(newServeCmd(e))
	return root
}

func nowUTC() time.Time { return time.Now().UTC() }

// groupNames lists the outlet groups, for an error message.
func groupNames(groups map[string]string) string {
	names := make([]string, 0, len(groups))
	for n := range groups {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
