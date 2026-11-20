package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mmxcode/internal/config"
	"mmxcode/internal/egress"
	"mmxcode/internal/provision"
	"mmxcode/internal/store"
)

// aFakeEnv is an env whose batch does not touch the network.
func aFakeEnv(t *testing.T) *env {
	t.Helper()
	e := &env{}
	e.makeRunner = func(interval time.Duration, keeper func(string) (string, error)) *provision.Runner {
		return &provision.Runner{
			Importer: &provision.Importer{
				Import: func(ctx context.Context, email, password string) (provision.Result, error) {
					if password != "pw" {
						return provision.Result{Email: email}, &provision.Error{
							Stage: provision.StagePassword, Code: 1200046, Message: "密码不对",
						}
					}
					return provision.Result{Email: email, OK: true, AccountID: "acc_1"}, nil
				},
			},
			Interval: interval,
			Sleep:    func(context.Context, time.Duration) error { return nil },
		}
	}
	return e
}

// TestTheImportReadsTheCreditsOfTheNewAccount says an account that
// just came in has its credits read.
//
// 导入完了要等上一趟扫描（usage.refresh.scan_every，默认 30 秒）才看
// 得到积分，是不让人话的：刚进来的账号马上就读一次账本。
func TestTheImportReadsTheCreditsOfTheNewAccount(t *testing.T) {
	var reads atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/minimax-cloud/api/v1/credit/details":
			reads.Add(1)
			w.Write([]byte(`{"total_count":1,"details":[{"credit_type":1,"granted_amount":"600","consumed_amount":"0","remaining_amount":"600","granted_at_ms":1763000000000,"expire_at_ms":1900000000000}]}`))
		case "/backend/account/token_plan/remains_percent":
			w.Write([]byte(`{"base_resp":{"status_code":0},"model_remains":[]}`))
		default:
			w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()
	e := testEnvWithServer(t, srv)
	seedAccount(t, e, time.Now().Add(time.Hour))
	e.makeRunner = func(interval time.Duration, keeper func(string) (string, error)) *provision.Runner {
		return &provision.Runner{
			Importer: &provision.Importer{
				Import: func(ctx context.Context, email, password string) (provision.Result, error) {
					return provision.Result{Email: email, OK: true, AccountID: "acc_1"}, nil
				},
			},
			Interval: interval,
			Sleep:    func(context.Context, time.Duration) error { return nil },
		}
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.txt")
	if err := os.WriteFile(path, []byte("a@example.com|pw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runImport(context.Background(), e, &out, path, time.Millisecond, false); err != nil {
		t.Fatal(err)
	}
	if reads.Load() == 0 {
		t.Fatalf("the credits of the new account were not read:\n%s", out.String())
	}
}

func TestImportReadsTheFileAndSaysWhatHappened(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.txt")
	if err := os.WriteFile(path, []byte("a@example.com|pw\nb@example.com|pw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runImport(context.Background(), aFakeEnv(t), &out, path, time.Millisecond, false); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.Contains(text, "a@example.com") || !strings.Contains(text, "b@example.com") {
		t.Fatalf("out = %q", text)
	}
	if !strings.Contains(text, "成功 2") {
		t.Fatalf("out = %q", text)
	}
}

func TestImportSaysALineItCouldNotRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.txt")
	// 清单里得有一个能用的号：光有坏行的话，整趟导入是空跑。
	if err := os.WriteFile(path, []byte("not-an-email\nb@example.com|pw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runImport(context.Background(), aFakeEnv(t), &out, path, time.Millisecond, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "第 1 行") {
		t.Fatalf("out = %q", out.String())
	}
}

func TestImportSaysWhyAnAccountDidNotMakeIt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.txt")
	if err := os.WriteFile(path, []byte("a@example.com|wrong\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runImport(context.Background(), aFakeEnv(t), &out, path, time.Millisecond, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "密码") {
		t.Fatalf("out = %q", out.String())
	}
}

func TestImportStopsAtItsTimeout(t *testing.T) {
	// 整趟导入有个时限：清单很大的时候，人不想等到天亮才知道它卡住了。
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.txt")
	if err := os.WriteFile(path, []byte("a@example.com|pw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := aFakeEnv(t)
	e.makeRunner = func(interval time.Duration, keeper func(string) (string, error)) *provision.Runner {
		return &provision.Runner{
			Importer: &provision.Importer{
				Import: func(ctx context.Context, email, password string) (provision.Result, error) {
					// 站点那边没有回答：这一趟就卡在这里。
					<-ctx.Done()
					return provision.Result{Email: email}, ctx.Err()
				},
			},
			Interval: interval,
			Sleep:    func(context.Context, time.Duration) error { return nil },
		}
	}
	cmd := newImportCmd(e)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--file", path, "--timeout", "100ms"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("the import ran past its timeout")
	}
}

func TestAnImportThatIsCutShortKeepsWhatItGot(t *testing.T) {
	// 时限到了的时候，已经进来的账号不能白跑：记录要收尾，页面上这趟
	// 导入不能永远显示成「还在跑」。
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.txt")
	if err := os.WriteFile(path, []byte("a@example.com|pw\nb@example.com|pw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := testEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.makeRunner = func(interval time.Duration, keeper func(string) (string, error)) *provision.Runner {
		return &provision.Runner{
			Importer: &provision.Importer{
				Import: func(ctx context.Context, email, password string) (provision.Result, error) {
					if email == "a@example.com" {
						return provision.Result{Email: email, OK: true, AccountID: "acc_1"}, nil
					}
					// 时限到了：这一趟就掐在这里。
					cancel()
					return provision.Result{Email: email}, ctx.Err()
				},
			},
			Interval: interval,
			Sleep:    func(context.Context, time.Duration) error { return nil },
		}
	}
	var out bytes.Buffer
	if err := runImport(ctx, e, &out, path, time.Millisecond, false); err == nil {
		t.Fatal("the import ran past its deadline")
	}
	// 已经进来的那个账号要说出来，也要说清楚这趟是被掐断的。
	if !strings.Contains(out.String(), "a@example.com") {
		t.Fatalf("out = %q", out.String())
	}
	if !strings.Contains(out.String(), "截断") {
		t.Fatalf("out = %q, want it to say the run was cut short", out.String())
	}
	runs, err := e.store.ListImportRuns(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("got %d runs", len(runs))
	}
	if runs[0].FinishedAt.IsZero() {
		t.Fatal("the run was never finished")
	}
	if runs[0].OK != 1 {
		t.Fatalf("ok = %d, want the one that got in", runs[0].OK)
	}
	if !strings.Contains(runs[0].Results, "a@example.com") {
		t.Fatalf("results = %q", runs[0].Results)
	}
}

// 导入走全局默认出口：新账号身上没有自己的地址，它跟着全局默认走。
func TestTheImportGoesOutOfTheGlobalDefault(t *testing.T) {
	e := &env{
		cfg: config.Default(),
		egr: egress.New("direct"),
	}
	c := e.chain(e.egr.Resolve(store.Account{}), nil)
	if c == nil {
		t.Fatal("the chain is not there")
	}
}
