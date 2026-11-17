package relay

import "testing"

// F11-2: an override is reported once when it starts and once when it ends,
// not on every list build.
func TestOverrideLog_ReportsChangesOnly(t *testing.T) {
	l := newOverrideLog()
	same := func(got []string, want ...string) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}
	if in, out := l.observe(map[string]bool{"b": true, "a": true}); !same(in, "a", "b") || !same(out) {
		t.Fatalf("first build: entered %v left %v", in, out)
	}
	if in, out := l.observe(map[string]bool{"a": true, "b": true}); !same(in) || !same(out) {
		t.Fatalf("unchanged build must report nothing: entered %v left %v", in, out)
	}
	if in, out := l.observe(map[string]bool{"a": true, "c": true}); !same(in, "c") || !same(out, "b") {
		t.Fatalf("changed build: entered %v left %v", in, out)
	}
	if in, out := l.observe(map[string]bool{}); !same(in) || !same(out, "a", "c") {
		t.Fatalf("all ended: entered %v left %v", in, out)
	}
}
