package account

import (
	"context"
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page_number")
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
