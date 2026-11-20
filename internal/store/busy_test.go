package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// withBusyTimeout says how long one write waits for the file's write lock.
//
// 测试用的：等锁的工夫在测试里等不起（一遍一秒多，一趟测试五秒，谁都受
// 不了）。
func withBusyTimeout(d time.Duration) Option {
	return func(o *options) { o.busyTimeout = d }
}

// openBusyTest opens a store whose writes wait a short time for the write
// lock, and gives the path of the file it is on.
//
// 另一个连接要占着那个锁，所以路径是给它的。
func openBusyTest(t *testing.T, wait time.Duration) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "busy.db")
	s, err := Open(path, withBusyTimeout(wait))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s, path
}

// holdTheWriteLock takes the file's write lock and keeps it until the
// returned function is called.
//
// 真库里另一个进程占着写锁就是这个样子：一个写事务开着，什么也不写。
func holdTheWriteLock(t *testing.T, path string) func() {
	t.Helper()
	db, err := sql.Open("sqlite", dsn(path, options{}))
	if err != nil {
		t.Fatal(err)
	}
	// The data source name says _txlock=immediate, so the transaction
	// takes the write lock when it starts.
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	return func() {
		_ = tx.Rollback()
		db.Close()
	}
}

// TestTheWriteThatMustNotBeLostIsTriedAgainWhenTheFileIsBusy says the
// write that must not be lost is tried again when the library says the
// file is busy.
//
// 写锁多半只被人占着几毫秒（一次写、一个事务），等一下就拿到了 —— 可是
// 「等一下」是库那个忙处理器的事：它越等越粗（一开始 1 毫秒，后来 100 毫
// 秒），占着锁的那件事中间让出来的空当（导入的时候，一笔账一个事务）它
// 就错过了。再来一遍就是新的忙处理器，它又从 1 毫秒开始等。
func TestTheWriteThatMustNotBeLostIsTriedAgainWhenTheFileIsBusy(t *testing.T) {
	ctx := context.Background()
	s, path := openBusyTest(t, 50*time.Millisecond)
	makeAccount(t, s, "acc_01")
	release := holdTheWriteLock(t, path)

	done := make(chan error, 1)
	go func() {
		done <- s.SaveToken(ctx, Token{
			AccountID: "acc_01", Access: "a", Refresh: "r",
			ExpiresAt: time.Now().Add(time.Hour), UpdatedAt: time.Now(),
		})
	}()
	// 让头一遍等满（它会失败），后面那几遍还在等的时候把锁放开。
	time.Sleep(200 * time.Millisecond)
	release()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the token was not written: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the write did not finish")
	}
	tok, err := s.GetToken(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if tok.Access != "a" {
		t.Errorf("the token in the store = %q, want %q", tok.Access, "a")
	}
}

// TestTheWriteFailsWhenTheFileStaysBusy says the write gives up when the
// file stays busy, and that the error says so.
//
// 锁一直被占着的时候，再试也是白等：那个错还是那个错，只是说得清楚。
func TestTheWriteFailsWhenTheFileStaysBusy(t *testing.T) {
	ctx := context.Background()
	s, path := openBusyTest(t, 50*time.Millisecond)
	makeAccount(t, s, "acc_01")
	release := holdTheWriteLock(t, path)
	defer release()

	err := s.SaveToken(ctx, Token{
		AccountID: "acc_01", Access: "a", Refresh: "r",
		ExpiresAt: time.Now().Add(time.Hour), UpdatedAt: time.Now(),
	})
	if err == nil {
		t.Fatal("the token was written while the file was busy")
	}
	if !Busy(err) {
		t.Errorf("the error = %v, want the one that says the file is busy", err)
	}
}

// TestBusyIsNotReadFromTheWords says the library's result code says the
// file is busy, not the words in the message.
//
// 「locked」「busy」这两个词别的毛病也会带着（"the server is busy"）。按
// 结果码认才不会把别的毛病当成忙。
func TestBusyIsNotReadFromTheWords(t *testing.T) {
	if Busy(errors.New("the server is busy")) {
		t.Error("an error that only says busy is not the file being busy")
	}
	if Busy(errors.New("the table is locked by the reader")) {
		t.Error("an error that only says locked is not the file being busy")
	}
	if !Busy(errors.New("database is locked (5) (SQLITE_BUSY)")) {
		t.Error("the library's own words say the file is busy")
	}
}

// TestTheDurablePoolHasRoomForTwoBlockedWrites says two writes do not
// queue up behind each other.
//
// 一条连接的时候，第二个写是在 Go 这边排队的：等第一条放弃了（等满了
// busy_timeout）它才拿到连接，然后自己再等一遍 —— 量出来是 5 秒和 7.8
// 秒。两条连接的时候，两个写一起在库的忙处理器里等，一起结束。
func TestTheDurablePoolHasRoomForTwoBlockedWrites(t *testing.T) {
	s := openTestOpts(t, WithSynchronous("normal"))
	if got := s.durable.Stats().MaxOpenConnections; got != 2 {
		t.Errorf("the durable pool has %d connections, want 2", got)
	}
}
