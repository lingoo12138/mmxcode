package main

import (
	"context"
	"strings"
	"testing"
)

// TestTheGroupIsMadeAndChanged says a group can be made and changed from
// the command line.
func TestTheGroupIsMadeAndChanged(t *testing.T) {
	e := testEnv(t)
	ctx := context.Background()
	cmd := newGroupsSetCmd(e)
	cmd.SetArgs([]string{"--name", "clash", "--url", "http://a:8080", "--url", "http://b:8080"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("set the group: %v", err)
	}
	g, err := e.store.GetProxyGroup(ctx, "clash")
	if err != nil {
		t.Fatalf("get the group: %v", err)
	}
	if len(g.URLs) != 2 || g.URLs[0] != "http://a:8080" || g.URLs[1] != "http://b:8080" {
		t.Errorf("the group's addresses are %v", g.URLs)
	}
}

// TestTheGroupIsListed says the list shows the groups and their
// addresses, without the credentials.
//
// 地址里的账号和密码是秘密，而这份输出会进到终端的回滚里，也会进到 CI
// 的日志里。
func TestTheGroupIsListed(t *testing.T) {
	e := testEnv(t)
	cmd := newGroupsSetCmd(e)
	cmd.SetArgs([]string{"--name", "clash", "--url", "socks5h://bob:hunter2@127.0.0.1:1080"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("set the group: %v", err)
	}
	out := captureStdout(t, func() {
		cmd := newGroupsListCmd(e)
		cmd.SetOut(nil)
		cmd.SetErr(nil)
		if err := cmd.Execute(); err != nil {
			t.Errorf("groups list: %v", err)
		}
	})
	if strings.Contains(out, "hunter2") || strings.Contains(out, "bob") {
		t.Errorf("the proxy's password is in the output:\n%s", out)
	}
	if !strings.Contains(out, "clash") {
		t.Errorf("the group is not in the output:\n%s", out)
	}
}
