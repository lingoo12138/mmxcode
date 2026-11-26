package convert

import "strings"

// outputConfig is the Anthropic output configuration.
//
// 这里只有一件事要紧：effort —— 思考多深（low / medium / high / xhigh /
// max）。MiniMax 那条 Anthropic 路就是用它说深度的。
type outputConfig struct {
	Effort string `json:"effort,omitempty"`
}

// thinkingFor is what a client's thinking settings become on the way to
// MiniMax.
//
// MiniMax 那条 Anthropic 路用两个字段说思考：thinking.type 说开不开
// （adaptive / disabled），output_config.effort 说多深。
//
// 客户端的话有好几种说法，翻译成这两个字段：
//
//   - 深度那几档（low / medium / high / xhigh / max）原样搬过去。
//   - minimal 是 OpenAI 那边的说法，MiniMax 没有这一档：它比 low 还浅，
//     落到 low 上。
//   - none 是「不要思考」：那不是一档深度，是把思考关掉。M3 真的就不了；
//     M3.1-Flash 总是想，上游会自己答 400 —— 跟它自己的 OpenAI/Responses
//     端点一个脾气。
//   - 不认识的词原样送过去，让上游自己答：我们不替它猜。
//
// thinkingType 是客户端明说的开不开（OpenAI 那边叫 thinking.type，
// Anthropic 那边也是它）。空的时候看深度：客户端说了深度就是要它思考。
func thinkingFor(thinkingType, effort string) (*thinking, *outputConfig) {
	t := strings.ToLower(strings.TrimSpace(thinkingType))
	e := strings.ToLower(strings.TrimSpace(effort))
	switch {
	case t == "disabled":
		// 客户端明说了不思考，深度就没什么可说的了。
		return &thinking{Type: "disabled"}, nil
	case e == "none":
		return &thinking{Type: "disabled"}, nil
	case t == "adaptive":
		return &thinking{Type: "adaptive"}, effortFor(effort)
	case t == "":
		if e == "" {
			// 什么都没说：M3 那就不思考，M3.1-Flash 总是想 —— 都由
			// 上游定，网关不替它决定。
			return nil, nil
		}
		return &thinking{Type: "adaptive"}, effortFor(effort)
	default:
		// 不认识的 type 原样送过去，让上游自己答。
		return &thinking{Type: t}, effortFor(effort)
	}
}

// effortFor is the depth a client's words become.
//
// minimal 落到 low 上（MiniMax 的枚举里没有 minimal，而它比 low 还浅）。
// 不认识的词原样送过去，让上游自己答。
func effortFor(effort string) *outputConfig {
	switch e := strings.ToLower(strings.TrimSpace(effort)); e {
	case "":
		return nil
	case "minimal":
		return &outputConfig{Effort: "low"}
	case "low", "medium", "high", "xhigh", "max":
		return &outputConfig{Effort: e}
	default:
		return &outputConfig{Effort: strings.TrimSpace(effort)}
	}
}
