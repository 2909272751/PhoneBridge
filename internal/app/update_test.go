package app

import "testing"

func TestVersionNewer(t *testing.T) {
	for _, sample := range []struct {
		candidate string
		current   string
		want      bool
	}{
		{"1.1.1", "1.1.0", true},
		{"1.2.0", "1.1.9", true},
		{"1.0.0", "1.1.0", false},
		{"1.1.0", "1.1.0", false},
	} {
		if got := versionNewer(sample.candidate, sample.current); got != sample.want {
			t.Fatalf("versionNewer(%q, %q) = %t, want %t", sample.candidate, sample.current, got, sample.want)
		}
	}
}
