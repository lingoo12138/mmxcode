package site

// Thinking says how a model handles thinking.
//
// A type of its own, so a typo like "switchble" does not compile.
type Thinking string

const (
	ThinkingOff        Thinking = "off"
	ThinkingOn         Thinking = "on"
	ThinkingForcedOn   Thinking = "forced_on"
	ThinkingSwitchable Thinking = "switchable"
)

// Model is one model of the site's.
type Model struct {
	// ID is the model's id, as the API takes it.
	ID string
	// Name is what it is called.
	Name string
	// Context is the input token limit.
	Context int
	// Output is the output token limit.
	Output int
	// Inputs are the input kinds it takes.
	Inputs []string
	// Thinking says how it handles thinking.
	Thinking Thinking
	// Efforts are the reasoning levels it takes, when it takes any.
	Efforts []string
}

// fallbackModels is the list used when the live one can't be had.
//
// It is package-private on purpose: a caller that gets a copy can
// change its copy, and there is no way to reach this one and change
// it for everybody.
//
// The context and output limits are the ones the endpoint really allows,
// not the ones MiniMax Code declares. Its model config says 512K/128K for
// the M3 family, but the endpoint accepts 1,048,576 input tokens and
// 524,288 output tokens for every one of them. An agent that believed the
// advertised ones would compact its history about half way too early.
var fallbackModels = []Model{
	{
		ID: "MiniMax-M3.1-Flash-Preview", Name: "M3.1-Flash-Preview",
		Context: 1048576, Output: 524288,
		Inputs:   []string{"text", "image", "video"},
		Thinking: ThinkingForcedOn,
		Efforts:  []string{"low", "medium", "high", "xhigh", "max"},
	},
	{
		// 站点现在叫这个名字：M3.1-Flash-Preview 是它以前的名字，
		// 站点自己给的那个 id 才是客户端要用的。
		ID: "MiniMax-M3.1-Flash", Name: "M3.1-Flash",
		Context: 1048576, Output: 524288,
		Inputs:   []string{"text", "image", "video"},
		Thinking: ThinkingForcedOn,
		Efforts:  []string{"low", "medium", "high", "xhigh", "max"},
	},
	{
		ID: "MiniMax-M3", Name: "M3",
		Context: 1048576, Output: 524288,
		Inputs:   []string{"text", "image", "video"},
		Thinking: ThinkingSwitchable,
		Efforts:  []string{"none", "high"},
	},
	{
		ID: "MiniMax-M2.7", Name: "M2.7",
		Context: 200000, Output: 128000,
		Inputs:   []string{"text"},
		Thinking: ThinkingOn,
	},
	{
		ID: "MiniMax-M2.7-highspeed", Name: "M2.7-highspeed",
		Context: 200000, Output: 128000,
		Inputs:   []string{"text"},
		Thinking: ThinkingOn,
	},
}

// FallbackModels gives the built-in model list.
//
// The caller gets its own copy: the models and the slices inside them.
// Changing what it got back changes nothing for anybody else.
func FallbackModels() []Model { return CloneModels(fallbackModels) }

// Clone copies the model, including the slices inside it, so a caller
// can change it without touching the original.
func (m Model) Clone() Model {
	c := m
	c.Inputs = append([]string(nil), m.Inputs...)
	c.Efforts = append([]string(nil), m.Efforts...)
	return c
}

// verifiedLimit is one model's input and output limits.
type verifiedLimit struct {
	context int
	output  int
}

// verifiedLimits 是接口真的收的数，给那些站点声明得保守的模型用。
//
// 站点的 model config 对 M3 那一族说的是 context 512000 / output
// 128000，那是 MiniMax Code 自己声明的一份保守说法：接口其实收
// 1048576 个输入 token 和 524288 个输出 token。这是实测出来的 ——
// 1048140 个输入 token 的请求是答的，1048700 个被拒（"context window
// exceeds limit (2013)"）；max_tokens 524288 收，600000 拒（"does not
// support max tokens > 524288"）。声明的数对签了名的账号和没签名的账号
// 是一个样，所以它不是哪个套餐的限额。
//
// 照声明的数用，代理会在历史还没满一半的时候就去压缩它。
var verifiedLimits = map[string]verifiedLimit{
	"MiniMax-M3.1-Flash":         {context: 1048576, output: 524288},
	"MiniMax-M3.1-Flash-Preview": {context: 1048576, output: 524288},
	"MiniMax-M3":                 {context: 1048576, output: 524288},
}

// ApplyVerifiedLimits gives the list with the limits the endpoint really
// allows.
//
// 表里有的模型换成实测的数，表里没有的照旧用它自己的数。
//
// 调用方拿到的是一份新的名单：改自己的那份不影响别人。
func ApplyVerifiedLimits(ms []Model) []Model {
	out := CloneModels(ms)
	for i := range out {
		if v, ok := verifiedLimits[out[i].ID]; ok {
			out[i].Context = v.context
			out[i].Output = v.output
		}
	}
	return out
}

// CloneModels copies the list, the models and their slices.
//
// A plain copy of the slice would share the slices inside each model:
// changing one model's inputs would change the original's, in a process
// that runs for days, with nothing in the log to point at it.
func CloneModels(in []Model) []Model {
	out := make([]Model, len(in))
	for i, m := range in {
		out[i] = m.Clone()
	}
	return out
}
