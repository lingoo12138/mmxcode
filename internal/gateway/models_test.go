package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"mmxcode/internal/site"
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
