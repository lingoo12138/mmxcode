package secret

import (
	"encoding/base64"
	"strings"
	"testing"
)

func testKey() Key {
	return Key("0123456789abcdef0123456789abcdef")
}

func TestSealAndOpen(t *testing.T) {
	k := testKey()
	cipher, err := k.Seal("hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(cipher, "hunter2") {
		t.Fatalf("the password is in the cipher text: %s", cipher)
	}
	plain, err := k.Open(cipher)
	if err != nil {
		t.Fatal(err)
	}
	if plain != "hunter2" {
		t.Fatalf("plain = %q", plain)
	}
}

func TestTwoCiphersOfTheSamePasswordAreDifferent(t *testing.T) {
	k := testKey()
	a, _ := k.Seal("hunter2")
	b, _ := k.Seal("hunter2")
	if a == b {
		t.Fatal("the same password gave the same cipher text")
	}
}

func TestTheWrongKeyDoesNotOpenIt(t *testing.T) {
	cipher, err := testKey().Seal("hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Key("fedcba9876543210fedcba9876543210").Open(cipher); err == nil {
		t.Fatal("the wrong key opened it")
	}
}

func TestFromEnv(t *testing.T) {
	t.Setenv("MMXCODE_TEST_KEY", base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")))
	k, err := FromEnv("MMXCODE_TEST_KEY")
	if err != nil {
		t.Fatal(err)
	}
	if len(k) != 32 {
		t.Fatalf("key is %d bytes, want 32", len(k))
	}
}

func TestFromEnvWithoutTheVariable(t *testing.T) {
	t.Setenv("MMXCODE_TEST_KEY", "")
	if _, err := FromEnv("MMXCODE_TEST_KEY"); err == nil {
		t.Fatal("no error without the variable")
	}
}

func TestFromEnvWithAShortKey(t *testing.T) {
	t.Setenv("MMXCODE_TEST_KEY", base64.StdEncoding.EncodeToString([]byte("short")))
	if _, err := FromEnv("MMXCODE_TEST_KEY"); err == nil {
		t.Fatal("no error with a short key")
	}
}
