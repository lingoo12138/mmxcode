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

func TestChatSendsTheRightHeaders(t *testing.T) {
	var got http.Header
	var gotURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		gotURL = r.URL.String()
		w.Write([]byte(`{"type":"message","content":[]}`))
	}))
	defer srv.Close()

	c := New(Options{Timeout: 5 * time.Second})
	req := ChatRequest{
		BaseURL:   srv.URL,
		Path:      "/mavis/api/v1/llm/v1/messages",
		Access:    "acc-token",
		SessionID: "sess-1",
		TZOffset:  -480,
		Body:      []byte(`{"model":"MiniMax-M3"}`),
		Stream:    false,
	}
	resp, err := c.Chat(context.Background(), req, nil)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if !strings.HasSuffix(gotURL, "/mavis/api/v1/llm/v1/messages") {
		t.Fatalf("url = %s", gotURL)
	}
	if got.Get("Authorization") != "Bearer acc-token" {
		t.Fatalf("authorization = %q", got.Get("Authorization"))
	}
	if got.Get("x-api-key") != "sk-xxx" {
		t.Fatalf("x-api-key = %q", got.Get("x-api-key"))
	}
	if got.Get("User-Agent") != "MiniMaxAgent" {
		t.Fatalf("user-agent = %q", got.Get("User-Agent"))
	}
	if got.Get("X-Mavis-Agent-Id") != "main" {
		t.Fatalf("agent id = %q", got.Get("X-Mavis-Agent-Id"))
	}
	if got.Get("X-Mavis-Session-Id") != "sess-1" {
		t.Fatalf("session id = %q", got.Get("X-Mavis-Session-Id"))
	}
	if got.Get("X-Mavis-Timezone-Offset") != "-480" {
		t.Fatalf("tz = %q", got.Get("X-Mavis-Timezone-Offset"))
	}
	if got.Get("accept-encoding") != "identity" {
		t.Fatalf("accept-encoding = %q, want identity", got.Get("accept-encoding"))
	}
	if got.Get("anthropic-version") != "2023-06-01" {
		t.Fatalf("anthropic-version = %q", got.Get("anthropic-version"))
	}
}

func TestChatKeepsTheClientAnthropicVersion(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := New(Options{Timeout: 5 * time.Second})
	req := ChatRequest{
		BaseURL: srv.URL, Path: "/mavis/api/v1/llm/v1/messages",
		Access: "a", Body: []byte(`{}`),
		Extra: map[string]string{"anthropic-version": "2099-01-01"},
	}
	resp, err := c.Chat(context.Background(), req, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got.Get("anthropic-version") != "2099-01-01" {
		t.Fatalf("anthropic-version = %q", got.Get("anthropic-version"))
	}
}

func TestChatSendsTheBody(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := New(Options{Timeout: 5 * time.Second})
	req := ChatRequest{
		BaseURL: srv.URL, Path: "/p", Access: "a",
		Body: []byte(`{"model":"x"}`),
	}
	resp, err := c.Chat(context.Background(), req, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if body != `{"model":"x"}` {
		t.Fatalf("body = %q", body)
	}
}

func TestChatUsesTheGivenTransport(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := New(Options{Timeout: 5 * time.Second})
	tr := &http.Transport{}
	req := ChatRequest{BaseURL: srv.URL, Path: "/p", Access: "a", Body: []byte(`{}`)}
	resp, err := c.Chat(context.Background(), req, tr)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func TestChatReportsOtherSite(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	}))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/x", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()

	c := New(Options{Timeout: 5 * time.Second})
	req := ChatRequest{BaseURL: srv.URL, Path: "/p", Access: "a", Body: []byte(`{}`)}
	_, err := c.Chat(context.Background(), req, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	var oe *OtherSiteError
	if !errors.As(err, &oe) {
		t.Fatalf("err = %v (%T)", err, err)
	}
}
