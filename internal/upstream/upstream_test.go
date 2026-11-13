package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestFollowsSameHostRedirect(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/end", http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("/end", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("arrived"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := New(Options{Timeout: 5 * time.Second})
	resp, err := c.Get(context.Background(), srv.URL+"/start")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "arrived" {
		t.Fatalf("body = %q", body)
	}
}

func TestSameHostRedirectKeepsTheBody(t *testing.T) {
	// 307 之后重放，body 必须还在。签名是按 body 算的，body 没了，
	// 上游看到的就是一个签错的请求。
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		if len(bodies) == 1 {
			w.Header().Set("Location", "/again")
			w.WriteHeader(http.StatusTemporaryRedirect)
			return
		}
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	c := New(Options{Timeout: 5 * time.Second})
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/start", strings.NewReader(`{"a":1}`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if len(bodies) != 2 {
		t.Fatalf("got %d requests, want 2: %v", len(bodies), bodies)
	}
	if bodies[0] != bodies[1] {
		t.Fatalf("the replayed body differs: %q vs %q", bodies[0], bodies[1])
	}
	if bodies[1] != `{"a":1}` {
		t.Fatalf("the replayed body = %q", bodies[1])
	}
}

func TestRefusesCrossHostRedirect(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("other"))
	}))
	defer other.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/x", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()

	c := New(Options{Timeout: 5 * time.Second})
	_, err := c.Get(context.Background(), srv.URL+"/start")
	if err == nil {
		t.Fatal("expected an error")
	}
	var oe *OtherSiteError
	if !errors.As(err, &oe) {
		t.Fatalf("err = %v (%T), want OtherSiteError", err, err)
	}
	if oe.Status != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d", oe.Status)
	}
}

func TestCheckEnvelopeBusinessError(t *testing.T) {
	// HTTP 200 but the body says it failed.
	raw := []byte(`{"statusInfo":{"code":1000048,"message":"the sign-in was refused"}}`)
	err := CheckEnvelope("check-in", raw)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "refused") {
		t.Fatalf("err = %v", err)
	}
}

func TestCheckEnvelopeBaseResp(t *testing.T) {
	raw := []byte(`{"base_resp":{"status_code":1001,"status_msg":"nope"}}`)
	err := CheckEnvelope("credits", raw)
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("err = %v", err)
	}
}

func TestCheckEnvelopeOK(t *testing.T) {
	raw := []byte(`{"base_resp":{"status_code":0},"statusInfo":{"code":0},"data":{}}`)
	if err := CheckEnvelope("credits", raw); err != nil {
		t.Fatal(err)
	}
}

func TestCheckEnvelopeNotJSON(t *testing.T) {
	if err := CheckEnvelope("credits", []byte("<html>")); err == nil {
		t.Fatal("expected an error")
	}
}

func TestQuotaRefusalDetection(t *testing.T) {
	cases := map[string]bool{
		`{"error":{"message":"insufficient balance"}}`: true,
		`{"message":"额度不足"}`:                           true,
		`{"message":"积分已用完"}`:                          true,
		`{"message":"model not available"}`:            false,
		`{"message":"quota exceeded"}`:                 true,
		`{"message":"rate limit exceeded"}`:            false,
		`{"message":"too many requests"}`:              false,
	}
	for body, want := range cases {
		if got := IsQuotaRefusal([]byte(body)); got != want {
			t.Errorf("IsQuotaRefusal(%s) = %v, want %v", body, got, want)
		}
	}
}
