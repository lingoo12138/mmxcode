package account

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"mmxcode/internal/site"
	"mmxcode/internal/upstream"
)

func testClient(t *testing.T) *upstream.Client {
	t.Helper()
	return upstream.New(upstream.Options{Timeout: 5 * time.Second})
}

func TestIdentity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/api/user/info" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("yy"); got == "" {
			t.Error("no yy header")
		}
		if got := r.Header.Get("x-signature"); got == "" {
			t.Error("no x-signature header")
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("authorization = %q", got)
		}
		w.Write([]byte(`{"data":{"userInfo":{"realUserID":"1234567890","userEmail":"a@example.com","name":"Ada"}}}`))
	}))
	defer srv.Close()

	c := New(testClient(t))
	c.SetBase(srv.URL, srv.URL)
	id, err := c.Identity(context.Background(), "tok", "1234567890")
	if err != nil {
		t.Fatal(err)
	}
	if id.RealUserID != "1234567890" || id.Email != "a@example.com" || id.Name != "Ada" {
		t.Fatalf("identity = %+v", id)
	}
}

func TestIdentityNoUserIDInAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":{}}`))
	}))
	defer srv.Close()

	c := New(testClient(t))
	c.SetBase(srv.URL, srv.URL)
	if _, err := c.Identity(context.Background(), "tok", ""); err == nil {
		t.Fatal("expected an error")
	}
}

func TestLedgerPaging(t *testing.T) {
	var pages []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page_number")
		pages = append(pages, page)
		if page == "1" {
			w.Write([]byte(`{"total_count":3,"details":[
				{"credit_type":1,"granted_amount":"1,200.5","consumed_amount":"100","remaining_amount":"1100.5","granted_at_ms":1763000000000,"expire_at_ms":1900000000000},
				{"credit_type":2,"granted_amount":"50","consumed_amount":"0","remaining_amount":"50","granted_at_ms":1763000000000,"expire_at_ms":1900000000000}
			]}`))
			return
		}
		w.Write([]byte(`{"total_count":3,"details":[
			{"credit_type":1,"granted_amount":"100","consumed_amount":"0","remaining_amount":"100","granted_at_ms":1763000000000,"expire_at_ms":1900000000000}
		]}`))
	}))
	defer srv.Close()

	c := New(testClient(t))
	c.SetBase(srv.URL, srv.URL)
	batches, err := c.Ledger(context.Background(), "tok", "1234567890")
	if err != nil {
		t.Fatal(err)
	}
	if len(batches) != 3 {
		t.Fatalf("got %d batches", len(batches))
	}
	if batches[0].Granted != 1200.5 || batches[0].Remaining != 1100.5 {
		t.Fatalf("batch = %+v", batches[0])
	}
	// 页面要一页一页地问，跳过第 2 页的 bug 不能混过去
	if len(pages) != 2 || pages[0] != "1" || pages[1] != "2" {
		t.Fatalf("pages = %v, want [1 2]", pages)
	}
}

// TestLedgerUnreadableAmount says the credits cannot be read.
//
// The reference does not answer a part of the ledger: a batch whose
// amount is not a number would tell a wrong share.
func TestLedgerUnreadableAmount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"total_count":1,"details":[
			{"credit_type":1,"granted_amount":"abc","consumed_amount":"0","remaining_amount":"1100.5","granted_at_ms":1763000000000,"expire_at_ms":1900000000000}
		]}`))
	}))
	defer srv.Close()

	c := New(testClient(t))
	c.SetBase(srv.URL, srv.URL)
	batches, err := c.Ledger(context.Background(), "tok", "1234567890")
	if err != nil {
		t.Fatal(err)
	}
	if batches != nil {
		t.Fatalf("batches = %+v, want none", batches)
	}
}

// TestLedgerMissingExpiry says the time a batch runs out is not there.
//
// It is not the same as a batch that ran out: the credits of such a
// batch cannot be read, and the batch must not be called expired.
func TestLedgerMissingExpiry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"total_count":1,"details":[
			{"credit_type":1,"granted_amount":"600","consumed_amount":"100","remaining_amount":"500","granted_at_ms":1763000000000}
		]}`))
	}))
	defer srv.Close()

	c := New(testClient(t))
	c.SetBase(srv.URL, srv.URL)
	batches, err := c.Ledger(context.Background(), "tok", "1234567890")
	if err != nil {
		t.Fatal(err)
	}
	if len(batches) != 1 {
		t.Fatalf("got %d batches", len(batches))
	}
	b := batches[0]
	if b.HasExpiresAt {
		t.Fatalf("batch = %+v, want no expiry", b)
	}
	if !b.ExpiresAt.IsZero() {
		t.Fatalf("expires at = %v, want the zero time", b.ExpiresAt)
	}
	if b.Granted != 600 || b.Remaining != 500 {
		t.Fatalf("batch = %+v", b)
	}
}

// TestLedgerExpireAtZero says a batch that expires at zero has expired.
//
// The time is there, and it is in the past: that is a spent batch, not
// an unreadable one.
func TestLedgerExpireAtZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"total_count":1,"details":[
			{"credit_type":1,"granted_amount":"600","consumed_amount":"0","remaining_amount":"600","expire_at_ms":0}
		]}`))
	}))
	defer srv.Close()

	c := New(testClient(t))
	c.SetBase(srv.URL, srv.URL)
	batches, err := c.Ledger(context.Background(), "tok", "1234567890")
	if err != nil {
		t.Fatal(err)
	}
	if len(batches) != 1 {
		t.Fatalf("got %d batches", len(batches))
	}
	b := batches[0]
	if !b.HasExpiresAt {
		t.Fatalf("batch = %+v, want an expiry", b)
	}
	if b.ExpiresAt.IsZero() {
		t.Fatalf("expires at = %v, want the epoch", b.ExpiresAt)
	}
}

// TestLedgerEmptyIsNotUnreadable says a ledger that was read and is
// empty is not a ledger that could not be read.
//
// 读到空账本是“没有积分”，读不到账本是“不知道”：两回事。
func TestLedgerEmptyIsNotUnreadable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"total_count":0,"details":[]}`))
	}))
	defer srv.Close()

	c := New(testClient(t))
	c.SetBase(srv.URL, srv.URL)
	batches, err := c.Ledger(context.Background(), "tok", "1234567890")
	if err != nil {
		t.Fatal(err)
	}
	if batches == nil {
		t.Fatal("an empty ledger must not look like one that could not be read")
	}
	if len(batches) != 0 {
		t.Fatalf("got %d batches, want none", len(batches))
	}
}

func TestLedgerBusinessError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"statusInfo":{"code":1000048,"message":"the sign-in was refused"}}`))
	}))
	defer srv.Close()

	c := New(testClient(t))
	c.SetBase(srv.URL, srv.URL)
	if _, err := c.Ledger(context.Background(), "tok", "1234567890"); err == nil {
		t.Fatal("expected an error")
	}
}

func TestParseAmount(t *testing.T) {
	cases := []struct {
		in   string
		want float64
		ok   bool
	}{
		{"1,200.5", 1200.5, true},
		{"0", 0, true},
		{"", 0, false},
		{"abc", 0, false},
		{"-5", 0, true},
	}
	for _, c := range cases {
		got, ok := ParseAmount(c.in, true)
		if ok != c.ok {
			t.Errorf("ParseAmount(%q) ok = %v, want %v", c.in, ok, c.ok)
			continue
		}
		if ok && got != c.want {
			t.Errorf("ParseAmount(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestPlanWindows(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"base_resp":{"status_code":0},"model_remains":[
			{"model_name":"general","current_interval_status":2,"current_interval_remaining_percent":10,
			 "current_interval_total_percent":100,"current_interval_total_count":150,"current_interval_used_count":24,
			 "start_time":1763000000000,"end_time":1763010000000,
			 "current_weekly_status":2,"current_weekly_remaining_percent":80,
			 "current_weekly_total_percent":100,"weekly_start_time":1762800000000,"weekly_end_time":1763400000000}
		]}`))
	}))
	defer srv.Close()

	c := New(testClient(t))
	c.SetBase(srv.URL, srv.URL)
	ws, err := c.PlanWindows(context.Background(), "tok", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 2 {
		t.Fatalf("got %d windows: %+v", len(ws), ws)
	}
	// status 2 是用完了，所以是 100%
	if ws[0].Used != 100 {
		t.Fatalf("interval used = %v, want 100", ws[0].Used)
	}
	if ws[0].Amount != 150 || ws[0].Limit != 150 {
		t.Fatalf("window = %+v", ws[0])
	}
}

// TestPlanWindowsFallsBackToTheOpenPlatform says the open platform's
// answer is used when the console's own answer has no windows in it.
//
// 控制台自己的 remains_percent 在国际站上可能回一个空的 model_remains。
// 参考实现这时候去问 coding_plan/remains，Go 代码原来直接说"没有窗口"：
// 账号的额度用完了却看不出来，故障转移一直挑它。
func TestPlanWindowsFallsBackToTheOpenPlatform(t *testing.T) {
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Path)
		switch r.URL.Path {
		case site.PlanRemainsPath:
			w.Write([]byte(`{"base_resp":{"status_code":0},"model_remains":[]}`))
		case site.CodingPlanPath:
			w.Write([]byte(`{"model_remains":[
				{"model_name":"general","current_interval_status":2,"current_interval_remaining_percent":10,
				 "current_interval_total_percent":100,"current_interval_total_count":150,"current_interval_used_count":24,
				 "start_time":1763000000000,"end_time":1763010000000,
				 "current_weekly_status":2,"current_weekly_remaining_percent":80,
				 "current_weekly_total_percent":100,"weekly_start_time":1762800000000,"weekly_end_time":1763400000000}
			]}`))
		default:
			t.Errorf("path = %s", r.URL.Path)
			w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	c := New(testClient(t))
	c.SetBase(srv.URL, srv.URL)
	ws, err := c.PlanWindows(context.Background(), "tok", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 2 {
		t.Fatalf("got %d windows: %+v", len(ws), ws)
	}
	if ws[0].Amount != 150 || ws[0].Limit != 150 {
		t.Fatalf("window = %+v", ws[0])
	}
	want := []string{site.PlanRemainsPath, site.CodingPlanPath}
	if len(asked) != len(want) {
		t.Fatalf("asked %v, want %v", asked, want)
	}
	for i := range want {
		if asked[i] != want[i] {
			t.Fatalf("asked %v, want %v", asked, want)
		}
	}
}

// TestPlanWindowsEmpty says a plan with no windows at all is no
// windows, and not an error.
//
// 两个端点都没有窗口的时候，答案是"没有窗口"，不是"读不出来"。
func TestPlanWindowsEmpty(t *testing.T) {
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Path)
		w.Write([]byte(`{"base_resp":{"status_code":0},"model_remains":[]}`))
	}))
	defer srv.Close()

	c := New(testClient(t))
	c.SetBase(srv.URL, srv.URL)
	ws, err := c.PlanWindows(context.Background(), "tok", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 0 {
		t.Fatalf("got %+v", ws)
	}
	want := []string{site.PlanRemainsPath, site.CodingPlanPath}
	if len(asked) != len(want) {
		t.Fatalf("asked %v, want %v", asked, want)
	}
	for i := range want {
		if asked[i] != want[i] {
			t.Fatalf("asked %v, want %v", asked, want)
		}
	}
}

// TestTheWindowEndIsReadInSecondsToo says a window's end is read the
// way its length is read.
//
// 站点写时间不说是秒还是毫秒：一万亿以下是秒。窗口的结束时间要是按
// 毫秒读，一个秒的时间就变成了 1970 年 —— 窗口早就"回来了"，于是
// 用完的账号一次又一次被挑中。
func TestTheWindowEndIsReadInSecondsToo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"base_resp":{"status_code":0},"model_remains":[
			{"model_name":"general","current_interval_status":2,"current_interval_remaining_percent":10,
			 "current_interval_total_percent":100,"current_interval_total_count":150,"current_interval_used_count":24,
			 "start_time":1763000000,"end_time":1763010800,
			 "current_weekly_status":2,"current_weekly_remaining_percent":80,
			 "current_weekly_total_percent":100,"weekly_start_time":1762800000,"weekly_end_time":1763400000}
		]}`))
	}))
	defer srv.Close()

	c := New(testClient(t))
	c.SetBase(srv.URL, srv.URL)
	ws, err := c.PlanWindows(context.Background(), "tok", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 2 {
		t.Fatalf("got %d windows: %+v", len(ws), ws)
	}
	if got := ws[0].ResetsAt.UnixMilli(); got != 1763010800000 {
		t.Fatalf("the window resets at %d, want 1763010800000", got)
	}
	// 长度也一样：三个小时（秒写和毫秒写都是三个小时）
	if ws[0].Name != "3 hours" {
		t.Fatalf("window = %+v, want the three hour window", ws[0])
	}
}

// TestPlanWindowsKeepsTheConsoleAnswerWhenItsListIsNotEmpty says a
// list that is not empty is the console's answer, even when every entry
// in it is a bucket the plan does not have.
//
// 参考实现看的是回答里有没有那个列表，不是我们能从列表里做出几个
// 窗口。列表在，答案就在：这时这个套餐就是没有窗口，那是答案，不是
// 再问一次的理由。
func TestPlanWindowsKeepsTheConsoleAnswerWhenItsListIsNotEmpty(t *testing.T) {
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Path)
		switch r.URL.Path {
		case site.PlanRemainsPath:
			// 一个套餐没有的桶：两个窗口都是无限的、两个都没有计数。
			w.Write([]byte(`{"base_resp":{"status_code":0},"model_remains":[
				{"model_name":"general","current_interval_status":3,"current_weekly_status":3,
				 "current_interval_total_count":0,"current_weekly_total_count":0}
			]}`))
		default:
			t.Errorf("the open platform was asked: %s", r.URL.Path)
			w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	c := New(testClient(t))
	c.SetBase(srv.URL, srv.URL)
	ws, err := c.PlanWindows(context.Background(), "tok", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 0 {
		t.Fatalf("got %+v, want no windows", ws)
	}
	if len(asked) != 1 || asked[0] != site.PlanRemainsPath {
		t.Fatalf("asked %v, want only the console", asked)
	}
}

// TestWindowName says a window is named after its own length.
//
// 窗口的名字是它自己的长度：三小时的窗口叫 "3 hours"，不是写死的
// "5 hours"。一周的窗口叫 "7 days"，没有长度的窗口叫 "Allowance"。
func TestWindowName(t *testing.T) {
	cases := []struct {
		name   string
		weekly bool
		span   time.Duration
		want   string
	}{
		{"three hours", false, 3 * time.Hour, "3 hours"},
		{"five hours", false, 5 * time.Hour, "5 hours"},
		{"one hour", false, time.Hour, "1 hours"},
		{"a day", false, 24 * time.Hour, "24 hours"},
		{"two days", false, 48 * time.Hour, "2 days"},
		{"forty five minutes", false, 45 * time.Minute, "45 minutes"},
		{"a week", true, 7 * 24 * time.Hour, "7 days"},
		{"a week without a span", true, 0, "7 days"},
		{"no span", false, 0, "Allowance"},
	}
	for _, c := range cases {
		got := windowName(c.weekly, c.span)
		if got != c.want {
			t.Errorf("windowName(%v, %v) = %q, want %q", c.weekly, c.span, got, c.want)
		}
	}
}

// TestPlanWindowsNameTheWindowByItsLength says the name of a window
// comes from the answer.
//
// 站点给三小时的窗口，名字就该是 "3 hours"。
func TestPlanWindowsNameTheWindowByItsLength(t *testing.T) {
	// 三小时：开始 1763000000000，结束 1763010800000。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case site.PlanRemainsPath:
			w.Write([]byte(`{"base_resp":{"status_code":0},"model_remains":[
				{"model_name":"general","current_interval_status":2,"current_interval_remaining_percent":10,
				 "current_interval_total_percent":100,"current_interval_total_count":150,"current_interval_used_count":24,
				 "start_time":1763000000000,"end_time":1763010800000,
				 "current_weekly_status":2,"current_weekly_remaining_percent":80,
				 "current_weekly_total_percent":100,"weekly_start_time":1762800000000,"weekly_end_time":1763400000000}
			]}`))
		default:
			t.Errorf("path = %s", r.URL.Path)
			w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	c := New(testClient(t))
	c.SetBase(srv.URL, srv.URL)
	ws, err := c.PlanWindows(context.Background(), "tok", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 2 {
		t.Fatalf("got %d windows: %+v", len(ws), ws)
	}
	if ws[0].Name != "3 hours" {
		t.Errorf("the window is called %q, want %q", ws[0].Name, "3 hours")
	}
	if ws[1].Name != "7 days" {
		t.Errorf("the weekly window is called %q, want %q", ws[1].Name, "7 days")
	}
}

// TestTheWeeksBoostIsItsTotal says the week's boost is the week's
// total when the site writes no total of its own.
//
// 站点给的是千分数：1200 是一百二十。用掉的百分比是按这个总数算的，
// 所以六成是"这个总数的六成"，不是"一百的六成"。
func TestTheWeeksBoostIsItsTotal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case site.PlanRemainsPath:
			w.Write([]byte(`{"base_resp":{"status_code":0},"model_remains":[
				{"model_name":"general","current_interval_status":1,"current_interval_remaining_percent":50,
				 "current_interval_total_percent":100,
				 "start_time":1763000000000,"end_time":1763010800000,
				 "current_weekly_status":1,"current_weekly_used_percent":72,"weekly_boost_permille":1200,
				 "weekly_start_time":1762800000000,"weekly_end_time":1763400000000}
			]}`))
		default:
			t.Errorf("path = %s", r.URL.Path)
			w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	c := New(testClient(t))
	c.SetBase(srv.URL, srv.URL)
	ws, err := c.PlanWindows(context.Background(), "tok", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 2 {
		t.Fatalf("got %d windows: %+v", len(ws), ws)
	}
	week := ws[1]
	if week.Name != "7 days" {
		t.Fatalf("the weekly window is called %q, want %q", week.Name, "7 days")
	}
	if week.Limit != 120 {
		t.Errorf("the week's total is %v, want 120", week.Limit)
	}
	// 用掉的七十二点是一百二十点里的六成。
	if week.Used != 60 {
		t.Errorf("the week is %v%% used, want 60%%", week.Used)
	}
	if week.Amount != 72 {
		t.Errorf("the week used %v of %v, want 72", week.Amount, week.Limit)
	}
	if week.Unit != "%" {
		t.Errorf("the week's unit is %q, want %%", week.Unit)
	}
}

func TestPlanTitle(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "cycle_audio_resource_package") {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Write([]byte(`{"current_subscribe":{"current_subscribe_title":"TokenPlanMax-年度会员"}}`))
	}))
	defer srv.Close()

	c := New(testClient(t))
	c.SetBase(srv.URL, srv.URL)
	title, err := c.PlanTitle(context.Background(), "tok", "")
	if err != nil {
		t.Fatal(err)
	}
	if title != "TokenPlanMax-年度会员" {
		t.Fatalf("title = %q", title)
	}
}

func TestModelsFallback(t *testing.T) {
	// 没有 token 时给内置的列表
	c := New(nil)
	ms := c.Models("")
	if len(ms) == 0 {
		t.Fatal("expected the fallback models")
	}
}

// TestMembership says the plan is read from the answer's data.
//
// 真的答案把字段放在 data 下面，只读顶层的话一个真的套餐会被说成没有。
func TestMembership(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/matrix/api/v1/commerce/get_membership_info" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Write([]byte(`{"data":{"has_token_plan":true,"token_plan_tier":"TokenPlanMax","op_group_id":"grp-1","op_credit_summary":{"total_remaining_amount":"1234.5"}}}`))
	}))
	defer srv.Close()

	c := New(testClient(t))
	c.SetBase(srv.URL, srv.URL)
	m, err := c.Membership(context.Background(), "tok", "1234567890")
	if err != nil {
		t.Fatal(err)
	}
	if !m.Known {
		t.Fatal("the plan is unknown")
	}
	if !m.HasTokenPlan {
		t.Fatalf("membership = %+v, want a plan", m)
	}
	if m.Tier != "TokenPlanMax" {
		t.Fatalf("tier = %q", m.Tier)
	}
	if m.GroupID != "grp-1" {
		t.Fatalf("group = %q", m.GroupID)
	}
	if m.Balance != "1234.5" {
		t.Fatalf("balance = %q", m.Balance)
	}
}

// TestMembershipTopLevelWins says the top level wins over the data.
//
// 顶层的字段优先，data 里的同名字段只在顶层没有的时候用。
func TestMembershipTopLevelWins(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"has_token_plan":false,"token_plan_tier":"Top","op_group_id":"grp-top","data":{"has_token_plan":true,"token_plan_tier":"Under","op_group_id":"grp-under"}}`))
	}))
	defer srv.Close()

	c := New(testClient(t))
	c.SetBase(srv.URL, srv.URL)
	m, err := c.Membership(context.Background(), "tok", "1234567890")
	if err != nil {
		t.Fatal(err)
	}
	if m.HasTokenPlan {
		t.Fatalf("membership = %+v, want no plan", m)
	}
	if m.Tier != "Top" || m.GroupID != "grp-top" {
		t.Fatalf("membership = %+v", m)
	}
}

// TestMembershipOpcreditBalance says the balance is the account's own.
//
// 没有 op_credit_summary 时用 opcredit_balance，字符串和数字都认。
func TestMembershipOpcreditBalance(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":{"has_token_plan":true,"opcredit_balance":" 42.5 "}}`))
	}))
	defer srv.Close()

	c := New(testClient(t))
	c.SetBase(srv.URL, srv.URL)
	m, err := c.Membership(context.Background(), "tok", "1234567890")
	if err != nil {
		t.Fatal(err)
	}
	if m.Balance != "42.5" {
		t.Fatalf("balance = %q, want 42.5", m.Balance)
	}
}

// TestMembershipNotFound says the plan is unknown, not that there is none.
//
// 国际站对会员接口回 404，那是“不知道”，不是“没有”。
func TestMembershipNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`404 page not found`))
	}))
	defer srv.Close()

	c := New(testClient(t))
	c.SetBase(srv.URL, srv.URL)
	m, err := c.Membership(context.Background(), "tok", "1234567890")
	if err != nil {
		t.Fatal(err)
	}
	if m.Known {
		t.Fatalf("membership = %+v, want an unknown plan", m)
	}
	if m.HasTokenPlan {
		t.Fatalf("membership = %+v, want no plan", m)
	}
}

// TestPlanWindowsNegativeCount says a negative count is not a count.
//
// 负数的已用次数不是次数，不能拿它当金额。
func TestPlanWindowsNegativeCount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"base_resp":{"status_code":0},"model_remains":[
			{"model_name":"general","current_interval_status":1,"current_interval_remaining_percent":90,
			 "current_interval_total_percent":100,"current_interval_total_count":10,"current_interval_used_count":-5,
			 "start_time":1763000000000,"end_time":1763010000000}
		]}`))
	}))
	defer srv.Close()

	c := New(testClient(t))
	c.SetBase(srv.URL, srv.URL)
	ws, err := c.PlanWindows(context.Background(), "tok", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 1 {
		t.Fatalf("got %d windows: %+v", len(ws), ws)
	}
	if ws[0].Amount != 0 || ws[0].Limit != 0 {
		t.Fatalf("window = %+v, want no count", ws[0])
	}
}

// TestPlanWindowsEmptyStatusMsg says the error names the status code.
//
// 回答没有 status_msg 时，错误里至少要有一个状态码，不能是空字符串。
func TestPlanWindowsEmptyStatusMsg(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"base_resp":{"status_code":1001,"status_msg":""}}`))
	}))
	defer srv.Close()

	c := New(testClient(t))
	c.SetBase(srv.URL, srv.URL)
	_, err := c.PlanWindows(context.Background(), "tok", "")
	if err == nil {
		t.Fatal("expected an error")
	}
	if err.Error() == "" {
		t.Fatalf("error = %q, want a text", err.Error())
	}
	if !strings.Contains(err.Error(), "1001") {
		t.Fatalf("error = %q, want the status code", err.Error())
	}
}

// TestCallPostNilBody says the body on the wire is what is signed.
//
// 签名算的是 "{}"，发出去的也得是 "{}"，不然上游会拒。
func TestCallPostNilBody(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = string(b)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := New(testClient(t))
	c.SetBase(srv.URL, srv.URL)
	if _, err := c.Call(context.Background(), "tok", "1234567890", http.MethodPost, "/v1/whatever", nil, nil); err != nil {
		t.Fatal(err)
	}
	if got != "{}" {
		t.Fatalf("body = %q, want {}", got)
	}
}

// TestCallGetHasNoBody says a GET signs and sends nothing.
func TestCallGetHasNoBody(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = string(b)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := New(testClient(t))
	c.SetBase(srv.URL, srv.URL)
	if _, err := c.Call(context.Background(), "tok", "1234567890", http.MethodGet, "/v1/whatever", nil, nil); err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("body = %q, want nothing", got)
	}
}

// TestModelsLive says the live list is asked for with the token.
func TestModelsLive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mavis/api/v1/models" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("authorization = %q", got)
		}
		w.Write([]byte(`{"providers":[{"providerId":"minimax","config":{"models":{"MiniMax-M3.1":{"name":"M3.1","limit":{"context":1048576,"output":524288},"modalities":{"input":["text","image"]},"reasoning":true,"thinking":{"effortOptions":["low","high","none"]}}},"model_order":["MiniMax-M3.1"]}}]}`))
	}))
	defer srv.Close()

	c := New(testClient(t))
	c.SetBase(srv.URL, srv.URL)
	ms := c.Models("tok")
	if len(ms) != 1 {
		t.Fatalf("got %d models: %+v", len(ms), ms)
	}
	if ms[0].ID != "MiniMax-M3.1" || ms[0].Name != "M3.1" {
		t.Fatalf("model = %+v", ms[0])
	}
	if ms[0].Context != 1048576 || ms[0].Output != 524288 {
		t.Fatalf("model = %+v", ms[0])
	}
	if len(ms[0].Efforts) != 2 {
		t.Fatalf("efforts = %v, want two", ms[0].Efforts)
	}
}

func TestTheSiteSaysHowMuchOfTheWeekIsUsed(t *testing.T) {
	// 站点自己说"用掉了百分之三十七"，那就是三十七。参考实现用的是
	// 它写的那个数，不是我们自己算出来的那个 —— 自己算会得到
	// 37.00000000000001 这种东西。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case site.PlanRemainsPath:
			w.Write([]byte(`{"base_resp":{"status_code":0},"model_remains":[
				{"model_name":"general","current_interval_status":3,
				 "current_weekly_status":1,"current_weekly_used_percent":37,"weekly_boost_permille":1200,
				 "weekly_start_time":1762800000000,"weekly_end_time":1763400000000}
			]}`))
		default:
			t.Errorf("path = %s", r.URL.Path)
			w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	c := New(testClient(t))
	c.SetBase(srv.URL, srv.URL)
	ws, err := c.PlanWindows(context.Background(), "tok", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 2 {
		t.Fatalf("got %d windows: %+v", len(ws), ws)
	}
	// 第一个窗口是那个不限量的（状态 3），周窗口是第二个。
	week := ws[1]
	if week.Amount != 37 {
		t.Errorf("the week used %v, want 37", week.Amount)
	}
	if week.Limit != 120 {
		t.Errorf("the week's total is %v, want 120", week.Limit)
	}
	if week.Unit != "%" {
		t.Errorf("the week's unit is %q, want %%", week.Unit)
	}
}

func TestAMissingCountIsNoCount(t *testing.T) {
	// 站点没写"用掉了几个"，那就没有这个数 —— 参考实现读到的那个数是
	// undefined，而 undefined 不是"大于等于零"，所以这个窗口走的是
	// 百分比那一支，不是"0 个 / 150 个"。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case site.PlanRemainsPath:
			w.Write([]byte(`{"base_resp":{"status_code":0},"model_remains":[
				{"model_name":"general","current_interval_status":3,
				 "current_weekly_status":1,"current_weekly_used_percent":72,
				 "current_weekly_total_count":150,"weekly_boost_permille":1200,
				 "weekly_start_time":1762800000000,"weekly_end_time":1763400000000}
			]}`))
		default:
			t.Errorf("path = %s", r.URL.Path)
			w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	c := New(testClient(t))
	c.SetBase(srv.URL, srv.URL)
	ws, err := c.PlanWindows(context.Background(), "tok", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 2 {
		t.Fatalf("got %d windows: %+v", len(ws), ws)
	}
	// 第一个窗口是那个不限量的（状态 3），周窗口是第二个。
	week := ws[1]
	if week.Amount != 72 {
		t.Errorf("the week used %v, want 72", week.Amount)
	}
	if week.Limit != 120 {
		t.Errorf("the week's total is %v, want 120", week.Limit)
	}
	if week.Unit != "%" {
		t.Errorf("the week's unit is %q, want %%", week.Unit)
	}
}

func TestAWindowWithNothingInItIsNotAWindow(t *testing.T) {
	// 既没有"还剩多少"，也没有"用掉多少"，这个窗口就不存在 —— 除非
	// 状态说它用完了（2）或者不限量（3）。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case site.PlanRemainsPath:
			w.Write([]byte(`{"base_resp":{"status_code":0},"model_remains":[
				{"model_name":"general","current_interval_status":1,
				 "current_weekly_status":1,"weekly_boost_permille":1200}
			]}`))
		default:
			t.Errorf("path = %s", r.URL.Path)
			w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	c := New(testClient(t))
	c.SetBase(srv.URL, srv.URL)
	ws, err := c.PlanWindows(context.Background(), "tok", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 0 {
		t.Fatalf("got %d windows, want none: %+v", len(ws), ws)
	}
}

func TestThePlanNumbersMayBeStrings(t *testing.T) {
	// 站点有时把数字写成字符串。参考实现两种都收（num() 和 pct()），
	// 所以 "1200" 是 1200，不是"这份回答里没有套餐"。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case site.PlanRemainsPath:
			w.Write([]byte(`{"base_resp":{"status_code":0},"model_remains":[
				{"model_name":"general","current_interval_status":"3",
				 "current_weekly_status":"1","current_weekly_used_percent":"72",
				 "weekly_boost_permille":"1200",
				 "weekly_start_time":"1762800000000","weekly_end_time":"1763400000000"}
			]}`))
		default:
			t.Errorf("path = %s", r.URL.Path)
			w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	c := New(testClient(t))
	c.SetBase(srv.URL, srv.URL)
	ws, err := c.PlanWindows(context.Background(), "tok", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 2 {
		t.Fatalf("got %d windows: %+v", len(ws), ws)
	}
	// 第一个窗口是那个不限量的（状态 3），周窗口是第二个。
	week := ws[1]
	if week.Amount != 72 {
		t.Errorf("the week used %v, want 72", week.Amount)
	}
	if week.Limit != 120 {
		t.Errorf("the week's total is %v, want 120", week.Limit)
	}
}
