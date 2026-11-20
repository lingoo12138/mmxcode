package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"modernc.org/sqlite"
)

// The result codes SQLite says when the file is busy.
//
// 5 是 SQLITE_BUSY（别人占着写锁），6 是 SQLITE_LOCKED。延长的结果码
// （SQLITE_BUSY_SNAPSHOT 之类）低八位还是这两个。
const (
	sqliteBusy   = 5
	sqliteLocked = 6
)

// The waits of a write that must not be lost.
//
// 写锁多半只被人占着几毫秒（一次写、一个事务），等一下就拿到了。占得久
// 的是导入和迁移 —— 那不是等得久就能等到的，所以等的工夫有个顶，超了顶就
// 再来一遍：占着锁的那件事中间会让出来。
const (
	// busyBudget is how long the store keeps trying, in all.
	busyBudget = 5 * time.Second
	// busyPause is how long the store waits between two tries.
	busyPause = 50 * time.Millisecond
	// busyTriesMax is how many times at most a write is tried.
	busyTriesMax = 5
)

// Busy says the failure is the file being busy.
//
// 它按结果码认：驱动说的忙是 *sqlite.Error，结果码 5（SQLITE_BUSY）或 6
// （SQLITE_LOCKED）。延长的结果码（517 之类）低八位还是这两个。
//
// 不是驱动说的错（错被包过一层，类型丢了）就看那句话里有没有 SQLite 自己
// 的名字 —— "SQLITE_BUSY"、"SQLITE_LOCKED"。不看「locked」「busy」这两个
// 词：别的毛病也会带着它们（"the server is busy"）。
func Busy(err error) bool {
	if err == nil {
		return false
	}
	var se *sqlite.Error
	if errors.As(err, &se) {
		code := se.Code() & 0xff
		return code == sqliteBusy || code == sqliteLocked
	}
	msg := err.Error()
	return strings.Contains(msg, "SQLITE_BUSY") || strings.Contains(msg, "SQLITE_LOCKED")
}

// writeBusy does a write that must not be lost, and does it again when the
// library says the file was busy.
//
// 一遍等的是连接上那个 busy_timeout（丢不得的那两笔走的那条连接上是
// 一秒）。占着写锁的那件事要是短命（导入的时候，一笔账一个事务，中间的
// 空当只有几毫秒），库的忙处理器越等越粗（一开始 1 毫秒，后来 100 毫秒），
// 那个空当它就错过了 —— 再来一遍是新的忙处理器，它又从 1 毫秒开始等。
//
// 一共 busyTries 遍，几遍加起来是 busyBudget 那个顶。超了顶就还是那个错，
// 只是说得清楚：库忙着呢。
//
// 调用方的 context 到了就停：再试也是白试。
func (s *Store) writeBusy(ctx context.Context, db execer, query string, args ...any) error {
	var err error
	for i := 0; i < s.busyTries(); i++ {
		if i > 0 && !sleepCtx(ctx, busyPause) {
			break
		}
		_, err = db.ExecContext(ctx, query, args...)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil || !Busy(err) {
			return err
		}
	}
	return err
}

// busyTries is how many times a write that must not be lost is tried.
//
// 一遍等的是连接上那个 busy_timeout：一遍等满了还没拿到锁，占着锁的那件
// 事不是等一秒就能做完的。总共的工夫有顶（busyBudget），所以遍数是顶除
// 以一遍的工夫 —— 而且有个上限（最多 busyTriesMax 遍）。
func (s *Store) busyTries() int {
	if s.busyWait <= 0 {
		// The wait is not known: one try and no more.
		return 1
	}
	n := int(busyBudget / s.busyWait)
	if n > busyTriesMax {
		n = busyTriesMax
	}
	if n < 1 {
		n = 1
	}
	return n
}

// sleepCtx waits, and says whether the wait happened.
//
// The context ending stops the wait.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
