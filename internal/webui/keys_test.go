package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lingoo12138/mmxcode/internal/apikey"
)

// TestAKeyMadeInTheConsoleIsGivenOnce says the console hands the key back
// once, when it is made.
//
// 那一把 key 只在这一刻出现：库里存的是哈希，别的时刻它拿不出来。所以
// 这一次的回答里必须有它，而页面上别的时刻没有。
func TestAKeyMadeInTheConsoleIsGivenOnce(t *testing.T) {
	w, _ := newTestWebUI(t)
	req := httptest.NewRequest(http.MethodPost, "/api/keys", strings.NewReader(`{"name":"我的机器"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out struct {
		ID  string `json:"id"`
		Key string `json:"key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Key == "" {
		t.Fatal("the answer did not carry the key")
	}
	if out.ID == "" {
		t.Fatal("the answer did not carry the id")
	}
	// 再要一次：这一回只有列表，列表里没有 key 本身。
	list := httptest.NewRecorder()
	w.ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/api/keys", nil))
	if list.Code != 200 {
		t.Fatalf("code = %d", list.Code)
	}
	if strings.Contains(list.Body.String(), out.Key) {
		t.Fatal("the list carries the key itself")
	}
}

// TestTheKeyItselfIsNotInTheList says the list shows the prefix, not the
// key.
//
// 列表是给眼睛看的：认得出哪一把是哪一把就行，能开门的那个不给。
func TestTheKeyItselfIsNotInTheList(t *testing.T) {
	w, _ := newTestWebUI(t)
	req := httptest.NewRequest(http.MethodPost, "/api/keys", strings.NewReader(`{"name":"机器一"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	var out struct {
		Key string `json:"key"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)

	list := httptest.NewRecorder()
	w.ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/api/keys", nil))
	var listing struct {
		Keys []struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			Prefix string `json:"prefix"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Keys) != 1 {
		t.Fatalf("keys = %d, want 1", len(listing.Keys))
	}
	if listing.Keys[0].Name != "机器一" {
		t.Errorf("name = %q, want %q", listing.Keys[0].Name, "机器一")
	}
	if listing.Keys[0].Prefix == "" {
		t.Error("the list has no prefix")
	}
	if strings.Contains(list.Body.String(), out.Key) {
		t.Error("the list carries the key itself")
	}
}

// TestARevokedKeyIsRefused says a revoked key does not open the gate any
// more.
//
// 撤销之后它还在列表里：得让人看得见这把 key 曾经有过、什么时候不灵了。
// 只是它不灵了。
func TestARevokedKeyIsRefused(t *testing.T) {
	w, s := newTestWebUI(t)
	req := httptest.NewRequest(http.MethodPost, "/api/keys", strings.NewReader(`{"name":"机器二"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	var made struct {
		ID  string `json:"id"`
		Key string `json:"key"`
	}
	json.Unmarshal(rec.Body.Bytes(), &made)

	del := httptest.NewRecorder()
	w.ServeHTTP(del, httptest.NewRequest(http.MethodDelete, "/api/keys/"+made.ID, nil))
	if del.Code != 200 {
		t.Fatalf("code = %d, body = %s", del.Code, del.Body.String())
	}

	// 网关不认它了。
	if _, err := s.APIKeyByHash(t.Context(), apikey.Hash(made.Key)); err == nil {
		t.Fatal("the revoked key is still found")
	}
	// 再撤一次：没有了。
	again := httptest.NewRecorder()
	w.ServeHTTP(again, httptest.NewRequest(http.MethodDelete, "/api/keys/"+made.ID, nil))
	if again.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404", again.Code)
	}
}

// TestTheKeysPage says there is a page to ask for keys on.
func TestTheKeysPage(t *testing.T) {
	w, _ := newTestWebUI(t)
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/keys", nil))
	if rec.Code != 200 {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "密钥") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}
