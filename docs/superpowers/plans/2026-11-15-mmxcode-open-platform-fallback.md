# 开放平台说「没有这条路」也要换条路 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** magpie 用 OpenAI 那套协议问网关时不再拿到 503：开放平台答的 `direct_route_not_configured` 要被当成「这条路不通」而不是「站点坏了」。

**Architecture:** `internal/gateway` 里那扇门后头有一条换路的逻辑：上游答 404 时，网关改成 Anthropic 那条路（本地转格式）用同一个账号再问一次。开放平台对 MiniMax Code 的账号答的是 503 加一句 `direct_route_not_configured` —— 说的是同一件事，只是换了Status码和一句人话。把它认出来，走同一条换路的逻辑。

**Tech Stack:** Go 1.26、`net/http`、`encoding/json`

---

## 三条必须守住的不变量

1. **503 不等于站点坏了。** 开放平台对 MiniMax Code 的账号在 `/chat/completions`、`/responses` 上答的是 `503 {"errorCode":50115,"errorReason":"direct_route_not_configured"}`：不是这条路对这个账号不通。那就得换路，不是换号。
2. **换的是路，不是号。** 换路之后还是同一个账号、同一把令牌：不消耗 refresh token，不在账号之间乱跳。
3. **认得出，也认不错。** 只认那一句明确的话（`errorCode 50115` 或 `direct_route_not_configured`）。别的 503 还是 503 —— 站点真的坏了的时候要照实说。

### Task 1: 认得出「没有这条路」

**Files:**
- Modify: `internal/gateway/gateway.go`
- Modify: `internal/gateway/openai_test.go`

- [ ] **Step 1: 写失败的测试**

在 `internal/gateway/openai_test.go` 里加：

```go
// TestTheOpenAIRequestFallsBackWhenTheOpenPlatformIsNotConfigured says a
// 503 from the open platform is "this account has no route there", not a
// failure to hand to the client.
//
// MiniMax 的开放平台是给 API key 用的：MiniMax Code 的账号在那条路上没有
// 配好的路由，站点答的是 503 和 direct_route_not_configured。这不是站点
// 坏了，是这条路对这个账号不通 —— 跟"没有这条路"是一回事，该走 Anthropic
// 那条。
func TestTheOpenAIRequestFallsBackWhenTheOpenPlatformIsNotConfigured(t *testing.T) {
	rec := &upstreamRecorder{}
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		if r.URL.Path == site.ChatCompletionsPath {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"error":"open platform service unavailable, please retry",` +
				`"errorCode":50115,"errorReason":"direct_route_not_configured"}`))
			return
		}
		w.Write([]byte(`{"type":"message","id":"msg_1","content":[{"type":"text","text":"hi"}]}`))
	})

	body := `{"model":"m","max_completion_tokens":32,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	paths, _ := rec.seen()
	if len(paths) != 2 {
		t.Fatalf("the upstream was asked %d times: %v", len(paths), paths)
	}
	if paths[1] != site.MessagesPath {
		t.Fatalf("the second path = %q, want the Anthropic one", paths[1])
	}
	// 这一趟把这个账号的这条路关掉了，后面的请求不再去撞它。
	if h.gw.NativeChatCompletions.Load() {
		t.Fatal("the native path is still on")
	}
}

// TestTheCodexRequestFallsBackWhenTheOpenPlatformIsNotConfigured says the
// same for the Responses protocol.
//
// Codex 客户端走的是 /v1/responses，那条路在开放平台上也不通。
func TestTheCodexRequestFallsBackWhenTheOpenPlatformIsNotConfigured(t *testing.T) {
	rec := &upstreamRecorder{}
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		if r.URL.Path == site.ResponsesPath {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"error":"open platform service unavailable, please retry",` +
				`"errorCode":50115,"errorReason":"direct_route_not_configured"}`))
			return
		}
		w.Write([]byte(`{"type":"message","id":"msg_1","content":[{"type":"text","text":"hi"}]}`))
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m","input":"hi"}`))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	paths, _ := rec.seen()
	if len(paths) != 2 {
		t.Fatalf("the upstream was asked %d times: %v", len(paths), paths)
	}
	if paths[1] != site.MessagesPath {
		t.Fatalf("the second path = %q, want the Anthropic one", paths[1])
	}
	if h.gw.NativeResponses.Load() {
		t.Fatal("the native path is still on")
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

```
go test ./internal/gateway/ -run "NotConfigured"
```

失败：网关把上游的 503 原样给了客户端（`code = 503`）。

- [ ] **Step 3: 认出那一句**

`internal/gateway/gateway.go` 加一个认得那一句的函数：

```go
// openPlatformNotConfigured says the upstream's answer is "the open
// platform has no route for this account".
//
// MiniMax 的开放平台是给 API key 用的：MiniMax Code 的账号在那条路上没
// 有配好的路由，站点答的是 503、errorCode 50115、direct_route_not_configured。
// 这不是站点坏了，是这条路对这个账号不通：跟「没有这条路」是一回事。
func openPlatformNotConfigured(raw []byte) bool {
	if len(raw) == 0 {
		return false
	}
	var env struct {
		ErrorCode   int    `json:"errorCode"`
		ErrorReason string `json:"errorReason"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return false
	}
	return env.ErrorCode == 50115 || env.ErrorReason == "direct_route_not_configured"
}
```

- [ ] **Step 4: 在换路的地方用上它**

`internal/gateway/gateway.go` 里换路的那一段：

```go
// 上游没有这条路：改用本地转换，同一个账号再来一次。
//
// 开放平台对 MiniMax Code 的账号答的也是「没有这条路」：503
// 加一句 direct_route_not_configured。那不是站点坏了，是那条
// 路对这个账号不通 —— 该走 Anthropic 那条。
if resp.StatusCode == http.StatusNotFound || openPlatformNotConfigured(raw) {
```

- [ ] **Step 5: 运行测试确认通过**

```
go test ./internal/gateway/
```

- [ ] **Step 6: 提交**

```
git add internal/gateway
git commit -m "fix(gateway): 开放平台说没有这条路的时候换条路"
```

### Task 2: 文档

**Files:**
- Modify: `README.md`

- [ ] **Step 1: 写清楚这一种 503**

README 里加一条：还有一种「没有这条路」—— 开放平台对 MiniMax Code 的账号答 `503 direct_route_not_configured`，说的是那条路对这个账号不通，网关会自己换到 Anthropic 那条路再问一次。

- [ ] **Step 2: 提交**

```
git add README.md
git commit -m "docs(readme): 还有一种没有这条路"
```

