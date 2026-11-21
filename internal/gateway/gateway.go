// Package gateway serves the chat protocols and the management API.
//
// A chat request is bound to an account by its session, and stays
// there. When the account is spent, another is tried and the request
// is replayed — but only while nothing has been written to the client
// yet.
//
// The session id the upstream is told about is the one the database
// keeps: an opaque id, made once and kept, so the upstream sees one
// conversation and the mapping from the session to the upstream's
// conversation survives a restart. The client's own session id does not
// go upstream, and neither does a hash of the request's words.
//
// What that does not fix: two conversations whose system prompt and
// first user message are the same, sent by a client that sends no
// session id of its own, are one session by the session key's own
// definition. They share one upstream id, and one upstream
// conversation, however random that id is, and no server can tell them
// apart: there is nothing in the request that does. The fix for that
// case is the client sending X-Session-Id.
package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"mmxcode/internal/config"
	"mmxcode/internal/convert"
	"mmxcode/internal/egress"
	"mmxcode/internal/manager"
	"mmxcode/internal/pool"
	"mmxcode/internal/rotator"
	"mmxcode/internal/site"
	"mmxcode/internal/store"
	"mmxcode/internal/upstream"
	"mmxcode/internal/usage"
)

// nowOffset gives the timezone offset in seconds east of UTC.
//
// It is the value the reference sends as getTimezoneOffset() * -60.
// time.Time has no Offset() method: Zone()'s second value is it.
func nowOffset() int {
	_, offset := time.Now().Zone()
	return offset
}

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
	// Pool is the accounts the gateway keeps in memory.
	//
	// 池是缓存，不是真相（规格不变量 1）：账号、耗尽标记、会话绑定的真
	// 相都在库里。池存在的理由是选号不用每次扫全表，还有内存有个上界。
	Pool *pool.Pool
	// Keeper fills the pool: at startup with what the database knows,
	// and later with the accounts that are needed.
	Keeper *pool.Keeper
	// touches are the sessions that were used and not written yet.
	//
	// 它们是「活跃会话」唯一的凭据（规格 §6.3）：会话用过没有，只有网
	// 关知道。攒着写回去，写一次算一批。
	touches *sessionTouches
	// Reader reads the credits of an account. It is a field so that a
	// test can stand in for the network: the gateway's only way to the
	// network for the credits goes through it.
	Reader usage.Reader
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

	// busy counts the requests in the air per account.
	busy *busyAccounts
	// gate counts the requests in the air in the whole gateway.
	gate *gate
	// lifeCtx is the life of the gateway: the background work hangs on
	// it, so it stops when the gateway stops.
	lifeCtx context.Context
	// lifeCancel ends the life of the gateway.
	lifeCancel context.CancelFunc
	// bgMu guards the background work's bookkeeping.
	bgMu sync.Mutex
	// bgClosed says the gateway is closing: no new background work.
	bgClosed bool
	// bgSem bounds how many background readings run at once.
	bgSem chan struct{}
	// bgWG counts the background readings that are in the air.
	bgWG sync.WaitGroup

	// concMu guards the cached total concurrency.
	concMu sync.Mutex
	// concLimit is the total concurrency that was read last.
	concLimit int
	// concAt is when it was read.
	concAt time.Time
	// concTTL is how long the cached value is kept. A test sets it to
	// zero to turn the cache off.
	concTTL time.Duration
	// fallback is the database's answer, cached, for the time the pool
	// is empty.
	//
	// 池空的时候要从库里拿账号、标记和余额 —— 那三样都是全表读（规格
	// §6.9）。缓存一秒，让那一波并发只读一次。
	fallback *dbFallback
}

// concurrencyCacheTTL is how long the total concurrency is kept.
//
// 算总并发要读一遍账号和标记，而聊天那条路上每个请求都要问一次。一串
// 请求就是一遍又一遍同样的读，而这一阵子的答案是一个样的：一秒够了。
const concurrencyCacheTTL = time.Second

// New makes a gateway.
func New(mgr *manager.Manager, cfg config.Config, s *store.Store, webui http.Handler) *Server {
	// 多久算热是配置说了算：管理器记着谁刚被用过，这个窗口是它的。
	mgr.HotWindow = cfg.Usage.Refresh.HotWindow.Duration()
	g := &Server{
		Mgr:          mgr,
		Cfg:          cfg,
		Store:        s,
		Reader:       mgr,
		UpstreamBase: site.LLMHost,
		WebUI:        webui,
	}
	// 原生路径先开着：上游说它没有 /responses 的时候再关。
	g.NativeResponses.Store(true)
	g.NativeChatCompletions.Store(true)
	g.busy = newBusyAccounts(cfg.Concurrency.PerAccount)
	g.gate = &gate{}
	// 池空的时候的那份兜底：库里的答案留一秒（规格 §6.9）。
	g.fallback = newDBFallback(s, concurrencyCacheTTL)
	g.lifeCtx, g.lifeCancel = context.WithCancel(context.Background())
	// 后台的读有上限：到顶了就跳过那一次，不排队。
	limit := cfg.Usage.Refresh.Concurrency
	if limit <= 0 {
		limit = 1
	}
	g.bgSem = make(chan struct{}, limit)
	g.concTTL = concurrencyCacheTTL
	// 池：选号从它读，不再每次扫全表（规格 §6.2）。
	g.Pool = pool.New(0)
	g.touches = newSessionTouches()
	g.Keeper = pool.NewKeeper(g.Pool, poolSource{s: s, window: cfg.Pool.SessionWindow.Duration()}, mgr, s,
		keeperOptions(cfg))
	g.Rot = rotator.New(poolAdapter{s: s, busy: g.busy, pool: g.Pool, fallback: g.fallback}, rotator.Options{
		MaxAttempts: cfg.Rotation.MaxAttempts,
		Cooldown:    cfg.Rotation.ExhaustedCooldown.Duration(),
		WindowGroup: manager.WindowGroup,
		Strategy:    cfg.Rotation.Strategy,
	})
	// 池要一直是满的：每 scan_every 一趟看看少了几个，补上。
	go g.poolLoop()
	// 用过的会话要写回去：一批一次，它们是「活跃会话数」的凭据（规格
	// §6.3）。它在后台组里：关网关时要等它把最后一批写完，不然它会往
	// 一个正在关着的库上写。
	g.bgWG.Add(1)
	go func() {
		defer g.bgWG.Done()
		g.touchLoop()
	}()
	return g
}

// round makes the reader of the credits.
//
// 它是网关和调度器共用的那一套：该读哪些账号、一趟读几个、并发几个。
func (s *Server) round() *usage.Round {
	return usage.NewRound(s.Store, s.Reader, s.Mgr.HotAccounts, s.Cfg)
}

// concurrencyLimit says how many requests may be in the air at once.
//
// 它是总并发那道门的上限（可用账号的五分之一，小规模至少三个）。算它要
// 问一次库里有多少个能用的号，而聊天那条路上每个请求都要问一次：一串请
// 求就是一串同样的问。所以答案留一秒 —— 这一阵子的答案是一个样的。
//
// 缓存里的那个数还是照着同样的规矩算出来的：能用的是那些签入中、而且
// 没有被记了「额度用完」的账号。
func (s *Server) concurrencyLimit(ctx context.Context) int {
	s.concMu.Lock()
	defer s.concMu.Unlock()
	if !s.concAt.IsZero() && time.Since(s.concAt) < s.concTTL {
		return s.concLimit
	}
	n := s.totalConcurrency(ctx)
	s.concLimit = n
	s.concAt = time.Now()
	return n
}

// Close stops the background work and waits for it.
//
// 后台的活儿挂在网关的命上：网关关了它也停，而且要等在飞的那些停下
// 来 —— 不然它会往一个正在关着的库上写。
func (s *Server) Close() {
	s.bgMu.Lock()
	s.bgClosed = true
	s.bgMu.Unlock()
	s.lifeCancel()
	s.bgWG.Wait()
}

// startBackground runs fn in the background, if there is room for it.
//
// 同时在跑的读有上限：到顶了就跳过这一次。标记就照着写下去的那份留
// 着（冷却时间），下一趟再说。
//
// 它说活儿有没有开始。
func (s *Server) startBackground(fn func()) bool {
	s.bgMu.Lock()
	defer s.bgMu.Unlock()
	if s.bgClosed {
		return false
	}
	select {
	case s.bgSem <- struct{}{}:
	default:
		return false
	}
	s.bgWG.Add(1)
	go func() {
		defer s.bgWG.Done()
		defer func() { <-s.bgSem }()
		fn()
	}()
	return true
}

// poolStore is what the pool adapter reads from the store.
//
// It is an interface and not the store itself, so that a test can count
// the questions the adapter asks without the store knowing about
// counting.
type poolStore interface {
	ListAccounts(ctx context.Context) ([]store.Account, error)
	ListExhausted(ctx context.Context) ([]store.Exhausted, error)
	BindSession(ctx context.Context, sessionKey, accountID string) error
	SessionState(ctx context.Context, sessionKey string) (store.SessionState, error)
	UpstreamID(ctx context.Context, sessionKey string) (string, error)
	SetExhausted(ctx context.Context, accountID, windowKey string, until time.Time, reason string) error
	LatestUsageSnapshots(ctx context.Context) ([]store.UsageSnapshot, error)
}

// poolAdapter makes the store into a rotator pool.
type poolAdapter struct {
	s poolStore
	// busy counts the requests in the air per account.
	busy *busyAccounts
	// pool is the accounts kept in memory.
	//
	// 它是空的（还没填上，或者填池失败）的时候，适配器回到从前的样子，
	// 从库里拿：网关不能因为池空着就一个请求都答不了。
	pool *pool.Pool
	// fallback is the database's answer, cached, for the time the pool
	// is empty.
	//
	// 池空的时候那三样都是全表读（规格 §6.9）：缓存一秒，让那一波并发只
	// 读一次。
	fallback *dbFallback
}

// fromPool says whether the pool has something to give.
//
// 池空着的时候（还没填上，或者填池失败）适配器回到从前的样子，从库里
// 拿：网关不能因为池空着就一个请求都答不了（规格不变量 1）。
func (p poolAdapter) fromPool() bool {
	return p.pool != nil && p.pool.Len() > 0
}

func (p poolAdapter) Accounts(ctx context.Context) ([]store.Account, error) {
	if p.fromPool() {
		return p.pool.Accounts(), nil
	}
	if p.fallback != nil {
		return p.fallback.accounts(ctx)
	}
	return p.s.ListAccounts(ctx)
}

func (p poolAdapter) Exhausted(ctx context.Context) ([]store.Exhausted, error) {
	if p.fromPool() {
		// 池里的号没有标记：额度用完的号当场出池（规格 §4.4），填池的
		// 时候也不收有标记的号。所以这一份是空的 —— 标记的真相在库
		// 里，池空的时候从库里拿。
		return nil, nil
	}
	if p.fallback != nil {
		return p.fallback.exhausted(ctx)
	}
	return p.s.ListExhausted(ctx)
}

func (p poolAdapter) BindSession(ctx context.Context, key, accountID string) error {
	return p.s.BindSession(ctx, key, accountID)
}

func (p poolAdapter) SessionState(ctx context.Context, key string) (store.SessionState, error) {
	return p.s.SessionState(ctx, key)
}

// UpstreamID gives the upstream id of a session.
//
// The id is the one the database keeps: an opaque id, made once and
// kept, so the upstream sees one conversation and the mapping survives
// a restart. The client's own session id does not go upstream.
func (p poolAdapter) UpstreamID(ctx context.Context, key string) (string, error) {
	return p.s.UpstreamID(ctx, key)
}

func (p poolAdapter) MarkExhausted(ctx context.Context, accountID, windowKey string, until time.Time, reason string) error {
	err := p.s.SetExhausted(ctx, accountID, windowKey, until, reason)
	if p.pool != nil {
		// 这个号出池：它接不了活了，让位给一个冷的（规格 §4.4）。补号
		// 是后台的活。
		//
		// 池里不留标记：它已经不在池里了，留着也没有人能看见 —— 标记的
		// 真相在库里（那个号出池之后，填池也不收它）。
		//
		// 标记写不写得进去，池里都不留它：池是缓存，把一个库里还觉得好
		// 的号丢出去是安全的（它以后还能再进来），而把一个已经用完的号
		// 留在池里，它带着一份看着还有额度的旧数，每一次被挑中都是一次
		// 白花的请求。
		p.pool.Remove(accountID)
	}
	return err
}

// RemainingAll gives the credits left of a whole list of accounts.
//
// The rotator asks for every account it may pick in one question, so
// the answer comes out of one read of the readings and not one read
// per account.
//
// The map holds only the accounts whose credits are known: an account
// that is not in it has not been read yet, and the rotator takes it
// after the ones whose balance it knows.
func (p poolAdapter) RemainingAll(ctx context.Context, accountIDs []string) map[string]rotator.Remaining {
	out := map[string]rotator.Remaining{}
	if len(accountIDs) == 0 {
		return out
	}
	wanted := make(map[string]bool, len(accountIDs))
	for _, id := range accountIDs {
		wanted[id] = true
	}
	if p.fromPool() {
		for id := range wanted {
			e, ok := p.pool.Get(id)
			if !ok || !e.Known {
				// 没读到余额的账号不算有余额：留空，让 rotator 把它排在
				// 后面。
				continue
			}
			out[id] = rotator.Remaining{Left: e.Remaining, Known: true}
		}
		return out
	}
	if p.fallback != nil {
		return p.fallback.remaining(ctx, accountIDs)
	}
	snaps, err := p.s.LatestUsageSnapshots(ctx)
	if err != nil {
		return out
	}
	for _, s := range snaps {
		if !wanted[s.AccountID] {
			continue
		}
		if !s.HasAmounts {
			// 没读到余额的账号不算有余额：留空，让 rotator 把它排在后面。
			continue
		}
		out[s.AccountID] = rotator.Remaining{Left: s.Remaining, Known: true}
	}
	return out
}

// Remaining gives one account's credits left.
//
// It goes through the same read as RemainingAll, so that the two
// cannot disagree about one account.
func (p poolAdapter) Remaining(ctx context.Context, accountID string) (float64, bool) {
	rem, ok := p.RemainingAll(ctx, []string{accountID})[accountID]
	if !ok {
		return 0, false
	}
	return rem.Left, true
}

// Busy says whether a request is in the air on an account.
//
// 网关把在飞的计数报给 rotator，rotator 挑账号的时候避开在飞的那些
// （规格 §6.3）。这个账是网关自己的：它不用问数据库。
func (p poolAdapter) Busy(ctx context.Context, accountID string) bool {
	if p.busy == nil {
		return false
	}
	return p.busy.isBusy(accountID)
}

// ServeHTTP routes a request.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/signin":
		// 登录页不用会话：没有会话的人正要来这里登录。
		s.signInPage(w, r, http.StatusOK, "")
		return
	case "/api/session":
		// 发 cookie 和销 cookie 都不用会话。
		s.session(w, r)
		return
	}
	// 两扇门：聊天那扇认两把 key（配置里的和后台申请的），管理页那扇只
	// 认配置里的那把。
	gate := s.auth
	if isChatPath(r.URL.Path) {
		gate = s.authChat
	}
	if !gate(r) {
		if isAPIPath(r.URL.Path) {
			writeError(w, protocolOf(r.URL.Path), http.StatusUnauthorized, "the key was refused")
			return
		}
		// 浏览器来的是页面：给它登录页，不是一段 JSON。
		s.signInPage(w, r, http.StatusOK, "")
		return
	}
	switch r.URL.Path {
	case "/v1/messages":
		s.handleChat(w, r, protocolAnthropic)
		return
	case "/v1/chat/completions":
		s.handleChat(w, r, protocolOpenAI)
		return
	case "/v1/responses":
		s.handleChat(w, r, protocolCodex)
		return
	case "/v1/models":
		s.handleModels(w, r)
		return
	case "/v1/usage":
		// magpie 的卡片：这个网关还剩多少额度。
		s.usageCard(w, r)
		return
	}
	if s.WebUI != nil && strings.HasPrefix(r.URL.Path, "/api/") {
		s.WebUI.ServeHTTP(w, r)
		return
	}
	if s.WebUI != nil {
		s.WebUI.ServeHTTP(w, r)
		return
	}
	writeError(w, protocolOf(r.URL.Path), http.StatusNotFound, "no such endpoint")
}

// protocol is the protocol the client speaks.
type protocol int

const (
	protocolAnthropic protocol = iota
	protocolOpenAI
	protocolCodex
)

// protocolOf says which protocol a path speaks.
//
// 一个路径一个协议：错误的样子也跟着协议走，所以路由和错误都要问
// 同一句。
func protocolOf(path string) protocol {
	switch path {
	case "/v1/chat/completions":
		return protocolOpenAI
	case "/v1/responses":
		return protocolCodex
	default:
		return protocolAnthropic
	}
}

// maxBodyBytes is how large a request body may be.
//
// 64 MiB：用户是可能传图片的 —— 站点的模型收 text、image、video（参考插件
// 的模型名单就是这么写的），而图片是 base64 写在身体里的，一张图几 MB 是常
// 态，用户传几张就上三十兆。
//
// 1M token 的对话是 4 MB 上下（带上 JSON 外壳 4.5–5 MB），64 MiB 是它的十几
// 倍，够宽裕。
//
// 再大的请求不是对话，是要把它这个网关的内存吃光的东西：没有上限的时候，一
// 个请求就能让网关没有内存可用。
const maxBodyBytes = 64 << 20

// bodyTooBig says the request body is larger than the gateway takes.
//
// 数要报出来：传图片的人被挡下来的时候，得知道那个上限是多少、自己超了
// 多少，才知道该少传一张还是该改配置。got 是负数的时候是不知道有多大
// （客户端没说是多大的）。
func bodyTooBig(limit, got int64) string {
	if got >= 0 {
		return fmt.Sprintf("the request body is %d bytes; the gateway takes at most %d bytes (%d MiB)",
			got, limit, limit>>20)
	}
	return fmt.Sprintf("the request body is larger than the %d bytes (%d MiB) the gateway takes",
		limit, limit>>20)
}

// handleChat serves a chat request.
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request, proto protocol) {
	// 客户端说了身体有多大（Content-Length）的话，超限的那个请求连读都
	// 不用读就拒了：读进来是 64 MiB 的内存，而它马上就是一个 413。
	//
	// 客户端没说的（分块传的）就不知道有多大 —— 那就还是读，MaxBytesReader
	// 照旧挡着。
	if r.ContentLength > maxBodyBytes {
		writeError(w, proto, http.StatusRequestEntityTooLarge, bodyTooBig(maxBodyBytes, r.ContentLength))
		return
	}
	ctx := r.Context()
	// 总并发：可用账号的五分之一，小规模至少三个。超了的请求等，等
	// 不到就 429 带 Retry-After —— 客户端自己超时了，网关不该把它吊
	// 着。
	//
	// 闸门在读请求体**之前**：闸门是网关对外面承诺的并发，在这个数里
	// 面，请求才占着网关的内存。请求体最大 64 MiB —— 排在一百个请求
	// 后面的那每一个都已经把身体读进内存了的话，那是 6.4 GB，跟承诺的
	// 并发一点关系都没有。排队的时候身体还在客户端那边，网关没有替它
	// 占内存。
	if !s.gate.waitFor(ctx, s.concurrencyLimit(ctx), s.Cfg.Concurrency.Wait.Duration()) {
		w.Header().Set("Retry-After", "5")
		writeError(w, proto, http.StatusTooManyRequests, "the gateway is busy: too many requests are in the air")
		return
	}
	defer s.gate.give()
	// 请求体有个上限：不然一个客户端发来一大块东西，网关就把它整个
	// 读进内存里，然后就没有内存了。
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, proto, http.StatusRequestEntityTooLarge, bodyTooBig(tooBig.Limit, -1))
			return
		}
		writeError(w, proto, http.StatusBadRequest, "the request body could not be read")
		return
	}
	// 上游请求的时限就是配的那一个。流式对话也吃这份预算：想要更长的
	// 就把 upstream.timeout 调大。
	if d := s.Cfg.Upstream.Timeout.Duration(); d > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}
	model := modelOf(body)
	stream := isStream(body)
	sess := rotator.SessionOf(r.Header, systemOf(body), firstUserOf(body))
	// 发给上游的会话 id 是数据库里存着的那个：不重算，也不把客户端
	// 自己那串字符串发出去。取不到就是取不到，不能拿别的顶上：顶上来的
	// 那个上游没见过，会把这段对话当成新的一段。
	//
	// 它是跟着轮转的 plan 一起回来的：那一行每个请求都要读一次，读出来
	// 的东西就跟着走，不用再问一遍。池子给不出 id 的时候（那种池子没有
	// 上游 id 这个本事），用的是请求自己算出来的那个。
	upstreamID := sess.UpstreamID

	used := map[string]bool{}
	var lastErr error
	// quota 说最后一次失败是“额度用完了”，quotaMsg 是上游自己说的那句。
	// 客户端最后看到的是上游的话，不是网关自己编的一句。
	var quota bool
	var quotaMsg string
	// lastStatus 是上游最后一次给的状态，lastMsg 是它自己说的那句。
	// 客户端最后看到的是上游的状态，不是网关编出来的一个 429：上游说
	// 503 就是 503，客户端才知道是上游坏了，而不是自己被限流了。
	var lastStatus int
	var lastMsg string
	attempts := s.Cfg.Rotation.MaxAttempts
	if attempts <= 0 {
		attempts = 3
	}
	// busyDeadline is when this request stops waiting for a free account.
	// 它是整个请求的一份：等一个账号空出来，等不到就 429（规格 §6.2）。
	var busyDeadline time.Time
	// waitForFree waits for an account that can answer.
	waitForFree := func() bool {
		if busyDeadline.IsZero() {
			busyDeadline = time.Now().Add(s.Cfg.Concurrency.Wait.Duration())
		}
		return s.waitForFree(ctx, busyDeadline, model, used)
	}
	for {
		plan, err := s.Rot.Pick(ctx, sess.Key, model, used)
		if err != nil {
			if errors.Is(err, rotator.ErrAllBusy) {
				// 能答的账号都在飞：等前面的请求结束，而不是报错
				// （规格 §6.4）。等不到就是等不到：客户端自己超时了。
				if !waitForFree() {
					w.Header().Set("Retry-After", "5")
					writeError(w, proto, http.StatusTooManyRequests,
						"every account is busy right now: too many requests are in the air")
					return
				}
				continue
			}
			if lastErr != nil {
				s.writeFailure(w, proto, quota, quotaMsg, lastStatus, lastMsg, lastErr)
				return
			}
			writeError(w, proto, http.StatusServiceUnavailable, err.Error())
			return
		}
		// 上游的会话 id 跟着轮转的 plan 一起回来了：那一行每个请求都要读
		// 一次，读出来的东西就跟着走，不用再问一遍。
		//
		// 池子给不出的时候（那种池子没有上游 id 这个本事），用的还是请求
		// 自己算出来的那个。
		if plan.UpstreamID != "" {
			upstreamID = plan.UpstreamID
		}
		if !s.busy.take(plan.AccountID) {
			// 刚挑中的那个账号在挑的时候还是闲的，现在被人抢了：等它
			// 空出来。这一次不算一次失败：它没有坏，它只是忙。
			if !waitForFree() {
				w.Header().Set("Retry-After", "5")
				writeError(w, proto, http.StatusTooManyRequests,
					"every account is busy right now: too many requests are in the air")
				return
			}
			continue
		}
		// 抢到了：这个账号归这个请求了。这一次算一次尝试：它真的被问
		// 过上游了。
		used[plan.AccountID] = true
		// 这段对话用过：记下来，攒一批写回库（规格 §6.3）。它的
		// updated_at 是「活跃会话数」的凭据 —— 池要靠它把这段对话绑
		// 着的号留在池里。
		s.touches.add(sess.Key)
		// 这个账号的活干完就把账号还回去：它在飞的时候不能接第二个
		// 请求。闭包是为了那个"还回去"一定发生 —— 无论这一圈是怎么
		// 结束的。
		answered := func() bool {
			defer s.busy.give(plan.AccountID)

			tok, err := s.Mgr.Token(ctx, plan.AccountID)
			if err != nil {
				lastErr = err
				// 这个号签不上了：它出池，让位给一个冷的。
				s.dropDeadAccount(ctx, plan.AccountID)
				return false
			}
			// 候选地址：账号自己的地址 → 它的组 → 全局组。路不通的时候还
			// 有下一条。
			cands, err := s.Mgr.Candidates(ctx, plan.AccountID)
			if err != nil {
				lastErr = err
				return false
			}
			ci := 0
			var transport http.RoundTripper
			if len(cands) > 0 {
				transport, err = s.Mgr.Egress.Transport(cands[0])
				if err != nil {
					lastErr = err
					return false
				}
			}

			upBody, upPath, convertBack, err := s.upstreamShape(body, proto)
			if err != nil {
				writeError(w, proto, http.StatusBadRequest, err.Error())
				return true
			}
			chatReq := upstream.ChatRequest{
				BaseURL:   s.UpstreamBase,
				Path:      upPath,
				Access:    tok.Access,
				SessionID: upstreamID,
				// 秒，UTC 以东。参考实现发的是 getTimezoneOffset() * -60，
				// 和 Zone() 给的偏移是同一个值。time.Time 没有 Offset()
				// 方法，Zone() 的第二个返回值就是它。
				TZOffset: nowOffset(),
				Body:     upBody,
				Stream:   stream,
				Extra:    clientHeaders(r),
			}

			// renewed 说这个账号的令牌已经续过一次：MiniMax 的 refresh token
			// 只能用一次，续第二次会把账号签死。
			renewed := false
			// 同一个账号可以再问几次：限流、5xx、续签之后，都是这个账号
			// 再来一次就好。
			for try := 0; try < attempts; try++ {
				resp, raw, err := s.attempt(ctx, chatReq, transport, stream)
				if err != nil {
					var oe *upstream.OtherSiteError
					if errors.As(err, &oe) {
						// 跨站：不换号、不消耗 refresh token
						writeError(w, proto, http.StatusBadGateway, oe.Error())
						return true
					}
					var tooBig *upstream.TooLargeError
					if errors.As(err, &tooBig) {
						// 上游发的不是回答：再问同一个账号也是这么大的回答
						// （回答的大小是模型和请求的事，不是账号的事）。当场
						// 答 502，不重试。
						writeError(w, proto, http.StatusBadGateway, err.Error())
						return true
					}
					// 路不通：还有下一条路就立刻换。不是上游慢，是这条路
					// 不通，等没有用。
					if ci+1 < len(cands) {
						failed := cands[ci]
						ci++
						tr, terr := s.Mgr.Egress.Transport(cands[ci])
						if terr != nil {
							lastErr = terr
							return false
						}
						transport = tr
						// 换掉的那条路记一笔坏的：下一个请求不该从它开始。
						_ = s.Mgr.MarkEgressBad(ctx, failed, err)
						// 换地址这件事要说一声：看着是「那个号走了 A 路」，其
						// 实走了 B 路。
						fmt.Printf("egress: the account %s moved from %s to %s\n",
							plan.AccountID, egress.Redacted(failed), egress.Redacted(cands[ci]))
						// 新的一条路还没有预算：try 归零（循环末尾会加一，
						// 所以是 -1）。换的是路，不是重试。
						try = -1
						continue
					}
					// 网络没通：还是这个账号，等一等再来。上游没有给出
					// 状态：这是网关自己的问题。
					lastErr = err
					lastStatus = 0
					lastMsg = ""
					if !sleepCtx(ctx, backoff(try)) {
						return true
					}
					continue
				}

				// 上游没有这条路：改用本地转换，同一个账号再来一次。
				//
				// 开放平台对 MiniMax Code 的账号答的也是「没有这条路」：503
				// 加一句 direct_route_not_configured。那不是站点坏了，是那条
				// 路对这个账号不通 —— 该走 Anthropic 那条。
				if resp.StatusCode == http.StatusNotFound || openPlatformNotConfigured(raw) {
					fb, fp, fback, ok := s.fallbackShape(body, proto, upPath)
					if ok {
						upBody, upPath, convertBack = fb, fp, fback
						chatReq.Body = upBody
						chatReq.Path = upPath
						resp, raw, err = s.attempt(ctx, chatReq, transport, stream)
						if err != nil {
							var oe *upstream.OtherSiteError
							if errors.As(err, &oe) {
								writeError(w, proto, http.StatusBadGateway, oe.Error())
								return true
							}
							var tooBig *upstream.TooLargeError
							if errors.As(err, &tooBig) {
								writeError(w, proto, http.StatusBadGateway, err.Error())
								return true
							}
							lastErr = err
							lastStatus = 0
							lastMsg = ""
							if !sleepCtx(ctx, backoff(try)) {
								return true
							}
							continue
						}
					}
				}

				// 200 且是流：直接转出去
				if resp.StatusCode == http.StatusOK && raw == nil {
					// 答上了：这个账号刚刚被用过，它变热了。
					s.Mgr.MarkUsed(plan.AccountID)
					s.writeStream(w, resp, proto, convertBack, model)
					return true
				}

				// 200 但身体里有业务错误
				if resp.StatusCode == http.StatusOK {
					if err := upstream.CheckEnvelope("chat", raw); err == nil {
						// 答上了：这个账号刚刚被用过，它变热了。
						s.Mgr.MarkUsed(plan.AccountID)
						s.writeJSON(w, resp, raw, proto, convertBack, model)
						return true
					}
				}

				failure := s.failure(resp.StatusCode, raw)
				lastStatus = resp.StatusCode
				lastMsg = failure.Message
				if lastMsg == "" {
					// 上游自己说的话要给客户端和运维看：只知道一个状态码，
					// 谁也说不清到底是哪一件事坏了。
					lastMsg = upstreamMessage(raw)
				}
				switch s.action(failure) {
				case rotator.ActionFailover:
					// 额度用完了：这个账号的窗口记成用完，换一个账号。
					s.markSpent(ctx, plan.AccountID, plan.GroupKey)
					lastErr = fmt.Errorf("the account's credits or plan are used up")
					quota = true
					quotaMsg = failure.Message
					return false
				case rotator.ActionRenew:
					if renewed {
						// 续过一次还是被拒：这个账号先放着，换一个。
						lastErr = fmt.Errorf("the sign-in was refused (HTTP %d)", resp.StatusCode)
						quota = false
						quotaMsg = ""
						return false
					}
					newTok, rerr := s.Mgr.ForceRenew(ctx, plan.AccountID)
					if rerr != nil {
						// 跨站：不换号、不消耗 refresh token
						var oe *upstream.OtherSiteError
						// 续签自己的错要看 rerr：err 是上一次尝试的，
						// 那时候它是空的，看它永远看不出来。
						if errors.As(rerr, &oe) {
							writeError(w, proto, http.StatusBadGateway, oe.Error())
							return true
						}
						lastErr = rerr
						quota = false
						quotaMsg = ""
						return false
					}
					// 用新 token 再来一次，还是这个账号
					renewed = true
					chatReq.Access = newTok.Access
					quota = false
					quotaMsg = ""
					continue
				case rotator.ActionRetrySame:
					// 限流、5xx：这个账号等一等，再问它一次。
					lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
					quota = false
					quotaMsg = ""
					wait, waitOK := waitBeforeRetry(resp, try)
					if !waitOK {
						// 上游让等太久：等是等不到的，换一个号。
						// 那个窗口不记成用完 —— 限流不是额度的事。
						lastErr = fmt.Errorf("HTTP %d: the upstream asked to wait longer than %s",
							resp.StatusCode, maxWaitForTheSameAccount)
						return false
					}
					if !sleepCtx(ctx, wait) {
						return true
					}
					continue
				default:
					// 给不出别的说法：上游的状态原样给客户端。
					s.writeFailure(w, proto, false, "", resp.StatusCode, lastMsg, lastErr)
					return true
				}
			}
			return false
		}()
		if answered {
			return
		}
	}
}

// failure says what went wrong upstream.
//
// The code and the message come out of the parsed answer, so that the
// decision the rotator makes and the words the client is given are read
// from one thing and not from two.
func (s *Server) failure(status int, raw []byte) rotator.Failure {
	f := rotator.Failure{Status: status, Body: raw}
	err := upstream.CheckEnvelope("chat", raw)
	var be *upstream.BusinessError
	if errors.As(err, &be) {
		f.Code = be.Code
		f.Message = be.Message
		// 1000048：站点说签到被拒了。令牌照旧了，账号本身没问题。
		if errors.Is(err, upstream.ErrSignInRefused) {
			f.Refused = true
		}
		return f
	}
	// 没有 MiniMax 自己的形状：消息按它平常的样子读，rotator 读的
	// 也是同一份 —— 客户端看到的和它做决定用的是同一句话。
	f.Message = rotator.UpstreamMessage(raw)
	return f
}

// upstreamMessage reads the message out of an upstream body.
//
// 上游的话要看得见：客户端和运维都要知道上游说了什么，而不是只知道
// 一个状态码。这和 rotator 读消息的是同一份。
func upstreamMessage(raw []byte) string {
	return rotator.UpstreamMessage(raw)
}

// action says what to do about the answer.
//
// The rotator owns the rule, and on the chat path the rule is the
// site's own: a 401 means the token was turned away before its end, so
// the token is renewed and the same account is asked again. A 403 is
// the site's refusal to answer and not a stale token — renewing on it
// would spend a refresh token (which MiniMax gives only once) on
// every refusal.
//
// The account API is a different story: there the site refuses a
// sign-in with a 403 as well as with a 401, and the account package
// reads both that way.
func (s *Server) action(failure rotator.Failure) rotator.Action {
	return rotator.Decide(failure)
}

// writeFailure writes the answer of a request that could not be served.
//
// The status is the upstream's own, when the upstream gave one: a 503
// is a 503 and a 400 is a 400. Calling both of them a 429 tells the
// client it was sent too fast, and it backs off from a gateway that is
// not the problem. The one that stays a 429 is the refusal about the
// credits: that is the one the client can do something about.
//
// A failure with no upstream status at all is the gateway's own: the
// request never got through, and that is a 502.
//
// The words the client is given are the upstream's own when it said the
// credits are gone: "usage limit reached: …" tells the client to wait
// and try again, which a plain error does not.
//
// 一个上游的 401/403 还要带一个头：X-Magpie-Sign-In: kept。
//
// magpie 的规矩是「401 默认就把这个账号标成签入失效」，用户被踢回签入页
// —— 除非回答里带这个头。网关后面所有账号同时死掉的那一次，客户端收到的
// 是上游的 401；magpie 会把「网关这个账号」（用户签入的那个）标成签入失效，
// 用户重新签进去发现一切正常 —— 网关是好的，坏的是网关后面的账号。参考
// 插件在每一个上游回答上都带着这个头（minimax/index.mjs 里 answer 那
// 一句：headers.set("X-Magpie-Sign-In", renewed && res.ok ? "renewed" :
// "kept")）。
//
// 这个头只加在上游的 401/403 上。客户端自己的 key 被拒的那个 401 是网关
// 这扇门挡的（ServeHTTP 里那个 writeError），不在这里走：那个 401 的意思
// 就是「你的 key 不对」，magpie 应该照旧把签入记成失效。
func (s *Server) writeFailure(w http.ResponseWriter, proto protocol, quota bool, quotaMsg string, status int, message string, err error) {
	if quota {
		msg := quotaMsg
		if msg == "" {
			msg = "the account's credits or plan are used up"
		}
		writeRateLimit(w, proto, msg)
		return
	}
	if status == 0 {
		// 上游没有给出状态：网络没通。这是网关自己的问题。
		msg := "the request did not get through"
		if err != nil {
			msg = err.Error()
		}
		writeError(w, proto, http.StatusBadGateway, msg)
		return
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		// 签入还是好的：坏的是网关后面的账号，不是用户签进来的这把。
		//
		// status 是上游自己给的：走到这里的每一个状态都是上游的。客户端
		// 自己那把 key 被拒的 401 从来不到这里 —— 那是网关这扇门挡的
		// （ServeHTTP 里那个 writeError），那个 401 的意思就是「你的 key
		// 不对」，magpie 应该照旧把签入记成失效。
		w.Header().Set("X-Magpie-Sign-In", "kept")
	}
	if message == "" {
		message = fmt.Sprintf("the upstream answered with HTTP %d", status)
	}
	writeError(w, proto, status, message)
}

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

// attempt sends the request upstream and gives the answer back.
//
// A stream is read no further than its first event: enough to tell an
// error from an answer, little enough that the client still gets the
// first token as soon as the upstream sends it. The rest of the stream
// is left on the response's body.
//
// raw is the whole body when the answer was not a stream, and nil when
// it was.
func (s *Server) attempt(ctx context.Context, req upstream.ChatRequest, transport http.RoundTripper, wantStream bool) (*http.Response, []byte, error) {
	resp, err := s.Mgr.Up.Chat(ctx, req, transport)
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode != http.StatusOK {
		raw, _ := upstream.ReadBody(resp)
		return resp, raw, nil
	}
	if !wantStream {
		raw, err := upstream.ReadBody(resp)
		if err != nil {
			return resp, nil, err
		}
		return resp, raw, nil
	}
	br := bufio.NewReader(resp.Body)
	head, _ := peekEvent(br)
	if sseHasError(head) {
		// 200 里装着错误：整条读完，当成失败处理
		rest, _ := upstream.ReadBodyLimit(br, upstream.MaxBody())
		resp.Body.Close()
		return resp, append(head, rest...), nil
	}
	// 把读走的第一个事件放回去，客户端一个字节都不会少
	resp.Body = &rejoinedBody{
		Reader: io.MultiReader(bytes.NewReader(head), br),
		Closer: resp.Body,
	}
	return resp, nil, nil
}

// rejoinedBody is a body whose first event was read ahead of time.
type rejoinedBody struct {
	io.Reader
	io.Closer
}

// peekEvent reads the first event of a stream.
//
// An SSE event ends at the blank line, so that is what is read.
func peekEvent(br *bufio.Reader) ([]byte, error) {
	var head []byte
	for {
		line, err := br.ReadString('\n')
		head = append(head, line...)
		if err != nil {
			return head, err
		}
		if line == "\n" {
			return head, nil
		}
	}
}

// sseHasError says a stream's first event carries an error.
//
// MiniMax can answer 200 with an error in the stream. By the time the
// client has seen a byte the account cannot be changed any more, so
// the error is looked for before anything is written.
func sseHasError(head []byte) bool {
	for _, line := range strings.Split(string(head), "\n") {
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var ev struct {
			Type       string          `json:"type"`
			Error      json.RawMessage `json:"error"`
			StatusInfo *struct {
				Code int `json:"code"`
			} `json:"statusInfo"`
		}
		if json.Unmarshal([]byte(payload), &ev) != nil {
			continue
		}
		if ev.StatusInfo != nil && ev.StatusInfo.Code != 0 {
			return true
		}
		if len(ev.Error) > 0 && string(ev.Error) != "null" && string(ev.Error) != "{}" {
			return true
		}
		if ev.Type == "error" {
			return true
		}
	}
	return false
}

// upstreamShape gives the request the upstream is to get.
//
// Anthropic goes through untouched: the site speaks it. The OpenAI and
// the Responses protocols go through untouched as long as the site is
// thought to speak them; when the site says it does not have them (a
// 404), the request is turned into an Anthropic one and the answer is
// turned back afterwards.
func (s *Server) upstreamShape(body []byte, proto protocol) (out []byte, path string, convertBack bool, err error) {
	switch proto {
	case protocolAnthropic:
		return body, site.MessagesPath, false, nil
	case protocolOpenAI:
		if s.NativeChatCompletions.Load() {
			return body, site.ChatCompletionsPath, false, nil
		}
		b, cerr := convert.ToAnthropicOpenAI(body)
		if cerr != nil {
			return nil, "", false, fmt.Errorf("the request could not be turned into the upstream's format: %w", cerr)
		}
		return b, site.MessagesPath, true, nil
	default:
		if s.NativeResponses.Load() {
			return body, site.ResponsesPath, false, nil
		}
		b, cerr := convert.ToAnthropic(body)
		if cerr != nil {
			return nil, "", false, fmt.Errorf("the request could not be turned into the upstream's format: %w", cerr)
		}
		return b, site.MessagesPath, true, nil
	}
}

// fallbackShape gives the shape to use when the upstream does not have
// the path the request went out to.
//
// The site may well serve /chat/completions and /responses itself, and
// that is the better way: nothing is converted and nothing is lost.
// When it answers a 404 instead, the path is closed for everybody and
// the request is turned into an Anthropic one; the answer is turned
// back afterwards.
//
// The path the request went out to is what decides: a request that
// already went out as an Anthropic one has no fallback left, and its
// 404 is the upstream's own answer.
func (s *Server) fallbackShape(body []byte, proto protocol, path string) (out []byte, newPath string, convertBack bool, ok bool) {
	switch proto {
	case protocolCodex:
		if path != site.ResponsesPath {
			return nil, "", false, false
		}
		// 上游说它没有 /responses：这条路就关掉了。
		s.NativeResponses.Store(false)
	case protocolOpenAI:
		if path != site.ChatCompletionsPath {
			return nil, "", false, false
		}
		// 上游说它没有 /chat/completions：这条路就关掉了。
		s.NativeChatCompletions.Store(false)
	default:
		return nil, "", false, false
	}
	b, p, back, err := s.upstreamShape(body, proto)
	if err != nil {
		return nil, "", false, false
	}
	return b, p, back, true
}

// copyHeaders gives the client's the upstream's headers.
//
// The encoding is left out: the client is told what it gets, not that
// it gets something compressed.
func copyHeaders(w http.ResponseWriter, resp *http.Response) {
	for k, v := range resp.Header {
		switch strings.ToLower(k) {
		case "content-encoding", "content-length", "transfer-encoding":
			continue
		}
		w.Header()[k] = v
	}
}

// writeStream writes the upstream's stream to the client.
//
// The bytes go out as they arrive, so the client gets the first token
// as soon as the upstream sends it. Once the first byte is out the
// account cannot be changed any more; if the stream then breaks, the
// client is told the stream ended early rather than being sent a
// second, different answer.
//
// The event that says so has the shape of the protocol the client
// speaks: an OpenAI client does not read Anthropic's error event, and a
// Codex client reads the Responses one.
func (s *Server) writeStream(w http.ResponseWriter, resp *http.Response, proto protocol, convertBack bool, model string) {
	copyHeaders(w, resp)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	var err error
	if convertBack {
		// 转换器自己收尾：它失败的时候已经写了一个失败事件，
		// 网关再写一个就是第二次失败，形状还不一样。
		switch proto {
		case protocolCodex:
			err = convert.StreamResponses(resp.Body, w, model)
		case protocolOpenAI:
			err = convert.StreamChatCompletions(resp.Body, w, model)
		}
	} else {
		sniffer := &idSniffer{w: w}
		_, err = io.Copy(sniffer, resp.Body)
		if err != nil {
			writeStreamError(sniffer, proto, err, model, sniffer.responseID())
		}
	}
	if flusher != nil {
		flusher.Flush()
	}
	resp.Body.Close()
}

// writeStreamError writes the event that says the stream broke.
//
// The shape is the one of the protocol the client speaks.
func writeStreamError(w io.Writer, proto protocol, err error, model, responseID string) {
	message := "the stream ended early: " + err.Error()
	switch proto {
	case protocolOpenAI:
		// OpenAI 的流里，错误也是一行 data。
		b, _ := json.Marshal(map[string]any{
			"error": map[string]any{"message": message, "type": "api_error"},
		})
		fmt.Fprintf(w, "data: %s\n\n", b)
	case protocolCodex:
		// Codex 读的是 Responses 的形状，和本地转换写出来的一样。
		response := map[string]any{
			"object": "response",
			"model":  model,
			"status": "failed",
			"error":  map[string]any{"code": "upstream_error", "message": message},
		}
		if responseID != "" {
			response["id"] = responseID
		}
		b, _ := json.Marshal(map[string]any{
			"type":     "response.failed",
			"response": response,
		})
		fmt.Fprintf(w, "event: response.failed\ndata: %s\n\n", b)
	default:
		b, _ := json.Marshal(map[string]any{
			"type":  "error",
			"error": map[string]any{"type": "api_error", "message": message},
		})
		fmt.Fprintf(w, "event: error\ndata: %s\n\n", b)
	}
}

// idSniffer keeps the first bytes of a stream, to find the response id.
//
// The Responses protocol names the response in its first event, and the
// failure at the end of a broken stream has to carry the same id: the
// client matches the two.
type idSniffer struct {
	w   io.Writer
	buf []byte
}

// maxSniff is how many bytes are looked at for the response id.
const maxSniff = 8 << 10

func (s *idSniffer) Write(p []byte) (int, error) {
	if len(s.buf) < maxSniff {
		n := maxSniff - len(s.buf)
		if n > len(p) {
			n = len(p)
		}
		s.buf = append(s.buf, p[:n]...)
	}
	return s.w.Write(p)
}

// responseID gives the id of the response the stream is about.
func (s *idSniffer) responseID() string {
	m := responseIDPattern.FindSubmatch(s.buf)
	if m == nil {
		return ""
	}
	return string(m[1])
}

// responseIDPattern finds the id of the response in the first events.
var responseIDPattern = regexp.MustCompile(`"id"\s*:\s*"(resp[^"]*)"`)

// writeJSON writes a whole answer to the client.
func (s *Server) writeJSON(w http.ResponseWriter, resp *http.Response, body []byte, proto protocol, convertBack bool, model string) {
	if convertBack {
		out, err := backConvert(body, proto, model)
		if err != nil {
			writeError(w, proto, http.StatusBadGateway, err.Error())
			return
		}
		body = out
	}
	copyHeaders(w, resp)
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(resp.StatusCode)
	w.Write(body)
}

// backConvert turns the upstream's Anthropic answer into the shape the
// client speaks.
//
// The upstream answered in Anthropic because the site has no endpoint
// of the client's own: the Codex protocol gets a Responses answer, the
// OpenAI protocol a chat completion.
func backConvert(body []byte, proto protocol, model string) ([]byte, error) {
	switch proto {
	case protocolCodex:
		return convert.ToResponses(body, model)
	case protocolOpenAI:
		return convert.ToChatCompletion(body, model)
	default:
		return body, nil
	}
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
		data = append(data, modelEntry(m))
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"object": "list",
		"data":   data,
	})
}

// retryAfter reads the upstream's Retry-After header.
//
// 它说的是「什么时候再来」：两种写法 —— 一个秒数（"120"），或者一个
// HTTP 日期（"Wed, 21 Oct 2015 07:28:00 GMT"）。读不出来就当没有。
//
// 已经过去的日期是「现在就可以再来」：不是没有，是一个零。
func retryAfter(v string, now time.Time) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0, false
		}
		return time.Duration(secs) * time.Second, true
	}
	t, err := http.ParseTime(v)
	if err != nil {
		return 0, false
	}
	d := t.Sub(now)
	if d < 0 {
		d = 0
	}
	return d, true
}

// backoff is how long to wait before asking again.
//
// The wait grows with the attempt and is short enough that a request
// does not hang for long.
//
// 抖动是加在上面的：形状不能变 —— 半秒、一秒、两秒、四秒，封顶四秒。
func backoff(attempt int) time.Duration {
	d := time.Duration(1<<uint(attempt)) * 500 * time.Millisecond
	d = jitter(d)
	if d > 4*time.Second {
		d = 4 * time.Second
	}
	return d
}

// jitter spreads a wait over a fifth of it, up and down.
//
// 一批请求同时撞上限流的时候，它们会同时退避、同时再问 —— 那是最坏的：
// 每一遍都正好落在同一个限流窗口的边上。加一点随机，它们就散开了。
//
// 上下都是五分之一：最多多等两成，也最多少等两成。
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	span := d / 5
	if span <= 0 {
		return d
	}
	return d - span + time.Duration(rand.Int63n(int64(2*span)+1))
}

// maxWaitForTheSameAccount is how long the gateway waits for an account
// the upstream has just refused.
//
// 上游说「120 秒后再来」的时候，等是等不到的：客户端等不起，那个号也
// 不该被占着一分钟。超过这个数就不等了 —— 换一个号更快。
const maxWaitForTheSameAccount = 10 * time.Second

// waitBeforeRetry says how long to wait before asking the same account
// again.
//
// 上游说了什么时候再来，它说的算：限流窗口里再去敲门就是白烧上游的次数。
// 没有说的话，用自己的退避。
//
// 上游说的太远的时候：不等，换号（第二个返回值是 false）。
func waitBeforeRetry(resp *http.Response, attempt int) (time.Duration, bool) {
	wait := backoff(attempt)
	if resp == nil {
		return wait, true
	}
	d, ok := retryAfter(resp.Header.Get("Retry-After"), time.Now())
	if !ok {
		return wait, true
	}
	if d > maxWaitForTheSameAccount {
		return 0, false
	}
	return d, true
}

// sleepCtx waits, unless the request is over.
//
// 客户端走了就不必再等：等着只是白占着上游的次数和时间，一个请求能
// 让九次上游调用和三十秒的睡觉一起吊着。
//
// It says whether the wait was over: false means the request is over
// and nothing more should be asked upstream.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// clientHeaders gives the headers the client set that are worth
// passing on.
func clientHeaders(r *http.Request) map[string]string {
	out := map[string]string{}
	for _, k := range []string{"anthropic-version", "anthropic-beta", "x-stainless-lang"} {
		if v := r.Header.Get(k); v != "" {
			out[k] = v
		}
	}
	return out
}

// modelOf reads the model out of a request body.
func modelOf(body []byte) string {
	var b struct {
		Model string `json:"model"`
	}
	json.Unmarshal(body, &b)
	return b.Model
}

// isStream says the request wants a stream.
func isStream(body []byte) bool {
	var b struct {
		Stream bool `json:"stream"`
	}
	json.Unmarshal(body, &b)
	return b.Stream
}

// systemOf reads the system prompt out of a request body.
func systemOf(body []byte) string {
	var b struct {
		System json.RawMessage `json:"system"`
	}
	if json.Unmarshal(body, &b) != nil {
		return ""
	}
	return textOf(b.System)
}

// firstUserOf reads the first user message out of a request body.
//
// The three protocols put it in different places, so all of them are
// looked at.
func firstUserOf(body []byte) string {
	var b struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Input json.RawMessage `json:"input"`
	}
	if json.Unmarshal(body, &b) != nil {
		return ""
	}
	for _, m := range b.Messages {
		if m.Role == "user" {
			return textOf(m.Content)
		}
	}
	if len(b.Input) > 0 {
		var s string
		if json.Unmarshal(b.Input, &s) == nil {
			return s
		}
	}
	return ""
}

// textOf reads the text out of a content field, which may be a string
// or a list of parts.
func textOf(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.Text)
	}
	return b.String()
}
