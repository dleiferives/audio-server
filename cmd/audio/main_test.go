package main

import "testing"

func TestConfigPathFromArgs(t *testing.T) {
	tests := []struct {
		args []string
		want string
	}{
		{nil, "default.yml"},
		{[]string{"-config", "local.yml"}, "local.yml"},
		{[]string{"--config=other.yml"}, "other.yml"},
		{[]string{"-addr", ":9000", "-config=last.yml"}, "last.yml"},
	}
	for _, test := range tests {
		if got := configPathFromArgs(test.args, "default.yml"); got != test.want {
			t.Fatalf("configPathFromArgs(%v) = %q, want %q", test.args, got, test.want)
		}
	}
}

func TestResolveVRAMLimitExplicit(t *testing.T) {
	got, err := resolveVRAMLimit("2048")
	if err != nil || got != 2048 {
		t.Fatalf("got %d, %v", got, err)
	}
	if _, err := resolveVRAMLimit("invalid"); err == nil {
		t.Fatal("expected invalid VRAM limit to fail")
	}
}

func TestModelVRAMOverride(t *testing.T) {
	configured := map[string]int{"supertonic": 768}
	if got := modelVRAM(configured, "supertonic", 1024); got != 768 {
		t.Fatalf("override = %d", got)
	}
	if got := modelVRAM(configured, "parakeet", 1280); got != 1280 {
		t.Fatalf("fallback = %d", got)
	}
}
