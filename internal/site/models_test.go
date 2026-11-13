package site

import "testing"

func TestFallbackModelsLimits(t *testing.T) {
	want := map[string][2]int{
		"MiniMax-M3.1-Flash-Preview": {1048576, 524288},
		"MiniMax-M3":                 {1048576, 524288},
		"MiniMax-M2.7":               {200000, 128000},
		"MiniMax-M2.7-highspeed":     {200000, 128000},
	}
	if len(FallbackModels) != len(want) {
		t.Fatalf("got %d models, want %d", len(FallbackModels), len(want))
	}
	for _, m := range FallbackModels {
		w, ok := want[m.ID]
		if !ok {
			t.Fatalf("unexpected model %q", m.ID)
		}
		c, o := m.Limits()
		if c != w[0] || o != w[1] {
			t.Fatalf("%s limits = %d/%d, want %d/%d", m.ID, c, o, w[0], w[1])
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
