package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"mmxcode/internal/config"
	"mmxcode/internal/manager"
	"mmxcode/internal/rotator"
	"mmxcode/internal/store"
)

// TestAnAccountTakesOneRequestAtATime says an account takes one
// request at a time.
//
// 两个请求落在一个账号上，额度掉得就快一倍：它们用的是同一份额度。
func TestAnAccountTakesOneRequestAtATime(t *testing.T) {
	b := newBusyAccounts(1)
	if !b.take("acc_1") {
		t.Fatal("the account did not take the request")
	}
	if b.take("acc_1") {
		t.Fatal("the account took a second request")
	}
	b.give("acc_1")
	if !b.take("acc_1") {
		t.Fatal("the account did not take the request again")
	}
}

// TestTheTotalConcurrencySaysHowMany says the total concurrency for a
// number of accounts.
//
// 总并发 = max(floor(可用账号数 / 5), min(可用账号数, 3))
func TestTheTotalConcurrencySaysHowMany(t *testing.T) {
	cfg := config.Default().Concurrency
	for _, c := range []struct{ usable, want int }{
		{1, 1}, {3, 3}, {5, 3}, {12, 3}, {100, 20},
	} {
		if got := totalFor(c.usable, cfg); got != c.want {
			t.Errorf("the total concurrency for %d accounts = %d, want %d",
				c.usable, got, c.want)
		}
	}
}

// TestTheGateLetsTheRequestsIn says the gate holds the requests that
// fit.
//
// 超了的请求排队等：等不到（客户端自己超时了）就 429 带 Retry-After。
func TestTheGateLetsTheRequestsIn(t *testing.T) {
	g := &gate{}
	if !g.take(2) {
		t.Fatal("the first request did not get in")
	}
	if !g.take(2) {
		t.Fatal("the second request did not get in")
	}
	if g.take(2) {
		t.Fatal("a third request got in under a limit of two")
	}
	g.give()
	if !g.take(2) {
		t.Fatal("the request did not get in after one was done")
	}
}

// TestTheGateWaitsForAPlace says a request waits for a free place.
//
// 只有 1 个账号的时候它忙，第二个请求要等它做完，而不是报错。
func TestTheGateWaitsForAPlace(t *testing.T) {
	g := &gate{}
	if !g.take(1) {
		t.Fatal("the first request did not get in")
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		g.give()
	}()
	if !g.waitFor(context.Background(), 1, time.Second) {
		t.Fatal("the request did not wait for a place")
	}
	g.give()
}

// TestTheBusyAccountIsNotTaken says an account with a request in the
// air on it is not taken.
//
// 在飞的账号再接一个请求，额度掉得就快一倍：它们用的是同一份额度。
func TestTheBusyAccountIsNotTaken(t *testing.T) {
	b := newBusyAccounts(1)
	if !b.take("acc_1") {
		t.Fatal("the account did not take the request")
	}
	if !b.isBusy("acc_1") {
		t.Fatal("the account does not say it is busy")
	}
	if b.isBusy("acc_2") {
		t.Fatal("the account that is not in the air says it is busy")
	}
	b.give("acc_1")
	if b.isBusy("acc_1") {
		t.Fatal("the account still says it is busy")
	}
}

// TestTheRequestIsAnsweredByTheAccountThatIsNotBusy says a request goes
// to an account that has nothing in the air on it.
//
// 三个账号上都有请求在飞，第四个空着：请求要落到第四个身上，不是 502。
// 忙不是它不能答了的理由（规格 §6.1、§6.4）。
func TestTheRequestIsAnsweredByTheAccountThatIsNotBusy(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"type":"message","content":[]}`))
	})
	ctx := context.Background()
	now := time.Now()
	for _, id := range []string{"acc_02", "acc_03", "acc_04"} {
		if err := h.store.CreateAccount(ctx, store.Account{
			ID: id, Name: id, Status: store.StatusActive, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
		if err := h.store.SaveToken(ctx, store.Token{
			AccountID: id, Access: "tok" + id, Refresh: "ref",
			ExpiresAt: now.Add(time.Hour), UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	// 前三个账号上都有一个请求在飞。
	for _, id := range []string{"acc_01", "acc_02", "acc_03"} {
		if !h.gw.busy.take(id) {
			t.Fatalf("the account %s did not take the request", id)
		}
		defer h.gw.busy.give(id)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	// 答的是那个空着的账号。
	h.mu.Lock()
	tokens := append([]string(nil), h.tokens...)
	h.mu.Unlock()
	if len(tokens) != 1 {
		t.Fatalf("the upstream was asked %d times, want once", len(tokens))
	}
	if !strings.Contains(tokens[0], "tokacc_04") {
		t.Fatalf("the upstream was asked with %q, want the account that is not busy", tokens[0])
	}
}

// TestTheSecondRequestWaitsForTheBusyAccount says the second request
// waits for the busy account instead of being given up on.
//
// 只有一个账号的时候它忙，第二个请求要等它做完，而不是报错（规格
// §6.4）。等到了就答上，等不到才是 429。
func TestTheSecondRequestWaitsForTheBusyAccount(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"type":"message","content":[]}`))
	})
	// 唯一的账号上有一个请求在飞：它一会儿就做完。
	if !h.gw.busy.take("acc_01") {
		t.Fatal("the account did not take the request")
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		h.gw.busy.give("acc_01")
	}()

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
}

// stealingStore takes an account while the rotator is still choosing.
//
// 它站在挑和抢中间那道缝里：rotator 挑的时候那个账号还闲，等网关去抢
// 的时候它已经被别的请求抢走了。这道缝平时很小，测试把它撑开。
type stealingStore struct {
	poolStore
	// busy counts the requests in the air per account.
	busy *busyAccounts
	// stolen is the account that was taken.
	stolen string
}

// BindSession binds the session, and takes the account it was bound to.
func (s *stealingStore) BindSession(ctx context.Context, sessionKey, accountID string) error {
	if err := s.poolStore.BindSession(ctx, sessionKey, accountID); err != nil {
		return err
	}
	if s.stolen != "" {
		return nil
	}
	if s.busy.take(accountID) {
		s.stolen = accountID
	}
	return nil
}

// TestTheRequestWaitsForAnAccountThatIsNotBusy says a request waits for
// an account that can answer, and not for the whole gateway to be idle.
//
// 一个账号在飞、另一个空着的时候，等的是那个空着的账号。等不到就回
// 429 说"every account is busy right now"，那句话是假的：网关里明明有
// 一个账号能答（规格 §6.4）。
//
// 账本只记着在飞的那几个账号，它不知道池子里有多少账号：所以"还有没
// 有空的账号"只能问 rotator。
func TestTheRequestWaitsForAnAccountThatIsNotBusy(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"type":"message","content":[]}`))
	})
	// 等不了多久：这个测试等的是那个「等不到」。
	h.gw.Cfg.Concurrency.Wait = config.Duration(100 * time.Millisecond)
	ctx := context.Background()
	now := time.Now()
	// 第二个账号：它空着。
	if err := h.store.CreateAccount(ctx, store.Account{
		ID: "acc_02", Name: "b", Status: store.StatusActive, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SaveToken(ctx, store.Token{
		AccountID: "acc_02", Access: "tokacc_02", Refresh: "ref",
		ExpiresAt: now.Add(time.Hour), UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	// 挑的时候那个账号还闲，等网关去抢的时候它已经被抢走了。
	steal := &stealingStore{poolStore: h.store, busy: h.gw.busy}
	h.gw.Rot = rotator.New(poolAdapter{s: steal, busy: h.gw.busy}, rotator.Options{
		MaxAttempts: h.gw.Cfg.Rotation.MaxAttempts,
		Cooldown:    h.gw.Cfg.Rotation.ExhaustedCooldown.Duration(),
		WindowGroup: manager.WindowGroup,
		Strategy:    h.gw.Cfg.Rotation.Strategy,
	})
	defer func() { h.gw.busy.give(steal.stolen) }()

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	if steal.stolen == "" {
		t.Fatal("the account was not taken: the test did not do what it says")
	}
	// 答的是那个空着的账号，不是被抢走的那个。
	h.mu.Lock()
	tokens := append([]string(nil), h.tokens...)
	h.mu.Unlock()
	if len(tokens) != 1 {
		t.Fatalf("the upstream was asked %d times, want once", len(tokens))
	}
	if strings.Contains(tokens[0], steal.stolen) {
		t.Fatalf("the upstream was asked with %q, want the account that is free", tokens[0])
	}
}

// TestTheRequestIsToldToWaitWhenEveryAccountIsBusy says a request that
// cannot get a free account is told to come back.
//
// 等不到就是等不到：客户端自己超时了，网关不该把它吊着（规格 §6.2）。
// 那个答案是 429 带 Retry-After，不是 502：账号只是忙，不是坏了。
func TestTheRequestIsToldToWaitWhenEveryAccountIsBusy(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"type":"message","content":[]}`))
	})
	// 等不了多久：这个测试等的是那个「等不到」。
	h.gw.Cfg.Concurrency.Wait = config.Duration(100 * time.Millisecond)
	// 唯一的账号上有一个请求在飞，它一直不来。
	if !h.gw.busy.take("acc_01") {
		t.Fatal("the account did not take the request")
	}
	defer h.gw.busy.give("acc_01")

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("no Retry-After was given")
	}
}
