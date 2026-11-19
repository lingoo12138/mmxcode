package store

import (
	"context"
	"database/sql/driver"
	"strings"
	"sync"

	"modernc.org/sqlite"
)

// Counter counts the statements the store sends to SQLite.
//
// 它是量具：这个版本要量的是「一个请求打在库上的次数」。数的是语句，不是库的
// 方法 —— 方法的个数说不了往返了几次。
//
// 它是安全的：好几条连接同时数也不会乱。
type Counter struct {
	mu    sync.Mutex
	stmts []string
}

// NewCounter makes a counter that has counted nothing yet.
func NewCounter() *Counter {
	return &Counter{}
}

// Statements gives the statements that were sent, in the order they were
// sent.
func (c *Counter) Statements() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.stmts...)
}

// Count gives how many statements were sent.
func (c *Counter) Count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.stmts)
}

// CountOf gives how many of the statements mention the given text.
//
// 说的是「提到」不是「就是」：一条语句往往一行一行地写，问「哪几条提到了
// sessions」比问「哪几条正好是这一句」有用。
func (c *Counter) CountOf(text string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, s := range c.stmts {
		if strings.Contains(s, text) {
			n++
		}
	}
	return n
}

// Reset throws the count away.
//
// 量一个请求之前要先把准备的开头扔掉：建号、存令牌那些语句不是这个请求
// 打的，它们混在里面量出来的条数就是多的。
func (c *Counter) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stmts = nil
}

// Connector gives the connector that counts into this counter.
//
// 把它交给 WithConnector，store 的每一条连接（常用的那条和丢不得的那条）都从
// 它这里来。
func (c *Counter) Connector() Connector {
	return func(dsn string) (driver.Connector, error) {
		base, err := sqlite.NewConnector(dsn)
		if err != nil {
			return nil, err
		}
		return &countingConnector{base: base, counter: c}, nil
	}
}

// add puts one statement into the count.
func (c *Counter) add(query string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stmts = append(c.stmts, query)
}

// countingConnector counts the statements of the connections it opens.
type countingConnector struct {
	base    driver.Connector
	counter *Counter
}

// Connect opens a counting connection.
func (c *countingConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &countingConn{Conn: conn, counter: c.counter}, nil
}

// Driver gives the driver of the connections underneath.
func (c *countingConnector) Driver() driver.Driver {
	return c.base.Driver()
}

// countingConn counts the statements that go through it.
//
// 它把 SQLite 那些「可选的」接口都转发下去：少转一个，database/sql 就退回那
// 条不带 context 的老路子上（等锁的时候就不认 context 了）。
type countingConn struct {
	driver.Conn
	counter *Counter
}

// Unwrap gives the connection underneath.
//
// SQLite 的 Limit 看着这一层：没有它就看不见底下的连接。
func (c *countingConn) Unwrap() driver.Conn {
	return c.Conn
}

// PrepareContext counts the statement and prepares it.
func (c *countingConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	c.counter.add(query)
	if p, ok := c.Conn.(driver.ConnPrepareContext); ok {
		return p.PrepareContext(ctx, query)
	}
	return c.Conn.Prepare(query)
}

// ExecContext counts the statement and runs it.
func (c *countingConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	e, ok := c.Conn.(driver.ExecerContext)
	if !ok {
		// 底下的连接不会直接跑这一句：让 database/sql 走准备那条路（那边
		// 数过了），免得数两遍。
		return nil, driver.ErrSkip
	}
	c.counter.add(query)
	return e.ExecContext(ctx, query, args)
}

// QueryContext counts the statement and runs it.
func (c *countingConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	q, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	c.counter.add(query)
	return q.QueryContext(ctx, query, args)
}

// BeginTx starts a transaction.
func (c *countingConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if b, ok := c.Conn.(driver.ConnBeginTx); ok {
		return b.BeginTx(ctx, opts)
	}
	return c.Conn.Begin()
}

// Ping checks the connection.
func (c *countingConn) Ping(ctx context.Context) error {
	if p, ok := c.Conn.(driver.Pinger); ok {
		return p.Ping(ctx)
	}
	return nil
}

// ResetSession puts the connection back the way it was.
func (c *countingConn) ResetSession(ctx context.Context) error {
	if r, ok := c.Conn.(driver.SessionResetter); ok {
		return r.ResetSession(ctx)
	}
	return nil
}

// IsValid says whether the connection can be used.
func (c *countingConn) IsValid() bool {
	if v, ok := c.Conn.(driver.Validator); ok {
		return v.IsValid()
	}
	return true
}
