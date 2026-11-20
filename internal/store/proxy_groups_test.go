package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestTheGroupsAreKept says a group is kept as it was written: the name
// is its identity, and the addresses keep their order.
//
// 顺序就是故障转移的顺序：第一个是主路，后面的都是备路。存进去的时候
// 顺序乱了，这个组就废了。
func TestTheGroupsAreKept(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now()
	err := s.SaveProxyGroup(ctx, ProxyGroup{
		Name: "clash", URLs: []string{"http://a:8080", "http://b:8080"},
		CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("save the group: %v", err)
	}
	got, err := s.GetProxyGroup(ctx, "clash")
	if err != nil {
		t.Fatalf("get the group: %v", err)
	}
	if got.Name != "clash" {
		t.Errorf("the group's name is %q", got.Name)
	}
	if len(got.URLs) != 2 || got.URLs[0] != "http://a:8080" || got.URLs[1] != "http://b:8080" {
		t.Errorf("the group's addresses are %v", got.URLs)
	}
}

// TestTheGroupIsChanged says a group that is saved again keeps its name
// and gets the new addresses.
//
// 组的名字是它的身份：改组是改它的地址，不是换一个新组来。
func TestTheGroupIsChanged(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now()
	if err := s.SaveProxyGroup(ctx, ProxyGroup{
		Name: "clash", URLs: []string{"http://a:8080"}, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("save the group: %v", err)
	}
	if err := s.SaveProxyGroup(ctx, ProxyGroup{
		Name: "clash", URLs: []string{"http://b:8080", "http://c:8080"}, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("change the group: %v", err)
	}
	got, err := s.GetProxyGroup(ctx, "clash")
	if err != nil {
		t.Fatalf("get the group: %v", err)
	}
	if len(got.URLs) != 2 || got.URLs[0] != "http://b:8080" {
		t.Errorf("the group's addresses are %v", got.URLs)
	}
}

// TestTheGroupThatIsNotThere says a group that is not there is said so.
func TestTheGroupThatIsNotThere(t *testing.T) {
	s := openTest(t)
	_, err := s.GetProxyGroup(context.Background(), "nope")
	if err == nil {
		t.Fatal("the group that is not there was found")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("the error is %v", err)
	}
}

// TestTheGroupsAreListedByName says the list is by name, so the page
// shows them in the same order every time.
func TestTheGroupsAreListedByName(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now()
	for _, n := range []string{"zh", "clash", "hk"} {
		if err := s.SaveProxyGroup(ctx, ProxyGroup{
			Name: n, URLs: []string{"http://" + n + ":8080"}, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("save the group %s: %v", n, err)
		}
	}
	got, err := s.ListProxyGroups(ctx)
	if err != nil {
		t.Fatalf("list the groups: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("there are %d groups", len(got))
	}
	if got[0].Name != "clash" || got[1].Name != "hk" || got[2].Name != "zh" {
		t.Errorf("the groups are %s, %s, %s", got[0].Name, got[1].Name, got[2].Name)
	}
}

// TestTheAccountPointsAtAGroup says an account can point at a group, and
// that the address and the group are one thing: setting one clears the
// other.
//
// 地址和组是二选一：账号只有一条出口。
func TestTheAccountPointsAtAGroup(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now()
	makeAccount(t, s, "acc_1")
	if err := s.SetAccountOutlet(ctx, "acc_1", "", "clash", now); err != nil {
		t.Fatalf("set the outlet: %v", err)
	}
	a, err := s.GetAccount(ctx, "acc_1")
	if err != nil {
		t.Fatalf("get the account: %v", err)
	}
	if a.ProxyGroup != "clash" {
		t.Errorf("the account's group is %q", a.ProxyGroup)
	}
	if a.ProxyURL != "" {
		t.Errorf("the account has an address of its own: %q", a.ProxyURL)
	}
	// 给了地址，组就该让位。
	if err := s.SetAccountOutlet(ctx, "acc_1", "http://a:8080", "", now); err != nil {
		t.Fatalf("set the outlet: %v", err)
	}
	a, err = s.GetAccount(ctx, "acc_1")
	if err != nil {
		t.Fatalf("get the account: %v", err)
	}
	if a.ProxyURL != "http://a:8080" {
		t.Errorf("the account's address is %q", a.ProxyURL)
	}
	if a.ProxyGroup != "" {
		t.Errorf("the account still points at the group %q", a.ProxyGroup)
	}
}

// TestTheGroupThatIsTakenAway says the accounts that pointed at a group
// go back to the global one when the group is taken away.
//
// 组没了，账号行上的组名就没人认了 —— 清掉，它们跟着全局走。
func TestTheGroupThatIsTakenAway(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now()
	for _, id := range []string{"acc_1", "acc_2"} {
		makeAccount(t, s, id)
		if err := s.SetAccountOutlet(ctx, id, "", "clash", now); err != nil {
			t.Fatalf("set the outlet: %v", err)
		}
	}
	if err := s.SaveProxyGroup(ctx, ProxyGroup{
		Name: "clash", URLs: []string{"http://a:8080"}, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("save the group: %v", err)
	}
	back, err := s.DeleteProxyGroup(ctx, "clash")
	if err != nil {
		t.Fatalf("take away the group: %v", err)
	}
	if back != 2 {
		t.Errorf("%d account(s) went back to the global group", back)
	}
	a, err := s.GetAccount(ctx, "acc_1")
	if err != nil {
		t.Fatalf("get the account: %v", err)
	}
	if a.ProxyGroup != "" {
		t.Errorf("the account still points at the group %q", a.ProxyGroup)
	}
}

// TestTheGlobalGroupCannotBeTakenAway says the global group stays.
//
// 全局组是保留名：它没了，"全局"就不全局了。
func TestTheGlobalGroupCannotBeTakenAway(t *testing.T) {
	s := openTest(t)
	if _, err := s.DeleteProxyGroup(context.Background(), GlobalGroup); err == nil {
		t.Fatal("the global group was taken away")
	}
}

// TestTheGlobalGroupIsMadeFromTheConfiguration says the global group is
// made from proxy.default when the database does not have it.
//
// 全局出口也是组：配置里的那个地址是它的种子。种子用过之后配置里那个就
// 不再管事了（页面上改的是库里那份）。
func TestTheGlobalGroupIsMadeFromTheConfiguration(t *testing.T) {
	s := openTest(t)
	res, err := s.SeedProxyGroups(context.Background(), "http://global:8080", nil)
	if err != nil {
		t.Fatalf("seed the groups: %v", err)
	}
	if !res.GlobalSeeded {
		t.Error("the global group was not made from the configuration")
	}
	g, err := s.GetProxyGroup(context.Background(), GlobalGroup)
	if err != nil {
		t.Fatalf("get the global group: %v", err)
	}
	if len(g.URLs) != 1 || g.URLs[0] != "http://global:8080" {
		t.Errorf("the global group's addresses are %v", g.URLs)
	}
}

// TestTheGroupsInTheConfigurationAreSeeded says a group in the
// configuration is made when the database does not have it.
//
// 老库里有一批账号指着组名（组名指向的地址在配置文件里）。组回到库里，
// 这些号不用动就还指着原来的那条路。
func TestTheGroupsInTheConfigurationAreSeeded(t *testing.T) {
	s := openTest(t)
	res, err := s.SeedProxyGroups(context.Background(), "",
		map[string]string{"clash": "http://clash:8080"})
	if err != nil {
		t.Fatalf("seed the groups: %v", err)
	}
	if len(res.Created) != 1 || res.Created[0] != "clash" {
		t.Errorf("the groups that were made are %v", res.Created)
	}
	g, err := s.GetProxyGroup(context.Background(), "clash")
	if err != nil {
		t.Fatalf("get the group: %v", err)
	}
	if len(g.URLs) != 1 || g.URLs[0] != "http://clash:8080" {
		t.Errorf("the group's addresses are %v", g.URLs)
	}
}

// TestTheGroupInTheDatabaseCounts says the group in the database is not
// touched by the one in the configuration.
//
// 库里是老家：人在页面上改过的组，配置里的那个不算数。
func TestTheGroupInTheDatabaseCounts(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now()
	if err := s.SaveProxyGroup(ctx, ProxyGroup{
		Name: "clash", URLs: []string{"http://mine:8080"}, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("save the group: %v", err)
	}
	res, err := s.SeedProxyGroups(ctx, "", map[string]string{"clash": "http://theirs:8080"})
	if err != nil {
		t.Fatalf("seed the groups: %v", err)
	}
	if len(res.Shadowed) != 1 || res.Shadowed[0] != "clash" {
		t.Errorf("the groups that were shadowed are %v", res.Shadowed)
	}
	g, err := s.GetProxyGroup(ctx, "clash")
	if err != nil {
		t.Fatalf("get the group: %v", err)
	}
	if len(g.URLs) != 1 || g.URLs[0] != "http://mine:8080" {
		t.Errorf("the group's addresses are %v", g.URLs)
	}
}

// TestTheGroupNameThatIsNotThereIsSaid says an account that points at a
// group that is not in the database is named.
//
// 那个账号跟着全局组走（组名留着：人把组建出来，它就自己回去了）。但人
// 该知道有账号指着一条不存在的路。
func TestTheGroupNameThatIsNotThereIsSaid(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now()
	makeAccount(t, s, "acc_1")
	if err := s.SetAccountOutlet(ctx, "acc_1", "", "hk", now); err != nil {
		t.Fatalf("set the outlet: %v", err)
	}
	res, err := s.SeedProxyGroups(ctx, "http://global:8080", nil)
	if err != nil {
		t.Fatalf("seed the groups: %v", err)
	}
	if len(res.Unknown) != 1 || res.Unknown[0] != "hk" {
		t.Errorf("the unknown groups are %v", res.Unknown)
	}
	if res.UnknownAccounts != 1 {
		t.Errorf("%d account(s) point at a group that is not there", res.UnknownAccounts)
	}
}
