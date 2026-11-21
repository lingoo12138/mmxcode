# 模型列表听站点的 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `GET /v1/models` 给的是站点自己那份名单 —— MiniMax 把 `MiniMax-M3.1-Flash-Preview` 改叫 `MiniMax-M3.1-Flash` 之后，客户端（magpie 之类）也要能挑到现在这个模型。

**Architecture:** `internal/gateway` 的 `modelsFor` 去站点读一次名单，读到的留着（十分钟），读不到就摆 `internal/site` 的内置名单。`internal/account` 把读 provider config 的那个函数露出来（`ParseModels`），因为站点答的就是那个形状。

**Tech Stack:** Go 1.26、`net/http`、`encoding/json`

---

## 三条必须守住的不变量

1. **名单是站点的。** 内置的那份只是兜底：它抄的时候会旧（MiniMax 改过模型的名字），站点那份不会。
2. **问的时候不带令牌。** 站点没有令牌也答；带令牌去问有可能在路上把令牌续了 —— 一份名单不值得换一把新令牌。
3. **问一次，留着。** 客户端问十回，站点被问一回。名单一年也换不了几回。

### Task 1: 内置名单补上现在那个模型

**Files:**
- Modify: `internal/site/models.go`
- Modify: `internal/site/models_test.go`

- [ ] **Step 1: 写失败的测试**

`internal/site/models_test.go` 的 `TestFallbackModelsLimits` 里加一行：

```go
func TestFallbackModelsLimits(t *testing.T) {
	type want struct {
		ctx, out int
		name     string
		inputs   []string
		thinking Thinking
		efforts  []string
	}
	wants := map[string]want{
		"MiniMax-M3.1-Flash-Preview": {1048576, 524288, "M3.1-Flash-Preview", []string{"text", "image", "video"}, ThinkingForcedOn, []string{"low", "medium", "high", "xhigh", "max"}},
		"MiniMax-M3.1-Flash":         {1048576, 524288, "M3.1-Flash", []string{"text", "image", "video"}, ThinkingForcedOn, []string{"low", "medium", "high", "xhigh", "max"}},
		"MiniMax-M3":                 {1048576, 524288, "M3", []string{"text", "image", "video"}, ThinkingSwitchable, []string{"none", "high"}},
		"MiniMax-M2.7":               {200000, 128000, "M2.7", []string{"text"}, ThinkingOn, nil},
		"MiniMax-M2.7-highspeed":     {200000, 128000, "M2.7-highspeed", []string{"text"}, ThinkingOn, nil},
	}
	models := FallbackModels()
	if len(models) != len(wants) {
		t.Fatalf("got %d models, want %d", len(models), len(wants))
	}
	for _, m := range models {
		w, ok := wants[m.ID]
		if !ok {
			t.Fatalf("unexpected model %q", m.ID)
		}
		if m.Context != w.ctx || m.Output != w.out {
			t.Fatalf("%s limits = %d/%d, want %d/%d", m.ID, m.Context, m.Output, w.ctx, w.out)
		}
		if m.Name != w.name {
			t.Fatalf("%s name = %q, want %q", m.ID, m.Name, w.name)
		}
		if strings.Join(m.Inputs, ",") != strings.Join(w.inputs, ",") {
			t.Fatalf("%s inputs = %v, want %v", m.ID, m.Inputs, w.inputs)
		}
		if m.Thinking != w.thinking {
			t.Fatalf("%s thinking = %q, want %q", m.ID, m.Thinking, w.thinking)
		}
		if strings.Join(m.Efforts, ",") != strings.Join(w.efforts, ",") {
			t.Fatalf("%s efforts = %v, want %v", m.ID, m.Efforts, w.efforts)
		}
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

```
go test ./internal/site/
```

失败：`got 4 models, want 5`。

- [ ] **Step 3: 内置名单加上它**

`internal/site/models.go`：

```go
// fallbackModels is the list used when the live one can't be had.
//
// It is package-private on purpose: a caller that gets a copy can
// change its copy, and there is no way to reach this one and change
// it for everybody.
//
// The context and output limits are the ones the endpoint really allows,
// not the ones MiniMax Code declares. Its model config says 512K/128K for
// the M3 family, but the endpoint accepts 1,048,576 input tokens and
// 524,288 output tokens for every one of them. An agent that believed the
// advertised ones would compact its history about half way too early.
var fallbackModels = []Model{
	{
		ID: "MiniMax-M3.1-Flash-Preview", Name: "M3.1-Flash-Preview",
		Context: 1048576, Output: 524288,
		Inputs:   []string{"text", "image", "video"},
		Thinking: ThinkingForcedOn,
		Efforts:  []string{"low", "medium", "high", "xhigh", "max"},
	},
	{
		// 站点现在叫这个名字：M3.1-Flash-Preview 是它以前的名字，
		// 站点自己给的那个 id 才是客户端要用的。
		ID: "MiniMax-M3.1-Flash", Name: "M3.1-Flash",
		Context: 1048576, Output: 524288,
		Inputs:   []string{"text", "image", "video"},
		Thinking: ThinkingForcedOn,
		Efforts:  []string{"low", "medium", "high", "xhigh", "max"},
	},
	{
		ID: "MiniMax-M3", Name: "M3",
		Context: 1048576, Output: 524288,
		Inputs:   []string{"text", "image", "video"},
		Thinking: ThinkingSwitchable,
		Efforts:  []string{"none", "high"},
	},
	{
		ID: "MiniMax-M2.7", Name: "M2.7",
		Context: 200000, Output: 128000,
		Inputs:   []string{"text"},
		Thinking: ThinkingOn,
	},
	{
		ID: "MiniMax-M2.7-highspeed", Name: "M2.7-highspeed",
		Context: 200000, Output: 128000,
		Inputs:   []string{"text"},
		Thinking: ThinkingOn,
	},
}
```

- [ ] **Step 4: 运行测试确认通过**

```
go test ./internal/site/
```

- [ ] **Step 5: 提交**

```
git add internal/site
git commit -m "feat(site): 内置名单补上 MiniMax-M3.1-Flash"
```

### Task 2: 把读 provider config 的那个函数露出来

**Files:**
- Modify: `internal/account/account.go`

- [ ] **Step 1: 导出读名单的函数**

站点答的是 provider config 的样子（`{providers:[{providerId,config:{models,model_order}}]}`），不是模型列表的样子。读它的那个函数本来是包内的，网关要用就得露出来：

```go
// ParseModels reads MiniMax Code's model config as models.
//
// It is the exported face of the reading: the gateway reads the site's
// own list itself, and the answer it gets is in this shape.
func ParseModels(body []byte) []site.Model { return parseModels(body) }
```

- [ ] **Step 2: 运行测试确认通过**

```
go test ./internal/account/
```

- [ ] **Step 3: 提交**

```
git add internal/account
git commit -m "feat(account): 读模型名单的函数露出来"
```

### Task 3: 网关的名单听站点的

**Files:**
- Create: `internal/gateway/models.go`
- Create: `internal/gateway/models_test.go`
- Modify: `internal/gateway/gateway.go`
- Modify: `internal/gateway/failures_test.go`

- [ ] **Step 1: 写失败的测试**

创建 `internal/gateway/models_test.go`：

```go
package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/lingoo12138/mmxcode/internal/site"
)

// modelIDs reads the ids out of a model list answer.
func modelIDs(t *testing.T, body string) []string {
	t.Helper()
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("the answer is not JSON: %v (%s)", err, body)
	}
	ids := make([]string, 0, len(out.Data))
	for _, d := range out.Data {
		ids = append(ids, d.ID)
	}
	return ids
}

// oneModelAnswer is the site's model config, the shape it really
// answers in.
const oneModelAnswer = `{"providers":[{"providerId":"minimax","config":{` +
	`"models":{"MiniMax-M3.1-Flash":{"name":"M3.1-Flash","limit":{"context":512000,"output":128000},` +
	`"modalities":{"input":["text","image","video"]},"thinking_config":{"mode":"forced_on"},` +
	`"effort_options":["default","low","medium","high","xhigh","max"]}},` +
	`"model_order":["MiniMax-M3.1-Flash"]}}]}`

// TestTheModelListIsTheSitesOwn says the list comes from the site.
//
// 网关原来给的是它自己知道的那一份 —— 那是照着参考实现抄下来的，抄的
// 时候站点上的模型还叫 MiniMax-M3.1-Flash-Preview。站点后来改了名，
// 抄的那一份就旧了：客户端照着它挑模型，挑不到现在这个。
func TestTheModelListIsTheSitesOwn(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != site.ModelsPath {
			t.Errorf("path = %s, want %s", r.URL.Path, site.ModelsPath)
			return
		}
		w.Write([]byte(oneModelAnswer))
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	ids := modelIDs(t, w.Body.String())
	if len(ids) != 1 || ids[0] != "MiniMax-M3.1-Flash" {
		t.Fatalf("ids = %v, want the site's own one", ids)
	}
}

// TestTheSiteIsAskedOnceForTheList says the list is kept for a while.
//
// 模型列表一年也换不了几回。客户端问一次就让它跑一趟 MiniMax，是把
// 一个本地的东西做成了网络调用。
func TestTheSiteIsAskedOnceForTheList(t *testing.T) {
	var asked int64
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == site.ModelsPath {
			atomic.AddInt64(&asked, 1)
		}
		w.Write([]byte(oneModelAnswer))
	})
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		w := httptest.NewRecorder()
		h.gw.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
		}
	}
	if n := atomic.LoadInt64(&asked); n != 1 {
		t.Fatalf("the site was asked %d times, want 1", n)
	}
}

// TestTheBuiltInListIsServedWhenTheSiteIsSilent says the built-in list
// is what is served when the site cannot be reached.
func TestTheBuiltInListIsServedWhenTheSiteIsSilent(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	ids := modelIDs(t, w.Body.String())
	found := false
	for _, id := range ids {
		if id == "MiniMax-M3.1-Flash" {
			found = true
		}
	}
	if !found {
		t.Fatalf("ids = %v, want the built-in one", ids)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

```
go test ./internal/gateway/ -run "ModelList|Model"
```

失败：名单是内置的那份，站点一次也没被问。

- [ ] **Step 3: 实现**

创建 `internal/gateway/models.go`：

```go
package gateway

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/lingoo12138/mmxcode/internal/account"
	"github.com/lingoo12138/mmxcode/internal/site"
)

// modelsTTL is how long the site's model list is kept.
//
// 模型的名单一年也换不了几回。客户端问一次就让它跑一趟 MiniMax，是把
// 一个本地能回答的东西做成了网络调用。
const modelsTTL = 10 * time.Minute

// modelsFallbackTTL is how long the built-in list is kept when the
// site could not be reached.
//
// 比真的那份短：站点暂时答不上来，过一会儿就该再问一次。
const modelsFallbackTTL = time.Minute

// modelsTimeout is how long the site has to answer the model list.
const modelsTimeout = 5 * time.Second

// liveModels reads the site's own model list.
//
// 问的时候不带账号的令牌：站点没有令牌也答，而带令牌去问有可能在路上
// 把令牌续了 —— 一份模型名单不值得换一把新令牌。
func (s *Server) liveModels(ctx context.Context) ([]site.Model, error) {
	if s.Mgr == nil || s.Mgr.Up == nil {
		return nil, errors.New("models: there is no upstream client")
	}
	ctx, cancel := context.WithTimeout(ctx, modelsTimeout)
	defer cancel()
	body, err := s.Mgr.Up.Models(ctx, s.UpstreamBase, "")
	if err != nil {
		return nil, err
	}
	ms := account.ParseModels(body)
	if len(ms) == 0 {
		return nil, errors.New("models: the site's answer has no models in it")
	}
	return ms, nil
}

// modelsFor gives the models the site has.
//
// 站点的那份读到了就留着，读不到就摆内置的那份：内置的那份是站点已
// 知有的东西，比一份空名单好。
func (s *Server) modelsFor(ctx context.Context) []site.Model {
	s.modelsMu.Lock()
	defer s.modelsMu.Unlock()

	if s.models != nil {
		ttl := modelsTTL
		if !s.modelsLive {
			ttl = modelsFallbackTTL
		}
		if time.Since(s.modelsAt) < ttl {
			return s.models
		}
	}

	ms, err := s.liveModels(ctx)
	if err != nil {
		log.Printf("models: the site's list could not be read: %v", err)
		s.models = site.FallbackModels()
		s.modelsLive = false
		s.modelsAt = time.Now()
		return s.models
	}
	s.models = ms
	s.modelsLive = true
	s.modelsAt = time.Now()
	return ms
}
```

`internal/gateway/gateway.go` 的 `Server` 加上留名单的地方，`handleModels` 改成用 `modelsFor`：

```go
// Server is the gateway.
type Server struct {
	// Mgr holds the accounts.
	Mgr *manager.Manager
	// Cfg is the configuration.
	Cfg config.Config
	// Store is the database.
	Store *store.Store
	// Rot picks the account.
	Rot *rotator.Rotator
	// UpstreamBase is the host the chats go to. It is a field so a
	// test can point it at a stand-in.
	UpstreamBase string
	// NativeResponses says the upstream speaks the Responses protocol
	// itself. It is tried and turned off when the upstream says no.
	//
	// 每个请求都读它，而第一个 404 会把它写掉：一个字段被一个请求写、
	// 被别的请求读，就是数据竞争。所以它是 atomic 的。
	NativeResponses atomic.Bool
	// NativeChatCompletions says the upstream speaks the OpenAI chat
	// completions protocol itself. It is tried and turned off when the
	// upstream says no.
	//
	// 参考实现只走 Anthropic 的 /messages，这条路是照着同目录下的
	// /messages 推的：站点也许有，也许没有。它和 NativeResponses 是
	// 同一根杆，同一个理由才用 atomic。
	NativeChatCompletions atomic.Bool
	// WebUI serves the management pages, when there is one.
	WebUI http.Handler
	// modelsMu guards the model list that was read.
	modelsMu sync.Mutex
	// models is the model list, the site's own or the built-in one.
	models []site.Model
	// modelsAt is when it was read.
	modelsAt time.Time
	// modelsLive says the list is the site's own, and not the built-in
	// one that is served when the site cannot be reached.
	modelsLive bool
}

// handleModels serves the model list.
//
// The list is the site's own: it is read from MiniMax and kept for a
// while, because the site's list changes a few times a year and a
// client asking for it should not be made to wait for a trip to
// MiniMax every time.
//
// 站点答不上来的时候摆的是内置的那一份：那是站点已知有的东西，比一
// 份空名单好。内置的那一份也会旧（站点改过模型的名字），所以它只
// 是兜底，不是主路。
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	data := []map[string]any{}
	for _, m := range s.modelsFor(r.Context()) {
		data = append(data, map[string]any{
			"id": m.ID, "object": "model", "owned_by": "minimax",
		})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"object": "list",
		"data":   data,
	})
}
```

`internal/gateway/failures_test.go` 里那个「模型列表不问上游」的测试要改掉：那个决定翻过来了 —— 现在问的是站点，问一次。断言改成「站点被问了一次」，其余不变。

- [ ] **Step 4: 运行测试确认通过**

```
go test ./internal/gateway/
```

- [ ] **Step 5: 提交**

```
git add internal/gateway
git commit -m "feat(gateway): 模型列表听站点的"
```

### Task 4: 文档

**Files:**
- Modify: `README.md`
- Modify: `cmd/mmxcode/root_test.go`

- [ ] **Step 1: 写清楚这个名单是怎么来的**

README 加一节「模型列表」：名单是站点自己那份（十分钟读一次、留着）、问的时候不带令牌、站点答不上来的时候摆内置的那一份（`mmxcode models` 打印的就是它）、站点现在给的是什么。

- [ ] **Step 2: 命令行那个测试跟着改**

内置名单多了一个模型，`cmd/mmxcode/root_test.go` 的 `TestModelsPrintsTheFallbackList` 里的行数从 4 改成 5。

- [ ] **Step 3: 提交**

```
git add README.md cmd/mmxcode/root_test.go
git commit -m "docs(readme): 模型列表听站点的"
```

