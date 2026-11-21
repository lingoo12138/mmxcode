package convert

import (
	"bytes"
	"encoding/json"
)

// nullBytes is the JSON null.
var nullBytes = []byte("null")

// rawJSON is a piece of JSON as it stands in the body.
//
// 它记的是身体里原来的那一段字节，不是抄一份：抄一份是几十兆的白抄。
// 身体在转换的整个过程里都在，所以指着它的那一段，一个字节也不用多。
type rawJSON []byte

// UnmarshalJSON keeps the bytes as they are.
func (r *rawJSON) UnmarshalJSON(b []byte) error {
	*r = b
	return nil
}

// sizeOf estimates how many bytes an Anthropic request will take.
//
// 缓冲开够了就不用翻倍着长大：翻倍是白抄 —— 一份 63 MiB 的文字，一路翻
// 倍抄过去要 126 MiB。开够了就只抄一遍。
//
// 估的是留下来的文字，不是身体：一张几十兆的图片在 OpenAI 那边是没有的
// （它被丢掉了），身体再大，出来的那份也是小的 —— 按身体估就是白开一块
// 几十兆的缓冲。
func sizeOf(out anthropicRequest) int {
	n := 1024 + len(out.Model) + len(out.System)
	for _, m := range out.Messages {
		n += len(m.Role) + 16
		for _, b := range m.Content {
			n += len(b.Text) + len(b.Content) + len(b.ID) + len(b.Name) +
				len(b.ToolUseID) + len(b.Input) + 64
		}
	}
	// 转义要多占一点地方：换行是两个字节。
	return n + n/8
}

// marshalTo writes the value as JSON into a buffer that is big enough from
// the start.
//
// json.Marshal 自己开一块缓冲，一路翻倍着长大 —— 一个 63 MiB 的身体，它
// 要分配 126 MiB 的缓冲，最后再抄一份 63 MiB 的出来。这里先把缓冲开够，
// 抄的那份也省了。
//
// 写出来的东西跟 json.Marshal 的**一个字节都不差**：同样的编码，同样的
// 转义。
func marshalTo(v any, size int) ([]byte, error) {
	if size < 1024 {
		size = 1024
	}
	buf := bytes.NewBuffer(make([]byte, 0, size+1024))
	enc := json.NewEncoder(buf)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	// Encode 会在后面添一个换行：请求的身体不要那个。
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}
