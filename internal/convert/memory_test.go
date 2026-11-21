package convert

import (
	"strings"
	"testing"
)

// A body of 63 MiB: the biggest one the gateway lets in.
const bigBody = 63 << 20

// BenchmarkTheConversionOfABigBody says what a big OpenAI body costs.
//
// 一份 63 MiB 的文字，转成 Anthropic 的形状要重新写一遍 —— 那份是身体
// 的第二份。数字是量出来的，不是猜的。
func BenchmarkTheConversionOfABigBody(b *testing.B) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"` +
		strings.Repeat("x", bigBody) + `"}]}`)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		out, err := ToAnthropicOpenAI(body)
		if err != nil || len(out) == 0 {
			b.Fatal(err)
		}
	}
}

// BenchmarkTheConversionOfABigImage says what a big picture costs.
//
// 图片在 OpenAI 那边是没有的（它被丢掉了），所以出来的那份是小的：身体
// 指着就行，不用抄。
func BenchmarkTheConversionOfABigImage(b *testing.B) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":[` +
		`{"type":"text","text":"what is this?"},` +
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,` +
		strings.Repeat("A", bigBody) + `"}}]}]}`)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		out, err := ToAnthropicOpenAI(body)
		if err != nil || len(out) == 0 {
			b.Fatal(err)
		}
	}
}

// BenchmarkTheCodexConversionOfABigImage says what a big picture costs
// on the Codex road.
func BenchmarkTheCodexConversionOfABigImage(b *testing.B) {
	body := []byte(`{"model":"m","input":[{"type":"message","role":"user","content":[` +
		`{"type":"input_text","text":"what is this?"},` +
		`{"type":"input_image","image_url":"data:image/png;base64,` +
		strings.Repeat("A", bigBody) + `"}]}]}`)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		out, err := ToAnthropic(body)
		if err != nil || len(out) == 0 {
			b.Fatal(err)
		}
	}
}
