package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"

	"github.com/lingoo12138/mmxcode/internal/rotator"
)

// TestTheBodyIsReadIntoOneBuffer says that reading a body does not
// double it.
//
// ReadAll 一路翻倍着长大：16 MiB 的身体它要分配 32 MiB，翻倍的那一刻两份
// 都在 —— 那 32 MiB 就是峰值，而身体本身只有 16 MiB。
func TestTheBodyIsReadIntoOneBuffer(t *testing.T) {
	const size = 16 << 20
	payload := make([]byte, size)
	for i := range payload {
		payload[i] = 'x'
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(payload)))
	w := httptest.NewRecorder()

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	body, err := readBody(w, r)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != size {
		t.Fatalf("read %d bytes, want %d", len(body), size)
	}
	used := after.TotalAlloc - before.TotalAlloc
	if used > size+size/4 {
		t.Fatalf("reading a %d byte body allocated %d bytes", size, used)
	}
}

// TestTheImageIsNotCopied says that the picture in a body is not copied.
//
// 抄一份是几十兆的白抄：抄完只是为了知道「这段里没有文字」。
func TestTheImageIsNotCopied(t *testing.T) {
	const size = 16 << 20
	picture := strings.Repeat("A", size)
	body := []byte(`{"model":"m","messages":[{"role":"user","content":[` +
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` +
		picture + `"},"type":"text","text":"what is this?"}]}]}`)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	peek := peekOf(body)
	runtime.ReadMemStats(&after)

	if got := peek.System.text; len(got) != 0 {
		t.Fatalf("the system prompt is %d bytes, want none", len(got))
	}
	if got := string(firstUserOfPeek(peek)); got != "what is this?" {
		t.Fatalf("the first user message is %q", got)
	}
	used := after.TotalAlloc - before.TotalAlloc
	if used > 1<<20 {
		t.Fatalf("reading a body with a %d byte picture allocated %d bytes", size, used)
	}
}

// TestTheWholeConversationIsNotCopied says that the messages that are
// not the first one are not copied either.
//
// 一段对话里，第一条用户消息以外的那些话不用留：它们只是路过。
func TestTheWholeConversationIsNotCopied(t *testing.T) {
	const size = 16 << 20
	talk := strings.Repeat("y", size)
	body := []byte(`{"model":"m","messages":[` +
		`{"role":"user","content":"first"},` +
		`{"role":"assistant","content":"` + talk + `"}]}`)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	peek := peekOf(body)
	runtime.ReadMemStats(&after)

	if got := string(firstUserOfPeek(peek)); got != "first" {
		t.Fatalf("the first user message is %q", got)
	}
	used := after.TotalAlloc - before.TotalAlloc
	if used > 1<<20 {
		t.Fatalf("reading a body with a %d byte message allocated %d bytes", size, used)
	}
}

// TestTheSessionHashesTheWords says that the session is a hash of the
// words, not of the bytes they came in.
//
// 身体里的字节是转义过的（换行是 \n 两个字节），哈希的那一份是转义之前的
// 那些字：不然同一个会话两次进来会算成两个。
func TestTheSessionHashesTheWords(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"one\ntwo"}]}`)
	peek := peekOf(body)

	got := rotator.SessionOfBytes(nil, nil, firstUserOfPeek(peek)).UpstreamID
	sum := sha256.Sum256([]byte("\x00one\ntwo"))
	want := hex.EncodeToString(sum[:])[:32]
	if got != want {
		t.Fatalf("the session hash is %s, want %s", got, want)
	}
}

// TestTheSessionIsTheSameWithAndWithoutEscapes says that the words and
// the bytes of the same words hash the same.
//
// 转义过的写法算的是同一段字：一个会话改了写法，还是那个会话。
func TestTheSessionIsTheSameWithAndWithoutEscapes(t *testing.T) {
	plain := rotator.SessionOfBytes(nil, nil, []byte("one\ntwo")).UpstreamID
	escaped := rotator.SessionOfBytes(nil, nil, []byte("one two")).UpstreamID
	if plain == escaped {
		t.Fatal("two different conversations hashed the same")
	}
	body := []byte(`{"messages":[{"role":"user","content":"one\ntwo"}]}`)
	peek := peekOf(body)
	if string(firstUserOfPeek(peek)) != "one\ntwo" {
		t.Fatalf("the first user message is %q", firstUserOfPeek(peek))
	}
}
