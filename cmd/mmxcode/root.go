// This file holds the mmxcode command line.
//
// Everything in this directory is package main: the directory is the
// binary, and a directory holds one package.
package main

import (
	"context"
	"fmt"
	"os"
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
	root, _ := newRoot()
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	return 0
}

// APIKeysEnv is the environment variable that holds the api keys.
//
// 镜像里不带密钥：密钥是申请来的，写进镜像就等于把它摊在别人眼前。
// 它从环境变量进来，配置文件里可以一把都没有。
const APIKeysEnv = "MMXCODE_API_KEYS"

// apiKeysFromEnv gives the api keys the environment brings.
//
// 逗号分隔。一把空的 key 等于没有 key：它会被去掉。
func apiKeysFromEnv(v string) []string {
	var out []string
	for _, k := range strings.Split(v, ",") {
		if k = strings.TrimSpace(k); k != "" {
			out = append(out, k)
		}
	}
	return out
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
	makeRunner func(interval time.Duration, keeper func(string) (string, error)) *provision.Runner
}

func newRoot() (*cobra.Command, *env) {
	var (
		configPath string
		dbPath     string
	)
	root := &cobra.Command{
		Use:          "mmxcode",
		Short:        "mmxcode 的网关版本",
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
		// 密钥也可以从环境变量来：镜像里不带密钥，密钥是申请来的。
		if v := os.Getenv(APIKeysEnv); v != "" {
			cfg.Server.APIKeys = append(cfg.Server.APIKeys, apiKeysFromEnv(v)...)
		}
		e.cfg = cfg
		// 回包能有多大：整个进程一个数（读额度、模型名单、签到、续签、聊
		// 天的回答读的都是它）。开机定一次。
		upstream.SetMaxBody(int64(cfg.Upstream.MaxBodyMB) << 20)
		s, err := store.Open(cfg.DB,
			store.WithCacheMB(cfg.SQLite.CacheMB),
			store.WithSynchronous(cfg.SQLite.Synchronous))
		if err != nil {
			return err
		}
		if err := s.Migrate(ctx); err != nil {
			return err
		}
		e.store = s
		e.egr = egress.New(cfg.Proxy.Default)
		// 组：库是老家，配置里的组只是种子。库里没有的组用配置里的地址
		// 建出来 —— 老库里那些指着组名的号，靠这一步还指着原来的那条路。
		res, err := e.store.SeedProxyGroups(ctx, cfg.Proxy.Default, cfg.Proxy.Groups)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: the outlet groups could not be made: %v\n", err)
		} else {
			if res.GlobalSeeded {
				fmt.Fprintf(os.Stderr, "warning: the global group was made from proxy.default in the configuration. The configuration's proxy.default does not count any more - change the global group on the page.\n")
			}
			if res.GlobalShadowed {
				fmt.Fprintf(os.Stderr, "warning: proxy.default in the configuration does not count any more: the global group is in the database. Change the global group on the page.\n")
			}
			for _, n := range res.Created {
				fmt.Fprintf(os.Stderr, "the outlet group %q was made from the configuration.\n", n)
			}
			for _, n := range res.Shadowed {
				fmt.Fprintf(os.Stderr, "warning: the outlet group %q in the configuration is already in the database. The one in the database counts. That part can be taken out of the configuration file.\n", n)
			}
			if res.UnknownAccounts > 0 {
				fmt.Fprintf(os.Stderr, "warning: %d account(s) point at an outlet group that is not in the database (%s). They follow the global group for now.\n",
					res.UnknownAccounts, strings.Join(res.Unknown, ", "))
			}
		}
		// 解析器得知道组：登录、导入、网关都按组挑地址。
		groups, err := e.store.ListProxyGroups(ctx)
		if err != nil {
			return err
		}
		e.egr.SetGroups(egress.GroupMap(groups))
		// 登录和账户 API 也走代理。配了不用等于没配：国内的网络
		// 连不上国际站，登录就停在第一步。
		tr, err := e.egr.Transport(e.egr.Resolve(store.Account{}))
		if err != nil {
			return err
		}
		e.up = upstream.New(upstream.Options{Timeout: 30 * time.Second, Transport: tr})
		e.acc = account.New(e.up)
		e.mgr = manager.New(s, e.up, e.acc, e.egr, oauth.NewRenewer())
		// 旧版本写在账号上的记号擦掉：那句话说的事今天已经不存在了，挂
		// 着它是骗人的（页面上显示着「这个号有问题」，而那个号其实得好
		// 得很）。擦不掉不是说一声就好 —— 网关照样起：那一次收拾，不
		// 是网关的命。
		if err := e.mgr.ClearRetiredNotes(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "warning: the notes of an older version could not be cleared: %v\n", err)
		}
		// 组和它们的健康灌进解析器：网关下一次挑地址就该按库里那份来。
		if err := e.mgr.RefreshOutlets(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "warning: the outlet groups could not be read: %v\n", err)
		}
		// 重登用的密钥：没有它，账号掉线了就只能自己再登一次。
		if key, err := secret.FromEnv(PasswordKeyEnv); err == nil {
			e.mgr.Key = key
		} else if strings.TrimSpace(os.Getenv(PasswordKeyEnv)) != "" {
			// 密钥设了但是不对：说一声，别让人以为密码存下了。
			fmt.Fprintf(os.Stderr, "warning: %s is set but unusable: %v\n", PasswordKeyEnv, err)
		}
		e.mgr.ReloginFn = e.reloginFn()
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
	root.AddCommand(newGroupsCmd(e))
	root.AddCommand(newCheckinCmd(e))
	root.AddCommand(newUsageCmd(e))
	root.AddCommand(newModelsCmd(e))
	root.AddCommand(newServeCmd(e))
	return root, e
}

func nowUTC() time.Time { return time.Now().UTC() }
