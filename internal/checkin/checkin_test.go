package checkin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"mmxcode/internal/account"
	"mmxcode/internal/upstream"
)

// panel makes a seven-day panel, with the given day's status.
func panel(todayStatus int, todayIsToday bool) string {
	days := make([]map[string]any, 7)
	for i := 0; i < 7; i++ {
		status := 1
		switch {
		case i == 3:
			status = todayStatus
		case i < 3:
			status = 3
		}
		days[i] = map[string]any{
			"day_no":   i + 1,
			"status":   status,
			"is_today": i == 3 && todayIsToday,
			"points":   25,
		}
	}
	return `{"data":{"days":[` + join(days) + `]}}`
}

func join(days []map[string]any) string {
	out := ""
	for i, d := range days {
		if i > 0 {
			out += ","
		}
		out += `{"day_no":` + itoa(d["day_no"].(int)) +
			`,"status":` + itoa(d["status"].(int)) +
			`,"is_today":` + boolstr(d["is_today"].(bool)) +
			`,"points":25}`
	}
	return out
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	s := ""
	for i > 0 {
		s = string(rune('0'+i%10)) + s
		i /= 10
	}
	return s
}

func boolstr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func testAccountClient(t *testing.T, srv *httptest.Server) *account.Client {
	t.Helper()
	up := upstream.New(upstream.Options{Timeout: 5 * time.Second})
	c := account.New(up)
	c.SetBase(srv.URL, srv.URL)
	return c
}

func TestAlreadyCheckedIn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/minimax-cloud/api/v1/signin/status" {
			w.Write([]byte(panel(3, true)))
			return
		}
		t.Errorf("unexpected path %s", r.URL.Path)
	}))
	defer srv.Close()

	c := New(testAccountClient(t, srv))
	out, err := c.Run(context.Background(), "tok", "1234567890")
	if err != nil {
		t.Fatal(err)
	}
	if out.Outcome != "done" {
		t.Fatalf("outcome = %q", out.Outcome)
	}
	if out.Streak != 4 {
		t.Fatalf("streak = %d, want 4", out.Streak)
	}
}

func TestClaim(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/minimax-cloud/api/v1/signin/status":
			w.Write([]byte(panel(2, true)))
		case "/minimax-cloud/api/v1/signin/claim":
			w.Write([]byte(`{"data":{"claim_result":1,"points":25}}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	c := New(testAccountClient(t, srv))
	out, err := c.Run(context.Background(), "tok", "1234567890")
	if err != nil {
		t.Fatal(err)
	}
	if out.Outcome != "claimed" {
		t.Fatalf("outcome = %q", out.Outcome)
	}
	if out.Credit != 25 {
		t.Fatalf("credit = %v", out.Credit)
	}
	if out.Streak != 4 {
		t.Fatalf("streak = %d, want 4", out.Streak)
	}
}

func TestClaimAlreadyDoneToday(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/minimax-cloud/api/v1/signin/status":
			w.Write([]byte(panel(2, true)))
		case "/minimax-cloud/api/v1/signin/claim":
			w.Write([]byte(`{"data":{"claim_result":2,"points":25}}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	c := New(testAccountClient(t, srv))
	out, err := c.Run(context.Background(), "tok", "1234567890")
	if err != nil {
		t.Fatal(err)
	}
	if out.Outcome != "done" {
		t.Fatalf("outcome = %q", out.Outcome)
	}
}

func TestIneligible(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(panel(4, true)))
	}))
	defer srv.Close()

	c := New(testAccountClient(t, srv))
	out, err := c.Run(context.Background(), "tok", "1234567890")
	if err != nil {
		t.Fatal(err)
	}
	if out.Outcome != "ineligible" {
		t.Fatalf("outcome = %q", out.Outcome)
	}
}

func TestInactive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(panel(1, true)))
	}))
	defer srv.Close()

	c := New(testAccountClient(t, srv))
	out, err := c.Run(context.Background(), "tok", "1234567890")
	if err != nil {
		t.Fatal(err)
	}
	if out.Outcome != "inactive" {
		t.Fatalf("outcome = %q", out.Outcome)
	}
}

func TestMalformedPanel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":{"days":[{"day_no":1,"status":3}]}}`))
	}))
	defer srv.Close()

	c := New(testAccountClient(t, srv))
	if _, err := c.Run(context.Background(), "tok", "1234567890"); err == nil {
		t.Fatal("expected an error")
	}
}

func TestNoPanel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":{}}`))
	}))
	defer srv.Close()

	c := New(testAccountClient(t, srv))
	if _, err := c.Run(context.Background(), "tok", "1234567890"); err == nil {
		t.Fatal("expected an error")
	}
}

// TestRefusedByHTTP says a 401 is a failed check-in, not an error.
//
// 401 是“签到被拒”，是一个结果，不是一个意外。
func TestRefusedByHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"invalid access token"}`))
	}))
	defer srv.Close()

	c := New(testAccountClient(t, srv))
	out, err := c.Run(context.Background(), "tok", "1234567890")
	if err != nil {
		t.Fatal(err)
	}
	if out.Outcome != OutcomeFailed {
		t.Fatalf("outcome = %q, want %q", out.Outcome, OutcomeFailed)
	}
}

// TestRefusedByBusinessCode says MiniMax's own refusal is the same event.
//
// 200 的回答里带 1000048，和 401 是一件事：签到被拒。
func TestRefusedByBusinessCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"statusInfo":{"code":1000048,"message":"the sign-in was refused"}}`))
	}))
	defer srv.Close()

	c := New(testAccountClient(t, srv))
	out, err := c.Run(context.Background(), "tok", "1234567890")
	if err != nil {
		t.Fatal(err)
	}
	if out.Outcome != OutcomeFailed {
		t.Fatalf("outcome = %q, want %q", out.Outcome, OutcomeFailed)
	}
}

// TestUnknownClaimResult says a claim that says nothing is not a claim.
//
// 回答里没有 claim_result 时，不能当成“已经签到过了”——那会把一次
// 没有发生的签到记成完成了。
func TestUnknownClaimResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/minimax-cloud/api/v1/signin/status":
			w.Write([]byte(panel(2, true)))
		case "/minimax-cloud/api/v1/signin/claim":
			w.Write([]byte(`{"data":{}}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	c := New(testAccountClient(t, srv))
	out, err := c.Run(context.Background(), "tok", "1234567890")
	if err == nil {
		t.Fatalf("outcome = %+v, want an error", out)
	}
	if !strings.Contains(err.Error(), "claim_result") {
		t.Fatalf("error = %q", err.Error())
	}
}
