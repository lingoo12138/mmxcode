package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"mmxcode/internal/account"
	"mmxcode/internal/config"
	"mmxcode/internal/egress"
	"mmxcode/internal/manager"
	"mmxcode/internal/oauth"
	"mmxcode/internal/store"
	"mmxcode/internal/upstream"
)

// TestIsLoopback says which listen addresses stay on this machine.
//
// ":8080" and "0.0.0.0:8080" mean every interface of the machine, so
// they are not loopback: with no api key they would hand the models to
// whoever can reach the machine.
func TestIsLoopback(t *testing.T) {
	cases := []struct {
		listen string
		want   bool
	}{
		{"127.0.0.1:8080", true},
		{"127.0.0.5:8080", true},
		{"localhost:8080", true},
		{"[::1]:8080", true},
		{":8080", false},
		{"0.0.0.0:8080", false},
		{"[::]:8080", false},
		{"192.168.1.5:8080", false},
	}
	for _, c := range cases {
		if got := isLoopback(c.listen); got != c.want {
			t.Errorf("isLoopback(%q) = %v, want %v", c.listen, got, c.want)
		}
	}
}

// serveCmdFor gives a serve command that says nothing but its error.
func serveCmdFor(t *testing.T, cfg config.Config) *cobra.Command {
	t.Helper()
	cmd := newServeCmd(&env{cfg: cfg})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	return cmd
}

// TestServeRefusesWithoutAnAPIKeyOnAWideAddress says the gateway does
// not come up for everyone when nobody has to give a key.
func TestServeRefusesWithoutAnAPIKeyOnAWideAddress(t *testing.T) {
	for _, listen := range []string{"0.0.0.0:8080", ":8080", "192.168.1.5:8080"} {
		cfg := config.Default()
		cfg.Server.Listen = listen
		cfg.Server.APIKeys = nil
		err := serveCmdFor(t, cfg).Execute()
		if err == nil {
			t.Fatalf("serve started on %s with no api key", listen)
		}
		if !strings.Contains(err.Error(), "no api key is configured") {
			t.Errorf("the error for %s does not say the key is missing: %v", listen, err)
		}
	}
}

// TestServeRefusesANonGlobalSite says the Chinese site is refused
// rather than served with the wrong token.
func TestServeRefusesANonGlobalSite(t *testing.T) {
	cfg := config.Default()
	cfg.Upstream.Site = "cn"
	err := serveCmdFor(t, cfg).Execute()
	if err == nil {
		t.Fatal("serve started with upstream.site = cn")
	}
	if !strings.Contains(err.Error(), "only \"global\" is supported") {
		t.Errorf("the error does not name the site: %v", err)
	}
}

// TestServeRefusesABadCheckinTime says a check-in time that cannot be
// read is refused instead of quietly run at the default time.
func TestServeRefusesABadCheckinTime(t *testing.T) {
	cfg := config.Default()
	cfg.Schedule.CheckinAt = "nine o'clock"
	err := serveCmdFor(t, cfg).Execute()
	if err == nil {
		t.Fatal("serve started with a check-in time that cannot be read")
	}
	if !strings.Contains(err.Error(), "want HH:MM") {
		t.Errorf("the error does not say the format: %v", err)
	}
}

// TestServeStartsOnLoopbackWithoutAnAPIKey says a loopback address
// with no api key is fine: only this machine can reach it.
//
// The port is taken first, so the server cannot come up and the run
// ends at once. What matters here is which error comes back: not the
// one about the missing key.
func TestServeStartsOnLoopbackWithoutAnAPIKey(t *testing.T) {
	err := serveUntilThePortIsTaken(t, "127.0.0.1", nil)
	if err == nil {
		t.Fatal("serve came up on a port that was already taken")
	}
	if strings.Contains(err.Error(), "no api key is configured") {
		t.Errorf("serve refused a loopback address with no api key: %v", err)
	}
}

// TestServeAllowsAWideAddressWithAnAPIKey says a wide address is fine
// when a key is configured: the key is what keeps the gateway closed.
func TestServeAllowsAWideAddressWithAnAPIKey(t *testing.T) {
	err := serveUntilThePortIsTaken(t, "0.0.0.0", []string{"sk-local-test"})
	if err == nil {
		t.Fatal("serve came up on a port that was already taken")
	}
	if strings.Contains(err.Error(), "no api key is configured") {
		t.Errorf("serve refused a wide address that has an api key: %v", err)
	}
}

// serveUntilThePortIsTaken runs serve on an address whose port is
// already taken, so the run ends at once, and gives back the error.
func serveUntilThePortIsTaken(t *testing.T, host string, keys []string) error {
	t.Helper()
	ln, err := net.Listen("tcp", host+":0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	addr := ln.Addr().String()

	cfg := config.Default()
	cfg.Server.Listen = addr
	cfg.Server.APIKeys = keys

	cmd := newServeCmd(serveEnv(t, cfg))
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	return cmd.Execute()
}

// serveEnv gives an environment with a store and a manager, the way
// the root command builds them.
func serveEnv(t *testing.T, cfg config.Config) *env {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mmxcode.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	egr := egress.New(cfg.Proxy.Groups, cfg.Proxy.Default)
	tr, err := egr.Transport(egr.Resolve(store.Account{}))
	if err != nil {
		t.Fatalf("transport: %v", err)
	}
	up := upstream.New(upstream.Options{Timeout: 30 * time.Second, Transport: tr})
	acc := account.New(up)
	return &env{
		cfg:   cfg,
		store: s,
		up:    up,
		egr:   egr,
		acc:   acc,
		mgr:   manager.New(s, up, acc, egr, oauth.NewRenewer()),
	}
}

func TestTheWebPathSaysThePasswordsAreNotKept(t *testing.T) {
	// 没有密钥的时候，页面上得说密码没有存下来：不然人以为账号以后能
	// 自己登回去。
	t.Setenv(PasswordKeyEnv, "")
	keeper, err := passwordKeeper(false)
	if err != nil {
		t.Fatal(err)
	}
	note := passwordNote(keeper, err)
	if note == "" {
		t.Fatal("no note about the passwords")
	}
	if !strings.Contains(note, PasswordKeyEnv) {
		t.Fatalf("note = %q, want it to name the key", note)
	}
}

func TestTheWebPathSaysNothingWhenThePasswordsAreKept(t *testing.T) {
	t.Setenv(PasswordKeyEnv, base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)))
	keeper, err := passwordKeeper(false)
	if err != nil {
		t.Fatal(err)
	}
	if note := passwordNote(keeper, err); note != "" {
		t.Fatalf("note = %q, want none", note)
	}
}
