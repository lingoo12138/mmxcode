package store

import (
	"context"
	"database/sql/driver"
	"strings"
	"sync"
	"testing"
	"time"

	"modernc.org/sqlite"
)

// gate holds one statement still.
//
// 它是给并发用的：让某一个语句停在半路上（结果已经算出来了，还没交给别
// 人），这样「谁先谁后」是确定的，不是靠运气。
type gate struct {
	// arrived is closed when the held statement has run.
	arrived chan struct{}
	// release is closed to let the answer through.
	release chan struct{}
	// once guards arrived.
	once sync.Once
	// mu guards hit.
	mu sync.Mutex
	// hit says the statement has been held already.
	hit bool
}

// hold says this statement is the one to hold still.
//
// 只有第一条提到的语句会被卡住：卡住的是「查 key 的那一条」。
func (g *gate) hold(query string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.hit || !strings.Contains(query, "FROM api_keys") {
		return false
	}
	g.hit = true
	// 它到了：外面可以开始安排「谁先谁后」了。
	g.once.Do(func() { close(g.arrived) })
	return true
}

// gateConnector opens the connections that hold statements still.
type gateConnector struct {
	base driver.Connector
	g    *gate
}

// Connect opens a gated connection.
func (c *gateConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &gateConn{Conn: conn, g: c.g}, nil
}

// Driver gives the driver of the connections underneath.
func (c *gateConnector) Driver() driver.Driver {
	return c.base.Driver()
}

// gateConn holds statements still.
type gateConn struct {
	driver.Conn
	g *gate
}

// QueryContext runs the query and holds its answer still.
func (c *gateConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	q, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	rows, err := q.QueryContext(ctx, query, args)
	if err != nil {
		return nil, err
	}
	// 语句已经跑了：那一读的快照已经定下来了。现在把答案卡住。
	if c.g.hold(query) {
		select {
		case <-c.g.release:
		case <-ctx.Done():
			rows.Close()
			return nil, ctx.Err()
		}
	}
	return rows, nil
}

// PrepareContext prepares the statement.
func (c *gateConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if p, ok := c.Conn.(driver.ConnPrepareContext); ok {
		return p.PrepareContext(ctx, query)
	}
	return c.Conn.Prepare(query)
}

// ExecContext runs the statement.
func (c *gateConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if e, ok := c.Conn.(driver.ExecerContext); ok {
		return e.ExecContext(ctx, query, args)
	}
	return nil, driver.ErrSkip
}

// BeginTx starts a transaction.
func (c *gateConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if b, ok := c.Conn.(driver.ConnBeginTx); ok {
		return b.BeginTx(ctx, opts)
	}
	return c.Conn.Begin()
}

// Ping checks the connection.
func (c *gateConn) Ping(ctx context.Context) error {
	if p, ok := c.Conn.(driver.Pinger); ok {
		return p.Ping(ctx)
	}
	return nil
}

// ResetSession puts the connection back the way it was.
func (c *gateConn) ResetSession(ctx context.Context) error {
	if r, ok := c.Conn.(driver.SessionResetter); ok {
		return r.ResetSession(ctx)
	}
	return nil
}

// IsValid says whether the connection can be used.
func (c *gateConn) IsValid() bool {
	if v, ok := c.Conn.(driver.Validator); ok {
		return v.IsValid()
	}
	return true
}

// Unwrap gives the connection underneath.
func (c *gateConn) Unwrap() driver.Conn {
	return c.Conn
}

// TestTheAnswerIsNotStaleWhileItIsBeingRead says the answer is not the old
// one when a key is made while the answer is being read.
//
// 这一条是**确定性**的：那个读被卡在半路上（查库的结果已经出来了，还没
// 交给别人），所以「谁先谁后」不是靠运气。
//
// 少了那把锁的时候是这样：key 造好了，通知也发了，然后那个读把旧答案
// （「一把都没有」）写了回去 —— 那之后每个请求都看不见那把 key，门就一直
// 开着。
func TestTheAnswerIsNotStaleWhileItIsBeingRead(t *testing.T) {
	ctx := context.Background()
	g := &gate{arrived: make(chan struct{}), release: make(chan struct{})}
	s := openTestOpts(t, WithConnector(func(dsn string) (driver.Connector, error) {
		base, err := sqlite.NewConnector(dsn)
		if err != nil {
			return nil, err
		}
		return &gateConnector{base: base, g: g}, nil
	}))

	// 那个读：查库的结果出来了，卡在半路上。
	read := make(chan bool, 1)
	go func() {
		have, err := s.HasAPIKeys(ctx)
		if err != nil {
			t.Error(err)
		}
		read <- have
	}()
	<-g.arrived

	// 有人申请了一把 key。
	created := make(chan struct{})
	go func() {
		defer close(created)
		if err := s.CreateAPIKey(ctx, APIKey{
			ID: "key_1", Name: "n", KeyHash: "h", Prefix: "p", CreatedAt: time.Now(),
		}); err != nil {
			t.Error(err)
		}
	}()

	// 等那把 key 真的进了库。
	for i := 0; i < 200; i++ {
		var n int
		if err := s.db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM api_keys WHERE revoked_at IS NULL").Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	// 通知该发了。锁是对的时候它卡在锁上（那个读还拿着锁）；锁是坏的时候
	// 它已经发过了 —— 那一下是纳秒级的，等它一下。
	time.Sleep(50 * time.Millisecond)

	// 放那个读走。
	close(g.release)
	<-created
	<-read

	// 申请完了之后的答案：得是「有 key」。
	have, err := s.HasAPIKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !have {
		t.Fatal("a key was made and the store still says there is none: the old answer was written back")
	}
}
