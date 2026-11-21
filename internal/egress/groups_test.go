package egress

import (
	"net"
	"testing"

	"github.com/lingoo12138/mmxcode/internal/store"
)

// TestTheCandidatesFollowTheChain says the candidates are the account's
// own address, then its group, then the global group.
//
// 出口是四级的：账号自己的地址 → 账号指着的组 → 全局组 → 直连。
func TestTheCandidatesFollowTheChain(t *testing.T) {
	r := New("http://global:8080")
	r.SetGroups(map[string][]string{
		store.GlobalGroup: {"http://global:8080"},
		"clash":           {"http://clash:8080", "http://clash2:8080"},
	})
	got := r.Candidates(store.Account{ProxyURL: "http://own:8080", ProxyGroup: "clash"})
	want := []string{"http://own:8080", "http://clash:8080", "http://clash2:8080", "http://global:8080"}
	if len(got) != len(want) {
		t.Fatalf("the candidates are %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("candidate %d is %q, not %q", i, got[i], want[i])
		}
	}
}

// TestTheAccountWithoutAGroupFollowsTheGlobalOne says an account with
// nothing of its own follows the global group.
func TestTheAccountWithoutAGroupFollowsTheGlobalOne(t *testing.T) {
	r := New("")
	r.SetGroups(map[string][]string{
		store.GlobalGroup: {"http://global:8080", "http://global2:8080"},
	})
	got := r.Candidates(store.Account{})
	if len(got) != 2 || got[0] != "http://global:8080" {
		t.Errorf("the candidates are %v", got)
	}
}

// TestTheAccountWithNothingAtAllIsDirect says there is no candidate at
// all when nothing is configured.
//
// 直连是最后一级，不是错。
func TestTheAccountWithNothingAtAllIsDirect(t *testing.T) {
	r := New("")
	r.SetGroups(map[string][]string{})
	if got := r.Resolve(store.Account{}); got != Direct {
		t.Errorf("the way out is %q", got)
	}
}

// TestTheAddressThatAnsweredComesFirst says the addresses that answered
// the probe come first, then the ones that were never probed, then the
// ones that did not answer.
//
// 组里的顺序是主备：探通过的先走，没探过的其次（探的结果可能是旧的），
// 探失败的最后。
func TestTheAddressThatAnsweredComesFirst(t *testing.T) {
	r := New("")
	r.SetGroups(map[string][]string{
		store.GlobalGroup: {"http://dead:8080", "http://alive:8080", "http://unknown:8080"},
	})
	r.SetHealth("http://dead:8080", false)
	r.SetHealth("http://alive:8080", true)
	got := r.Candidates(store.Account{})
	want := []string{"http://alive:8080", "http://unknown:8080", "http://dead:8080"}
	if len(got) != len(want) {
		t.Fatalf("the candidates are %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("candidate %d is %q, not %q", i, got[i], want[i])
		}
	}
}

// TestTheGroupThatIsNotThereIsSkipped says a group that is not in the
// database is skipped: the account follows the global one.
//
// 组名留着（人等下把组建出来，这个号就自己回去了），出口往下走。
func TestTheGroupThatIsNotThereIsSkipped(t *testing.T) {
	r := New("")
	r.SetGroups(map[string][]string{
		store.GlobalGroup: {"http://global:8080"},
	})
	got := r.Candidates(store.Account{ProxyGroup: "nope"})
	if len(got) != 1 || got[0] != "http://global:8080" {
		t.Errorf("the candidates are %v", got)
	}
}

// TestTheSameAddressIsOnlyOnce says the same address is not asked for
// twice.
func TestTheSameAddressIsOnlyOnce(t *testing.T) {
	r := New("")
	r.SetGroups(map[string][]string{
		store.GlobalGroup: {"http://global:8080"},
		"clash":           {"http://global:8080", "http://clash:8080"},
	})
	got := r.Candidates(store.Account{ProxyGroup: "clash"})
	if len(got) != 2 {
		t.Errorf("the candidates are %v", got)
	}
}

// TestTheUnreachableIsToldFromTheSlow says only the errors that say the
// proxy was never reached count as the proxy's fault.
//
// 上游慢、TLS 错、拿到了 502 —— 那些不是代理的错。全记的话，一次抖动就
// 把一条好路踢掉了。
func TestTheUnreachableIsToldFromTheSlow(t *testing.T) {
	dial := &net.OpError{Op: "dial", Err: &net.DNSError{Err: "no such host"}}
	if !Unreachable(dial) {
		t.Error("the dial failure is not unreachable")
	}
	proxy := &net.OpError{Op: "proxyconnect", Err: dial}
	if !Unreachable(proxy) {
		t.Error("the proxy failure is not unreachable")
	}
	read := &net.OpError{Op: "read", Err: net.ErrClosed}
	if Unreachable(read) {
		t.Error("the read failure is unreachable")
	}
	if Unreachable(nil) {
		t.Error("no error at all is unreachable")
	}
}
