package gateway

import (
	"context"
	"io"
	"net/http"
	"sync"
	"time"
)

// bodyPool counts the bytes of the small request bodies in the air.
//
// 小身体走的是普通并发，但它们的总量有个上限：那个上限是运营定的，跟有多
// 少账号、承诺多少并发没关系（规格 §5）。
//
// 上限是 0 的时候是不限：那时候只剩并发闸门管着（跟 total_max 一个规
// 矩）。
type bodyPool struct {
	mu sync.Mutex
	// used is how many bytes are in the air.
	used int64
}

// take reserves n bytes when there is room under limit.
//
// 空着的时候总是放一个进来：哪怕这个身体比整个池子还大（配置写小了），
// 它也得能进来 —— 不然它永远等不到，池子空着它也进不来。
func (p *bodyPool) take(n, limit int64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	// 零字节的身体不占地方：池子满了也放它进来。
	if n == 0 {
		return true
	}
	if limit > 0 && p.used > 0 && p.used+n > limit {
		return false
	}
	p.used += n
	return true
}

// give gives n bytes back.
func (p *bodyPool) give(n int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.used -= n
	if p.used < 0 {
		p.used = 0
	}
}

// waitFor reserves n bytes, waiting until there is room or the wait is over.
//
// 等不到就是等不到：客户端自己超时了，网关不该把它吊着。
func (p *bodyPool) waitFor(ctx context.Context, n, limit int64, wait time.Duration) bool {
	deadline := time.Now().Add(wait)
	for {
		if p.take(n, limit) {
			return true
		}
		if !sleepCtx(ctx, 25*time.Millisecond) {
			return false
		}
		if time.Now().After(deadline) {
			return false
		}
	}
}

// bodySmallBytes is how large a request body may be and still count as
// small.
//
// 小的走普通并发，大的（几十兆 —— 图片就是那么大的）一次只让一个进来。
//
// 配 0 的时候按 1 算：那样每个请求都是大身体，网关就成了一条道。
func (s *Server) bodySmallBytes() int64 {
	mb := int64(s.Cfg.Concurrency.BodySmallMB)
	if mb < 1 {
		mb = 1
	}
	return mb << 20
}

// bodyBudgetBytes is how many bytes of small request bodies may be in the
// air at once.
//
// 0 = 不限（跟 total_max 一个规矩）：那时候只剩并发闸门管着。
func (s *Server) bodyBudgetBytes() int64 {
	mb := int64(s.Cfg.Concurrency.BodyBudgetMB)
	if mb < 1 {
		return 0
	}
	return mb << 20
}

// readBody reads the request body.
//
// 客户端说了多大（Content-Length）就照着那个数读，一次读完：ReadAll 是
// 一路翻倍着长大的，16 MiB 的身体它要分配 32 MiB，翻倍的那一刻两份都在
// —— 那 32 MiB 就是峰值，而身体本身只有 16 MiB。
//
// 不知道多大（分块传的）还是用 MaxBytesReader 挡着：那是唯一一种能超过
// Content-Length 的。
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	if r.ContentLength < 0 {
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		return io.ReadAll(r.Body)
	}
	// 客户端说了多大就是多大：服务器照着那个数读，多出来的没有。
	buf := make([]byte, r.ContentLength)
	if _, err := io.ReadFull(r.Body, buf); err != nil {
		return nil, err
	}
	return buf, nil
}
