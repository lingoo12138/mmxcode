package egress

import (
	"errors"
	"net"
	"strings"

	"github.com/lingoo12138/mmxcode/internal/store"
)

// SetGroups gives the resolver the groups it does not know yet.
//
// 组是换过的：页面上改了一个组，网关下一次挑地址就该按新的来。这是一份
// 快照 —— 换的时候整份换，不是一组一组地换（换到一半的快照会挑出半个组
// 里的地址）。
func (r *Resolver) SetGroups(groups map[string][]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	next := make(map[string][]string, len(groups))
	for name, urls := range groups {
		next[name] = append([]string(nil), urls...)
	}
	r.groups = next
}

// SetHealth says what the last probe of an address said.
//
// 探活的结果是用来排序的：探通过的先走。
func (r *Resolver) SetHealth(url string, ok bool) {
	url = strings.TrimSpace(url)
	if url == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.health[url] = ok
}

// Candidates gives the addresses an account's requests may go through, in
// the order they are tried.
//
// 出口是四级的：账号自己的地址 → 账号指着的组 → 全局组 → 直连。组里的
// 地址按健康排（探通过的、没探过的、探失败的）。
//
// 一个候选都没有就是直连 —— 直连是最后一级，不是错。
func (r *Resolver) Candidates(a store.Account) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	seen := map[string]bool{}
	add := func(u string) {
		u = strings.TrimSpace(u)
		if u == "" || seen[u] {
			return
		}
		seen[u] = true
		out = append(out, u)
	}
	if u := strings.TrimSpace(a.ProxyURL); u != "" {
		add(u)
	}
	if g := strings.TrimSpace(a.ProxyGroup); g != "" {
		for _, u := range r.order(r.groups[g]) {
			add(u)
		}
	}
	for _, u := range r.order(r.groups[store.GlobalGroup]) {
		add(u)
	}
	// 解析器还没有组（开机的时候还没读库）：用配置里那个全局默认。
	if _, ok := r.groups[store.GlobalGroup]; !ok {
		if v := strings.TrimSpace(r.def); v != "" {
			add(v)
		}
	}
	return out
}

// order puts the addresses that answered first, then the ones that were
// never probed, then the ones that did not answer.
//
// 探失败的最后：探的结果可能是旧的（探完到现在过了一个轮次，代理早就活
// 了）。
func (r *Resolver) order(urls []string) []string {
	if len(urls) < 2 {
		return urls
	}
	var good, unknown, bad []string
	for _, u := range urls {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		switch ok, seen := r.health[u]; {
		case seen && ok:
			good = append(good, u)
		case seen:
			bad = append(bad, u)
		default:
			unknown = append(unknown, u)
		}
	}
	out := make([]string, 0, len(urls))
	out = append(out, good...)
	out = append(out, unknown...)
	out = append(out, bad...)
	return out
}

// Unreachable says whether an error means the proxy was never reached.
//
// 只有「根本没连上」才算代理的错：dial 失败、proxyconnect 失败、代理自己
// 的域名解析不了。上游慢、超时、TLS 握手错、上游回了 502 —— 那些不是代
// 理的错，不该把它记成坏的。
//
// 全记的话，一次抖动就把一条好路踢掉了；全不记的话，一个死掉的代理在下
// 一轮健康检查之前，每个请求都先撞它一次。
func Unreachable(err error) bool {
	if err == nil {
		return false
	}
	var op *net.OpError
	if errors.As(err, &op) {
		switch op.Op {
		case "dial", "proxyconnect":
			return true
		}
	}
	var dns *net.DNSError
	return errors.As(err, &dns)
}

// GroupMap gives the groups as a map from the name to the addresses.
//
// 页面上、健康检查里、网关上都用这一份：一个组一个名字，名字下面是它
// 的地址。
func GroupMap(groups []store.ProxyGroup) map[string][]string {
	out := make(map[string][]string, len(groups))
	for _, g := range groups {
		out[g.Name] = append([]string(nil), g.URLs...)
	}
	return out
}

// GroupsWithDefault puts the configuration's default address into the
// groups when the global group is not there.
//
// 库里还没有全局组的时候（刚开机还没种，或者种失败了），出口走的就是配置
// 里那个全局默认 —— 解析器也是这么挑的。健康检查和页面得查同一个地址：查
// 的是另一个，说的是另一件事。
func GroupsWithDefault(groups map[string][]string, def string) map[string][]string {
	out := make(map[string][]string, len(groups)+1)
	for name, urls := range groups {
		out[name] = append([]string(nil), urls...)
	}
	if _, ok := out[store.GlobalGroup]; !ok {
		if v := strings.TrimSpace(def); v != "" {
			out[store.GlobalGroup] = []string{v}
		}
	}
	return out
}
