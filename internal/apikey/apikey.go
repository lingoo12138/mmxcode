package apikey

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// The prefix a key carries.
//
// 站点自己的 key 也是这个开头：网关不认它，只是让人一眼看出这是一把
// key，而不是别的一串字符。
const prefix = "sk-"

// New makes a key.
//
// 32 个字节的乱数，base64url 编出来。key 猜不出来：256 位的乱数不是
// 能穷举的，而它又是这个网关唯一认的东西。
func New() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("random: %w", err)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// Hash gives the hash of a key.
//
// 库里存的是哈希，不是 key 本身：数据库被人拿走的时候，里面没有一把能
// 用的 key。
//
// 哈希是 SHA-256：key 自己有 256 位的乱数，慢哈希在这里什么也换不来
// ——慢哈希是为了猜密码的，不是为了猜乱数的。
func Hash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// Display gives the part of a key that may be shown.
//
// 它是给人看的：让人认得出「这一把是我刚才那把」，又认不出它能开门。
// 前八个字符加上开头，够认了。
func Display(key string) string {
	if len(key) <= len(prefix)+8 {
		return key
	}
	return key[:len(prefix)+8] + "…"
}
