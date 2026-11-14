package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mmxcode/internal/provision"
)

// aFakeEnv is an env whose batch does not touch the network.
func aFakeEnv(t *testing.T) *env {
	t.Helper()
	e := &env{}
	e.makeRunner = func(group string, interval time.Duration, keeper func(string) (string, error)) *provision.Runner {
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

func TestImportReadsTheFileAndSaysWhatHappened(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.txt")
	if err := os.WriteFile(path, []byte("a@example.com|pw\nb@example.com|pw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runImport(context.Background(), aFakeEnv(t), &out, path, "", time.Millisecond, false); err != nil {
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
	if err := runImport(context.Background(), aFakeEnv(t), &out, path, "", time.Millisecond, false); err != nil {
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
	if err := runImport(context.Background(), aFakeEnv(t), &out, path, "", time.Millisecond, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "密码") {
		t.Fatalf("out = %q", out.String())
	}
}
