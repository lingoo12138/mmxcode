package gateway

import (
	"context"
	"log"
	"sync"
	"time"
)

// touchEvery is how often the used sessions are written into the database.
//
// 攒着写，不是每个请求写一行：一行 SQLite 写相对上游那一下请求不算什
// 么，但它会跟读额度抢写锁，而读额度是要紧的活（规格 §6.4）。
//
// 20 秒够细了：「活跃」的窗口是 1 小时，20 秒的差别看不出来。
const touchEvery = 20 * time.Second

// touchTimeout is how long the write of the used sessions may take.
//
// 写不进去不是要命的事：这一批的 updated_at 旧一点，下一批会再写一次。
// 但它不能拖着请求不放 —— 等一个空位比等一次写更值得等。
const touchTimeout = 5 * time.Second

// sessionTouches holds the sessions that were used, waiting to be written.
//
// 它是内存里的账：会话用过没有，只有网关知道（规格 §6.3）。它记不住什
// 么 —— 进程一停就没了 —— 但那没关系：重启之后头几趟请求会把它写回去。
type sessionTouches struct {
	mu sync.Mutex
	// keys are the sessions that were used and not written yet.
	keys map[string]bool
}

// newSessionTouches makes an empty ledger.
func newSessionTouches() *sessionTouches {
	return &sessionTouches{keys: map[string]bool{}}
}

// add remembers that a session was used.
func (t *sessionTouches) add(sessionKey string) {
	if sessionKey == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.keys[sessionKey] = true
}

// take gives the sessions that were used and forgets them.
//
// 拿了就是归网关了：写不进去也得放下 —— 留着只会让下一批更大，而下一批
// 会再写一次同样的键。
func (t *sessionTouches) take() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.keys) == 0 {
		return nil
	}
	out := make([]string, 0, len(t.keys))
	for k := range t.keys {
		out = append(out, k)
	}
	t.keys = map[string]bool{}
	return out
}

// flushTouches writes the sessions that were used into the database.
//
// 写不进去就记一笔：写不进去的会话看起来不活跃，池子就少留一个号 ——
// 说出来才知道少了什么。
func (s *Server) flushTouches(ctx context.Context) {
	keys := s.touches.take()
	if len(keys) == 0 {
		return
	}
	if err := s.Store.TouchSessions(ctx, keys); err != nil {
		log.Printf("the %d used sessions could not be written: %v", len(keys), err)
	}
}

// touchLoop writes the used sessions into the database now and then.
//
// 每 touchEvery 一趟：把这一阵子用过的会话写回去。它们的 updated_at 是
// 「活跃会话数」唯一的凭据（规格 §6.3）—— 不写回去，一段一直在用的对话
// 看起来就像不活跃，池子就不会留着它绑着的号，缓存命中也就没了。
func (s *Server) touchLoop() {
	t := time.NewTicker(touchEvery)
	defer t.Stop()
	for {
		select {
		case <-s.lifeCtx.Done():
			// 走之前把攒着的写回去：最后这一批不该丢。
			ctx, cancel := context.WithTimeout(context.Background(), touchTimeout)
			s.flushTouches(ctx)
			cancel()
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(s.lifeCtx, touchTimeout)
			s.flushTouches(ctx)
			cancel()
		}
	}
}
