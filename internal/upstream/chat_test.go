package upstream

import (
	"bytes"
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

// TestChatSendsTheBodyAsItIs says the body arrives at the upstream byte
// for byte, whatever is in it.
//
// 往上游发的时候身体是不该被抄一遍的（64 MiB 的身体抄一遍就是多 64 MiB
// 的内存）。抄的那份跟原来那份在行为上是一模一样的 —— 所以这里守的不是
// 「不抄」，守的是「抄了也别把身体改坏」。
func TestChatSendsTheBodyAsItIs(t *testing.T) {
	want := []byte{0x00, 0xff, 0xfe, '"', '\\', 0x00, 'a'}
	var got []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := New(Options{Timeout: 5 * time.Second})
	resp, err := c.Chat(context.Background(), ChatRequest{
		BaseURL: srv.URL, Path: "/p", Access: "a", Body: want,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !bytes.Equal(got, want) {
		t.Fatalf("the body = %v, want %v", got, want)
	}
}

// discardTransport answers every request without touching the network.
//
// 它是那个基准的地面：身体走的是它，不是真的网 —— 基准量的是网关自己
// 那一份，不是网络快慢。
type discardTransport struct{}

func (discardTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	io.Copy(io.Discard, r.Body)
	return &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader(`{}`)),
		Header:     make(http.Header),
		Request:    r,
	}, nil
}

// BenchmarkChatSendsABigBody says what a big body costs on the way to
// the upstream.
//
// 身体是 4 MB（1M token 的对话）：往上游发的时候身体不该被抄一遍 ——
// 抄一遍就是多一份 4 MB 的分配，也是多 4 MB 的内存（在飞的时候）。
func BenchmarkChatSendsABigBody(b *testing.B) {
	body := bytes.Repeat([]byte("x"), 4<<20)
	c := New(Options{Timeout: 30 * time.Second})
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		resp, err := c.Chat(context.Background(), ChatRequest{
			BaseURL: "http://upstream.invalid", Path: "/p", Access: "a", Body: body,
		}, discardTransport{})
		if err != nil {
			b.Fatal(err)
		}
		resp.Body.Close()
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
