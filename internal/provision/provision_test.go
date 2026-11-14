package provision

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestTheListIsRead(t *testing.T) {
	list := `
# 这是注释
a@example.com|password

b@example.com|pass|with|bar
`
	entries, bad := Parse(strings.NewReader(list))
	if len(entries) != 2 {
		t.Fatalf("got %d entries: %+v", len(entries), entries)
	}
	if entries[0].Email != "a@example.com" || entries[0].Password != "password" {
		t.Fatalf("entry = %+v", entries[0])
	}
	if entries[1].Password != "pass|with|bar" {
		t.Fatalf("the password kept only its first part: %q", entries[1].Password)
	}
	if len(bad) != 0 {
		t.Fatalf("got %d bad lines: %+v", len(bad), bad)
	}
}

func TestALineThatIsNotAnAccount(t *testing.T) {
	entries, bad := Parse(strings.NewReader("not-an-email\n"))
	if len(entries) != 0 {
		t.Fatalf("got %d entries", len(entries))
	}
	if len(bad) != 1 {
		t.Fatalf("got %d bad lines", len(bad))
	}
	if bad[0].Line != 1 {
		t.Errorf("the line is %d, want 1", bad[0].Line)
	}
}

func TestALineWithNoPassword(t *testing.T) {
	entries, bad := Parse(strings.NewReader("a@example.com\n"))
	if len(entries) != 0 || len(bad) != 1 {
		t.Fatalf("entries = %+v, bad = %+v", entries, bad)
	}
}

func TestTheSameEmailTwiceIsOnlyOneAccount(t *testing.T) {
	entries, bad := Parse(strings.NewReader("a@example.com|p\na@example.com|q\n"))
	if len(entries) != 1 {
		t.Fatalf("got %d entries: %+v", len(entries), entries)
	}
	if len(bad) != 1 {
		t.Fatalf("got %d bad lines: %+v", len(bad), bad)
	}
	if bad[0].Line != 2 {
		t.Errorf("the line is %d, want 2", bad[0].Line)
	}
}

func TestThePasswordIsNotInTheBadLine(t *testing.T) {
	entries, bad := Parse(strings.NewReader("not-an-email|secret\nb@example.com|\n"))
	if len(entries) != 0 {
		t.Fatalf("got %d entries: %+v", len(entries), entries)
	}
	if len(bad) != 2 {
		t.Fatalf("got %d bad lines: %+v", len(bad), bad)
	}
	for _, b := range bad {
		if strings.Contains(b.Text, "secret") {
			t.Fatalf("the password is in the bad line: %+v", b)
		}
	}
}

func TestTheRunnerTakesThemOneByOne(t *testing.T) {
	var seen []string
	r := &Runner{
		Importer: &Importer{
			Import: func(ctx context.Context, email, password string) (Result, error) {
				seen = append(seen, email)
				return Result{Email: email, OK: true, AccountID: "acc_" + email}, nil
			},
		},
		Interval: time.Millisecond,
		Sleep:    func(context.Context, time.Duration) error { return nil },
	}
	out, err := r.Run(context.Background(), []Entry{
		{Email: "a@example.com", Password: "p"},
		{Email: "b@example.com", Password: "p"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || !out[0].OK || !out[1].OK {
		t.Fatalf("out = %+v", out)
	}
	if len(seen) != 2 || seen[0] != "a@example.com" || seen[1] != "b@example.com" {
		t.Fatalf("seen = %v", seen)
	}
}

func TestOneBadAccountDoesNotStopTheRest(t *testing.T) {
	r := &Runner{
		Importer: &Importer{
			Import: func(ctx context.Context, email, password string) (Result, error) {
				if email == "bad@example.com" {
					return Result{Email: email}, &Error{Stage: StagePassword, Code: 1200046, Message: "wrong password"}
				}
				return Result{Email: email, OK: true}, nil
			},
		},
		Sleep: func(context.Context, time.Duration) error { return nil },
	}
	out, err := r.Run(context.Background(), []Entry{
		{Email: "bad@example.com", Password: "p"},
		{Email: "good@example.com", Password: "p"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("out = %+v", out)
	}
	if out[0].OK || out[0].Stage != StagePassword || out[0].Code != 1200046 {
		t.Fatalf("the first one = %+v", out[0])
	}
	if !out[1].OK {
		t.Fatalf("the second one = %+v", out[1])
	}
}

func TestANetworkErrorIsTriedAgain(t *testing.T) {
	var tries int
	r := &Runner{
		Importer: &Importer{
			Import: func(ctx context.Context, email, password string) (Result, error) {
				tries++
				if tries < 3 {
					return Result{Email: email}, &Error{Stage: StageToken, Message: "connection reset", Retry: true, Wait: time.Millisecond}
				}
				return Result{Email: email, OK: true}, nil
			},
		},
		Sleep: func(context.Context, time.Duration) error { return nil },
	}
	out, err := r.Run(context.Background(), []Entry{{Email: "a@example.com", Password: "p"}})
	if err != nil {
		t.Fatal(err)
	}
	if tries != 3 {
		t.Fatalf("tried %d times, want 3", tries)
	}
	if !out[0].OK {
		t.Fatalf("out = %+v", out)
	}
}

func TestAWrongPasswordIsNotTriedAgain(t *testing.T) {
	var tries int
	r := &Runner{
		Importer: &Importer{
			Import: func(ctx context.Context, email, password string) (Result, error) {
				tries++
				return Result{Email: email}, &Error{Stage: StagePassword, Code: 1200046}
			},
		},
		Sleep: func(context.Context, time.Duration) error { return nil },
	}
	if _, err := r.Run(context.Background(), []Entry{{Email: "a@example.com", Password: "p"}}); err != nil {
		t.Fatal(err)
	}
	if tries != 1 {
		t.Fatalf("tried %d times, want 1", tries)
	}
}

func TestTheTriesAreNotEndless(t *testing.T) {
	var tries int
	r := &Runner{
		Importer: &Importer{
			Import: func(ctx context.Context, email, password string) (Result, error) {
				tries++
				return Result{Email: email}, &Error{Stage: StageToken, Code: 1200007, Message: "not logged in", Retry: true}
			},
		},
		Sleep: func(context.Context, time.Duration) error { return nil },
	}
	out, err := r.Run(context.Background(), []Entry{{Email: "a@example.com", Password: "p"}})
	if err != nil {
		t.Fatal(err)
	}
	if tries != 3 {
		t.Fatalf("tried %d times, want 3", tries)
	}
	if out[0].OK {
		t.Fatalf("out = %+v", out[0])
	}
	if out[0].Code != 1200007 {
		t.Fatalf("the site's own code is gone: %+v", out[0])
	}
}

func TestTheRunStopsWhenTheContextIsDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := &Runner{
		Importer: &Importer{
			Import: func(ctx context.Context, email, password string) (Result, error) {
				return Result{}, errors.New("should not be called")
			},
		},
	}
	if _, err := r.Run(ctx, []Entry{{Email: "a@example.com"}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the cancellation", err)
	}
}

func TestTheRunStopsInTheMiddleOfAnAccount(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := &Runner{
		Importer: &Importer{
			Import: func(ctx context.Context, email, password string) (Result, error) {
				cancel()
				return Result{Email: email}, &Error{Stage: StageToken, Message: "connection reset", Retry: true, Wait: time.Millisecond}
			},
		},
		Sleep: func(ctx context.Context, d time.Duration) error { return ctx.Err() },
	}
	if _, err := r.Run(ctx, []Entry{{Email: "a@example.com"}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the cancellation", err)
	}
}

func TestTheResultsAreTheTextTheDatabaseKeeps(t *testing.T) {
	raw := ResultsJSON([]Result{{
		Email:   "a@example.com",
		OK:      false,
		Stage:   StagePassword,
		Code:    1200046,
		Message: "wrong password",
	}})
	for _, want := range []string{`"email":"a@example.com"`, `"ok":false`, `"stage":"password"`, `"code":1200046`} {
		if !strings.Contains(raw, want) {
			t.Fatalf("%s is not in %s", want, raw)
		}
	}
}

func TestTheWaitsAreTheOnesTheDesignSays(t *testing.T) {
	// 设计文档 §5.3：网络错误、5xx、超时退避重试 2 次（1s、4s）。
	var waits []time.Duration
	r := &Runner{
		Importer: &Importer{
			Import: func(ctx context.Context, email, password string) (Result, error) {
				return Result{Email: email}, &Error{Stage: StageToken, Message: "connection reset", Retry: true}
			},
		},
		Sleep: func(ctx context.Context, d time.Duration) error {
			waits = append(waits, d)
			return nil
		},
	}
	if _, err := r.Run(context.Background(), []Entry{{Email: "a@example.com", Password: "p"}}); err != nil {
		t.Fatal(err)
	}
	if len(waits) != 2 || waits[0] != time.Second || waits[1] != 4*time.Second {
		t.Fatalf("waits = %v, want 1s then 4s", waits)
	}
}

func TestASiteThatIsLimitingIsTriedOnlyOnceMore(t *testing.T) {
	// 429：退避 30s 重试 1 次。
	var tries int
	r := &Runner{
		Importer: &Importer{
			Import: func(ctx context.Context, email, password string) (Result, error) {
				tries++
				return Result{Email: email}, &Error{Stage: StageToken, Message: "too many requests",
					Retry: true, Wait: time.Millisecond, Tries: 2}
			},
		},
		Sleep: func(context.Context, time.Duration) error { return nil },
	}
	out, err := r.Run(context.Background(), []Entry{{Email: "a@example.com", Password: "p"}})
	if err != nil {
		t.Fatal(err)
	}
	if tries != 2 {
		t.Fatalf("tried %d times, want 2", tries)
	}
	if out[0].OK {
		t.Fatalf("out = %+v", out[0])
	}
}
