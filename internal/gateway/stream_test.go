package gateway

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestStreamIsRelayedVerbatim says the Anthropic stream is the
// upstream's own stream.
//
// Anthropic 那一门不转：上游发什么，客户端收什么，一个字节都不差。
// 用量（usage）就在里面 —— 上游开头说进去多少，末尾说出来多少。
//
// 这一条是「usage 有没有」的底线：门这一头不转，上游给了的用量就一定
// 到得了客户端。
func TestStreamIsRelayedVerbatim(t *testing.T) {
	up := "event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"m\",\"usage\":{\"input_tokens\":7,\"output_tokens\":0}}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
		"event: message_delta\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":11}}\n\n" +
		"event: message_stop\n" +
		"data: {\"type\":\"message_stop\"}\n\n"
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(up))
	})

	srv := httptest.NewServer(h.gw)
	defer srv.Close()

	body := `{"model":"m","stream":true,"max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`
	res, err := http.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	got, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != up {
		t.Fatalf("the stream is not the upstream's own:\ngot:  %q\nwant: %q", string(got), up)
	}
}

// TestStreamGoesOutAsItComes says a stream is a stream.
//
// 上游来一段，客户端就该收到一段 —— 不是等整段回答写完了一次给过去。
//
// Go 的 HTTP 服务自己攒着 2048 字节才往 socket 上写（响应头也在里头）。
// 一段流的头几百字节，谁都在攒着：客户端连响应头都要等整个答案写完
// 才收到，那还叫流式吗？流式的意义就是第一个字早一点到。
//
// 最后那一块带着用量（usage）。它最后才到 —— 攒着的时候客户端根本等不
// 到它，于是「这趟用了多少」就成了没有。
func TestStreamGoesOutAsItComes(t *testing.T) {
	// 上游写完第一段就停在那儿，等客户端收到才继续。
	release := make(chan struct{})
	upstreamDone := make(chan struct{})
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\"}\n\n"))
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-time.After(3 * time.Second):
			t.Error("the client never got the first event")
		}
		w.Write([]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":5}}\n\n"))
		close(upstreamDone)
	})

	srv := httptest.NewServer(h.gw)
	defer srv.Close()

	body := `{"model":"m","stream":true,"max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`
	res, err := http.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}

	br := bufio.NewReader(res.Body)
	first := make(chan string, 1)
	go func() {
		line, err := br.ReadString('\n')
		if err != nil {
			first <- ""
			return
		}
		first <- line
	}()

	var line string
	select {
	case line = <-first:
	case <-time.After(2 * time.Second):
		t.Fatal("the first event did not come out before the upstream was done")
	}
	if !strings.Contains(line, "message_start") {
		t.Fatalf("the first line = %q", line)
	}

	// 上游还在那儿卡着：它还没写第二段，客户端已经拿到第一段了。
	select {
	case <-upstreamDone:
		t.Fatal("the upstream was already done")
	default:
	}
	close(release)

	// 最后那一块带着用量：它得真的到得了客户端。
	rest := make(chan string, 1)
	go func() {
		var sb strings.Builder
		for {
			line, err := br.ReadString('\n')
			sb.WriteString(line)
			if err != nil {
				break
			}
		}
		rest <- sb.String()
	}()
	select {
	case got := <-rest:
		if !strings.Contains(got, "output_tokens") {
			t.Fatalf("the usage did not come out: %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the stream did not end")
	}
}
