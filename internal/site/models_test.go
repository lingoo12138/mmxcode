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
	}
	wants := map[string]want{
		"MiniMax-M3.1-Flash-Preview": {1048576, 524288, "M3.1-Flash-Preview", []string{"text", "image", "video"}, ThinkingForcedOn},
		"MiniMax-M3":                 {1048576, 524288, "M3", []string{"text", "image", "video"}, ThinkingSwitchable},
		"MiniMax-M2.7":               {200000, 128000, "M2.7", []string{"text"}, ThinkingOn},
		"MiniMax-M2.7-highspeed":     {200000, 128000, "M2.7-highspeed", []string{"text"}, ThinkingOn},
	}
	if len(FallbackModels) != len(wants) {
		t.Fatalf("got %d models, want %d", len(FallbackModels), len(wants))
	}
	for _, m := range FallbackModels {
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
	}
}

// TestCloneModelsIsIndependent makes sure a copy really is a copy.
//
// The list is a package-level variable that outlives every request. A
// shallow copy would share the slices inside each model, and a change
// to one model's inputs would change the original's, with nothing in
// the log to point at it.
func TestCloneModelsIsIndependent(t *testing.T) {
	c := CloneModels(FallbackModels)
	c[0].Inputs[0] = "audio"
	c[0].Efforts = append(c[0].Efforts, "extra")
	if FallbackModels[0].Inputs[0] != "text" {
		t.Fatalf("the original's inputs changed to %q", FallbackModels[0].Inputs[0])
	}
	for _, e := range FallbackModels[0].Efforts {
		if e == "extra" {
			t.Fatal("the original's efforts changed")
		}
	}
}

func TestM3ThinkingVariants(t *testing.T) {
	var m Model
	for _, x := range FallbackModels {
		if x.ID == "MiniMax-M3" {
			m = x
		}
	}
	if m.Thinking != ThinkingSwitchable {
		t.Fatalf("M3 thinking = %q", m.Thinking)
	}
	// 规格里 M3 是 switchable：none / high
	if len(m.Efforts) != 2 || m.Efforts[0] != "none" || m.Efforts[1] != "high" {
		t.Fatalf("M3 efforts = %v", m.Efforts)
	}
}
