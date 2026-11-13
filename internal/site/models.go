package site

// Thinking says how a model handles thinking.
const (
	ThinkingOff        = "off"
	ThinkingOn         = "on"
	ThinkingForcedOn   = "forced_on"
	ThinkingSwitchable = "switchable"
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
	Thinking string
	// Efforts are the reasoning levels it takes, when it takes any.
	Efforts []string
}

// FallbackModels is the list used when the live one can't be had.
//
// It is read-only: a caller that wants to change it copies it first.
//
// The context and output limits are the ones the endpoint really allows,
// not the ones MiniMax Code declares. Its model config says 512K/128K for
// the M3 family, but the endpoint accepts 1,048,576 input tokens and
// 524,288 output tokens for every one of them. An agent that believed the
// advertised ones would compact its history about half way too early.
var FallbackModels = []Model{
	{
		ID: "MiniMax-M3.1-Flash-Preview", Name: "M3.1-Flash-Preview",
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

// Limits returns a model's context and output limits.
func (m Model) Limits() (ctxLimit, outputLimit int) { return m.Context, m.Output }
