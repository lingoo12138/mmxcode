package webui

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/lingoo12138/mmxcode/internal/apikey"
	"github.com/lingoo12138/mmxcode/internal/store"
)

// keysData is the keys page's data.
//
// 页面上摆的是列表：每一把 key 认得出是哪一把（名字和开头），也摆得下
// 它什么时候不再管用了。key 本身不在这里——它在申请的那一次就交出去了。
func (w *WebUI) keysData(ctx context.Context) (map[string]any, error) {
	keys, err := w.Store.ListAPIKeys(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]keyView, 0, len(keys))
	for _, k := range keys {
		out = append(out, keyView{APIKey: k, Revoked: k.Revoked()})
	}
	return map[string]any{"Keys": out}, nil
}

// keyView is one key as the page shows it.
type keyView struct {
	store.APIKey
	// Revoked says whether the key has been taken away.
	Revoked bool
}

// apiKeys lists the keys, or makes one.
//
// POST 的回答里有 key 本身：那是它唯一一次出现。库里存的是哈希，别的
// 时刻拿不出来，也就不该拿得出来。
func (w *WebUI) apiKeys(rw http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		// 列表给的是库里的那些行：id、名字、开头、时间。key 本身不在
		// 里面 —— 它只交过一次。
		keys, err := w.Store.ListAPIKeys(r.Context())
		if err != nil {
			http.Error(rw, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(rw, map[string]any{"keys": keys})
	case http.MethodPost:
		w.makeKey(rw, r)
	default:
		rw.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// makeKey makes one key and gives it back.
func (w *WebUI) makeKey(rw http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	name := strings.TrimSpace(body.Name)
	if name == "" {
		// 没起名字的 key 也得有个名字：列表里「（没名字）」比一列空白
		// 好认。
		name = "（没名字）"
	}
	key, err := apikey.New()
	if err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}
	now := time.Now()
	k := store.APIKey{
		ID:        newKeyID(),
		Name:      name,
		KeyHash:   apikey.Hash(key),
		Prefix:    apikey.Display(key),
		CreatedAt: now,
	}
	if err := w.Store.CreateAPIKey(r.Context(), k); err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}
	// 这是它唯一一次出现：往后页面上只有它的开头。
	writeJSON(rw, map[string]any{
		"id": k.ID, "name": k.Name, "key": key, "prefix": k.Prefix,
	})
}

// apiKey takes one key away.
//
// DELETE /api/keys/{id}。
func (w *WebUI) apiKey(rw http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/keys/")
	if id == "" {
		http.NotFound(rw, r)
		return
	}
	switch r.Method {
	case http.MethodDelete:
		if err := w.Store.RevokeAPIKey(r.Context(), id, time.Now()); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				http.Error(rw, "没有这一把 key", http.StatusNotFound)
				return
			}
			http.Error(rw, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(rw, map[string]any{"ok": true})
	default:
		rw.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// newKeyID makes the id of one key.
//
// 时间还不够：两次申请可能落在同一毫秒里，那样的第二个会被主键挡下。
func newKeyID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("key_%d", time.Now().UnixMilli())
	}
	return fmt.Sprintf("key_%d_%s", time.Now().UnixMilli(), hex.EncodeToString(b))
}
