package account

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

func TestPlanWindowsEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
