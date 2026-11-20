package store

import (
	"context"
	"testing"
	"time"
)

// TestTheNotesWithAPrefixAreCleared says the notes that start with a
// prefix are cleared, and the others are left alone.
//
// 旧版本写在账号上的记号说的是一句今天已经不存在的话，挂着它是骗人的
// （页面上显示着「这个号有问题」，而那个号其实好得很）。擦的时候只擦自
// 己认得的那一条前缀：别的理由说的是别的事，擦了就是把真话也藏起来。
func TestTheNotesWithAPrefixAreCleared(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	now := time.Now()
	for _, id := range []string{"one", "two", "three"} {
		makeAccount(t, s, id)
	}
	if err := s.SetAccountLastError(ctx, "one",
		"the token was written down but it was not made durable: a power cut could lose it", now); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAccountLastError(ctx, "two",
		"the token was written down but it was not made durable: flush the database: it is busy", now); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAccountLastError(ctx, "three",
		"the renewal failed but the token in hand is still good", now); err != nil {
		t.Fatal(err)
	}

	n, err := s.ClearAccountNotes(ctx, "the token was written down but it was not made durable")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("the number of notes cleared = %d, want 2", n)
	}

	for _, id := range []string{"one", "two"} {
		a, err := s.GetAccount(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if a.LastError != "" {
			t.Errorf("the note on %s is still there: %q", id, a.LastError)
		}
	}
	other, err := s.GetAccount(ctx, "three")
	if err != nil {
		t.Fatal(err)
	}
	if other.LastError == "" {
		t.Error("the note of another reason was cleared")
	}
}

// TestTheNotesWithAPrefixAreClearedOnlyOnce says the second time there is
// nothing to clear.
//
// 擦过一遍就干净了：开机的时候擦一遍就够，擦第二遍是白擦（也是白拿一
// 把写锁）。
func TestTheNotesWithAPrefixAreClearedOnlyOnce(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	now := time.Now()
	makeAccount(t, s, "one")
	if err := s.SetAccountLastError(ctx, "one",
		"the token was written down but it was not made durable: a power cut could lose it", now); err != nil {
		t.Fatal(err)
	}
	n, err := s.ClearAccountNotes(ctx, "the token was written down but it was not made durable")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("the number of notes cleared = %d, want 1", n)
	}
	n, err = s.ClearAccountNotes(ctx, "the token was written down but it was not made durable")
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("the number of notes cleared the second time = %d, want 0", n)
	}
}
