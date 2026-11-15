package apikey

import (
	"strings"
	"testing"
)

// TestANewKeyIsDifferentEveryTime says two keys are never the same.
func TestANewKeyIsDifferentEveryTime(t *testing.T) {
	a, err := New()
	if err != nil {
		t.Fatal(err)
	}
	b, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatalf("two keys are the same: %q", a)
	}
	if !strings.HasPrefix(a, prefix) {
		t.Errorf("key = %q, want the %q prefix", a, prefix)
	}
}

// TestTheHashIsTheSameForTheSameKey says a key always hashes the same.
//
// 网关要靠这个认出「来的是不是那一把 key」：请求带来的 key 算一次
// 哈希，库里存的也是一次哈希。
func TestTheHashIsTheSameForTheSameKey(t *testing.T) {
	k, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if Hash(k) != Hash(k) {
		t.Fatal("the same key hashed two ways")
	}
	if Hash(k) == k {
		t.Fatal("the key is stored as it is")
	}
}

// TestTwoKeysDoNotHashTheSame says two keys do not share a hash.
func TestTwoKeysDoNotHashTheSame(t *testing.T) {
	a, _ := New()
	b, _ := New()
	if Hash(a) == Hash(b) {
		t.Fatal("two keys hash the same")
	}
}

// TestTheHashOfNothingIsTheHashOfNothing says an empty key still hashes.
//
// 空 key 不是一把 key：网关拒绝它，不是因为它算不出哈希，是因为它跟
// 配置里那把空的一样，什么都能开。
func TestTheHashOfNothingIsTheHashOfNothing(t *testing.T) {
	if Hash("") != Hash("") {
		t.Fatal("the empty key hashed two ways")
	}
}

// TestTheDisplayIsNotTheKey says what a page shows is not the key.
//
// 页面上要认得出哪一把是哪一把，又不能把它整个摊在那儿。
func TestTheDisplayIsNotTheKey(t *testing.T) {
	k, _ := New()
	d := Display(k)
	if d == k {
		t.Fatal("the display is the whole key")
	}
	if !strings.HasPrefix(d, prefix) {
		t.Errorf("display = %q, want the %q prefix", d, prefix)
	}
}
