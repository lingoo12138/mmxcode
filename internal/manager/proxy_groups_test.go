package manager

import (
	"context"
	"testing"

	"mmxcode/internal/store"
)

// 出口组退役了：开机的时候收拾一次 —— 组名变成账号自己的地址。
func TestTheOutletGroupsBecomeAddresses(t *testing.T) {
	m, path := newTestManagerWithPath(t, nil)
	ctx := context.Background()
	for _, a := range []struct {
		id    string
		group string
		url   string
	}{
		{"acc_01", "clash", ""},
		{"acc_02", "", "http://10.0.0.5:1080"},
		{"acc_03", "clash", "http://10.0.0.9:1080"},
		{"acc_04", "nope", ""},
	} {
		if err := m.Store.CreateAccount(ctx, store.Account{
			ID: a.id, Name: a.id, Email: a.id + "@example.com",
			Status: store.StatusActive, ProxyURL: a.url,
		}); err != nil {
			t.Fatal(err)
		}
		if a.group != "" {
			if _, err := rawDB(t, path).Exec(
				`UPDATE accounts SET proxy_group=? WHERE id=?`, a.group, a.id); err != nil {
				t.Fatal(err)
			}
		}
	}
	res, err := m.MaterializeProxyGroups(ctx, map[string]string{
		"clash": "http://127.0.0.1:7890",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Moved != 1 {
		t.Errorf("moved = %d, want 1", res.Moved)
	}
	if res.Kept != 1 {
		t.Errorf("kept = %d, want 1", res.Kept)
	}
	if res.Dropped != 1 || len(res.DroppedIDs) != 1 || res.DroppedIDs[0] != "acc_04" {
		t.Errorf("dropped = %d %v, want 1 [acc_04]", res.Dropped, res.DroppedIDs)
	}
	// 组里的地址成了账号自己的地址。
	got, err := m.Store.GetAccount(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if got.ProxyURL != "http://127.0.0.1:7890" {
		t.Errorf("acc_01's address = %q", got.ProxyURL)
	}
	// 自己有地址的账号，地址不动。
	got, err = m.Store.GetAccount(ctx, "acc_03")
	if err != nil {
		t.Fatal(err)
	}
	if got.ProxyURL != "http://10.0.0.9:1080" {
		t.Errorf("acc_03's address = %q", got.ProxyURL)
	}
	// 组名对不上的账号：地址空了，它回到全局默认。
	got, err = m.Store.GetAccount(ctx, "acc_04")
	if err != nil {
		t.Fatal(err)
	}
	if got.ProxyURL != "" {
		t.Errorf("acc_04's address = %q, want empty", got.ProxyURL)
	}
	// 收拾过就没有组名了：再收拾一次什么也不做。
	list, err := m.Store.LegacyProxyGroups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("list = %+v, want none", list)
	}
}
