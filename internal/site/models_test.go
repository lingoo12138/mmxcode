package site

import (
	"strings"
	"testing"
)

func TestFallbackModelsLimits(t *testing.T) {
	type want struct {
		ctx, out int
		name     string
		inputs   []string
		thinking Thinking
		efforts  []string
	}
	wants := map[string]want{
		"MiniMax-M3.1-Flash-Preview": {1048576, 524288, "M3.1-Flash-Preview", []string{"text", "image", "video"}, ThinkingForcedOn, []string{"low", "medium", "high", "xhigh", "max"}},
		"MiniMax-M3.1-Flash":         {1048576, 524288, "M3.1-Flash", []string{"text", "image", "video"}, ThinkingForcedOn, []string{"low", "medium", "high", "xhigh", "max"}},
		"MiniMax-M3":                 {1048576, 524288, "M3", []string{"text", "image", "video"}, ThinkingSwitchable, nil},
		"MiniMax-M2.7":               {200000, 128000, "M2.7", []string{"text"}, ThinkingOn, nil},
		"MiniMax-M2.7-highspeed":     {200000, 128000, "M2.7-highspeed", []string{"text"}, ThinkingOn, nil},
	}
	models := FallbackModels()
	if len(models) != len(wants) {
		t.Fatalf("got %d models, want %d", len(models), len(wants))
	}
	for _, m := range models {
		w, ok := wants[m.ID]
		if !ok {
			t.Fatalf("unexpected model %q", m.ID)
		}
		if m.Context != w.ctx || m.Output != w.out {
			t.Fatalf("%s limits = %d/%d, want %d/%d", m.ID, m.Context, m.Output, w.ctx, w.out)
		}
		if m.Name != w.name {
			t.Fatalf("%s name = %q, want %q", m.ID, m.Name, w.name)
		}
		if strings.Join(m.Inputs, ",") != strings.Join(w.inputs, ",") {
			t.Fatalf("%s inputs = %v, want %v", m.ID, m.Inputs, w.inputs)
		}
		if m.Thinking != w.thinking {
			t.Fatalf("%s thinking = %q, want %q", m.ID, m.Thinking, w.thinking)
		}
		if strings.Join(m.Efforts, ",") != strings.Join(w.efforts, ",") {
			t.Fatalf("%s efforts = %v, want %v", m.ID, m.Efforts, w.efforts)
		}
	}
}

// TestCloneModelsIsIndependent makes sure a copy really is a copy.
//
// The list is a package-level variable that outlives every request. A
// shallow copy would share the slices inside each model, and a change
// to one model's inputs would change the original's, with nothing in
// the log to point at it.
func TestCloneModelsIsIndependent(t *testing.T) {
	c := FallbackModels()
	c[0].Inputs[0] = "audio"
	c[0].Efforts[0] = "changed"
	original := FallbackModels()
	if original[0].Inputs[0] != "text" {
		t.Fatalf("the original's inputs changed to %q", original[0].Inputs[0])
	}
	if original[0].Efforts[0] == "changed" {
		t.Fatal("the original's efforts changed")
	}
}

// TestTheVerifiedLimitsWinOverTheAdvertisedOnes says the limits the
// endpoint really allows win over the ones the site declares.
//
// 站点的 model config 对 M3 那一族说的是 512000/128000，那是 MiniMax
// Code 自己声明的一份保守说法：接口其实收 1048576 个输入 token 和
// 524288 个输出 token。照声明的数用，代理会在历史还没满一半的时候就去
// 压缩它。
func TestTheVerifiedLimitsWinOverTheAdvertisedOnes(t *testing.T) {
	for _, id := range []string{"MiniMax-M3.1-Flash", "MiniMax-M3.1-Flash-Preview", "MiniMax-M3"} {
		in := []Model{{ID: id, Context: 512000, Output: 128000}}
		out := ApplyVerifiedLimits(in)
		if len(out) != 1 {
			t.Fatalf("%s: got %d models, want 1", id, len(out))
		}
		if out[0].Context != 1048576 || out[0].Output != 524288 {
			t.Fatalf("%s limits = %d/%d, want 1048576/524288", id, out[0].Context, out[0].Output)
		}
		// 调用方自己那份不能被改：改自己的不能影响别人。
		if in[0].Context != 512000 || in[0].Output != 128000 {
			t.Fatalf("%s: the caller's own list changed to %d/%d", id, in[0].Context, in[0].Output)
		}
	}
}

// TestAModelThatIsNotVerifiedKeepsItsOwnLimits says a model that is not
// in the verified table keeps the limits the site gave it.
func TestAModelThatIsNotVerifiedKeepsItsOwnLimits(t *testing.T) {
	in := []Model{{ID: "MiniMax-M2.7", Context: 200000, Output: 128000}}
	out := ApplyVerifiedLimits(in)
	if len(out) != 1 {
		t.Fatalf("got %d models, want 1", len(out))
	}
	if out[0].Context != 200000 || out[0].Output != 128000 {
		t.Fatalf("limits = %d/%d, want 200000/128000", out[0].Context, out[0].Output)
	}
}

// TestM3ThinkingVariants says M3's thinking is switchable and that it
// names no effort levels.
//
// "none" 和 "default" 不是档位，是占位。参考插件读活名单的时候就把它们
// 滤掉了（minimax/index.mjs 里 parseModels 的那一行 filter），它自己的
// 内置名单里 M3 写的也是 efforts: []。
//
// 名单里留一个叫 "none" 的档位是有害的：插件那边先摆 none = 把思考关掉
// 那一档，再遍历档位表 —— 档位表里有个叫 "none" 的，就把「关掉思考」
// 那一档盖成「adaptive 思考 + effort=none」。选 none 的人以为关了思考，
// 其实没关。
func TestM3ThinkingVariants(t *testing.T) {
	var m Model
	for _, x := range FallbackModels() {
		if x.ID == "MiniMax-M3" {
			m = x
		}
	}
	if m.Thinking != ThinkingSwitchable {
		t.Fatalf("M3 thinking = %q", m.Thinking)
	}
	if len(m.Efforts) != 0 {
		t.Fatalf("M3 efforts = %v, want none: \"none\" is not an effort level", m.Efforts)
	}
}
