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
	"mmxcode/internal/oauth"
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
		return nil
	}
	root.PersistentPostRun = func(cmd *cobra.Command, args []string) {
		if e.store != nil {
			e.store.Close()
		}
	}

	root.AddCommand(newLoginCmd(e))
	root.AddCommand(newAccountsCmd(e))
	root.AddCommand(newCheckinCmd(e))
	root.AddCommand(newUsageCmd(e))
	root.AddCommand(newModelsCmd(e))
	return root
}

// accFor gives the account API client an account's calls go out with.
//
// Every fetch an account makes goes through the account's own way out,
// or the global one. The transport is cached by the resolver, so making
// a client for it is cheap.
func (e *env) accFor(a store.Account) (*account.Client, error) {
	tr, err := e.egr.Transport(e.egr.Resolve(a))
	if err != nil {
		return nil, err
	}
	return e.acc.With(upstream.New(upstream.Options{Timeout: 30 * time.Second, Transport: tr})), nil
}

// tokenFor gives an account's token, or says it is not signed in.
func (e *env) tokenFor(ctx context.Context, accountID string) (*oauth.Token, error) {
	t, err := e.store.GetToken(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("account %s is not signed in", accountID)
	}
	return &oauth.Token{
		Access:  t.Access,
		Refresh: t.Refresh,
		Expires: t.ExpiresAt,
		Subject: t.Subject,
	}, nil
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
