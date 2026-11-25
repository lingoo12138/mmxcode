package convert

// anthropicUsage is what an answer says about its tokens.
//
// The fields are pointers on purpose: the upstream says the numbers
// twice in a stream — once at the start and once at the end — and "not
// said" is not the same as "said zero". The one that is said last wins.
type anthropicUsage struct {
	// InputTokens is the number of new input tokens: the ones that did
	// not come out of the cache.
	InputTokens *int `json:"input_tokens"`
	// OutputTokens is the number of tokens the answer came out in.
	OutputTokens *int `json:"output_tokens"`
	// CacheCreationInputTokens is the number of tokens that went into
	// the cache on this call.
	CacheCreationInputTokens *int `json:"cache_creation_input_tokens"`
	// CacheReadInputTokens is the number of tokens that came out of the
	// cache.
	CacheReadInputTokens *int `json:"cache_read_input_tokens"`
}

// tokenCounts is what the upstream said about the tokens of an answer.
type tokenCounts struct {
	// input is the number of new input tokens.
	input int
	// output is the number of tokens the answer came out in.
	output int
	// cacheRead is the number of tokens that came out of the cache.
	cacheRead int
	// cacheWrite is the number of tokens that went into the cache.
	cacheWrite int
}

// take folds one usage into the counts.
//
// 上游在一段流里说两遍这些数：开头一块说一遍，末尾一块再说一遍。末尾
// 说的算数 —— MiniMax 的流开头把整个提示词的长度报在
// cache_read_input_tokens 里（1366），末尾才说清哪一部分是新的
// （1252）、哪一部分命中了缓存（114）。
func (c *tokenCounts) take(u *anthropicUsage) {
	if u == nil {
		return
	}
	if u.InputTokens != nil {
		c.input = *u.InputTokens
	}
	if u.OutputTokens != nil {
		c.output = *u.OutputTokens
	}
	if u.CacheCreationInputTokens != nil {
		c.cacheWrite = *u.CacheCreationInputTokens
	}
	if u.CacheReadInputTokens != nil {
		c.cacheRead = *u.CacheReadInputTokens
	}
}

// counts reads the numbers out of one usage.
func (u *anthropicUsage) counts() tokenCounts {
	var c tokenCounts
	c.take(u)
	return c
}

// totalInput is how many tokens went in, the cached ones included.
//
// Anthropic says how many tokens were new and how many came out of the
// cache; OpenAI and the Responses protocol say one number: the total.
// 客户端要的是总数（提示词有多大），命中了多少在 details 里另说。
func (c *tokenCounts) totalInput() int {
	return c.input + c.cacheWrite + c.cacheRead
}

// chatUsage is the usage of an OpenAI chat completion.
//
// prompt_tokens 是总数：命中缓存的那些字在里头，prompt_tokens_details
// 说其中有多少命中了缓存。这既是 OpenAI 自己的样子，也是 MiniMax 那条
// OpenAI 路的说法（它的 prompt_tokens 就是输入的总字数）。
func chatUsage(c tokenCounts) map[string]any {
	total := c.totalInput()
	return map[string]any{
		"prompt_tokens":     total,
		"completion_tokens": c.output,
		"total_tokens":      total + c.output,
		"prompt_tokens_details": map[string]any{
			"cached_tokens": c.cacheRead,
		},
	}
}

// responsesUsage is the usage of a Responses answer.
//
// 数还是那些数，只是换个名字：总数叫 input_tokens，命中数在
// input_tokens_details 里。
func responsesUsage(c tokenCounts) map[string]any {
	total := c.totalInput()
	return map[string]any{
		"input_tokens":  total,
		"output_tokens": c.output,
		"total_tokens":  total + c.output,
		"input_tokens_details": map[string]any{
			"cached_tokens": c.cacheRead,
		},
	}
}
