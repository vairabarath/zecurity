package posture

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestNormalizeMinVersion(t *testing.T) {
	for _, ok := range []string{"10", "10.0.22631", " 14.2 ", "10.0.26100.1742"} {
		if _, err := NormalizeMinVersion(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "x", "10.", ".10", "10..1", "1.2.3.4.5", "-1", "10.0b", "1234567890"} {
		if _, err := NormalizeMinVersion(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestCompareVersions(t *testing.T) {
	v := func(s string) []uint64 { p, _ := parseVersion(s); return p }
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"14", "14.0", 0},
		{"14.1", "14", 1},
		{"10.0.22631", "10.0.26100", -1},
		{"10.0.26300.9711", "10.0.26100", 1},
		{"9", "12", -1}, // numeric, not lexical
	} {
		if got := compareVersions(v(tc.a), v(tc.b)); got != tc.want {
			t.Errorf("compare(%s,%s) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestReportedOSVersion(t *testing.T) {
	raw, _ := json.Marshal(osInfo{Name: "Windows", Version: "10.0.26300.9711"})
	if got := reportedOSVersion(raw); got != "10.0.26300.9711" {
		t.Fatalf("got %q", got)
	}
	if got := reportedOSVersion(nil); got != "" {
		t.Fatalf("empty os_info gave %q", got)
	}
}

// A Minimum OS Version requirement applies to its platform only and fails
// closed on an older, unknown or unparsable version.
func TestMinOSVersionRequirement(t *testing.T) {
	now := time.Now()
	p := Profile{ID: uuid.New(), Revision: 1, AllowedPlatforms: []string{"windows", "android"}}
	reqs := []Requirement{
		{CheckID: CheckWindowsMinOS, MinVersion: "10.0.22631"},
		{CheckID: CheckAndroidMinOS, MinVersion: "13"},
	}
	obs := func(version string) map[string]Observation {
		m := map[string]Observation{}
		for _, id := range []string{CheckWindowsMinOS, CheckAndroidMinOS, CheckIOSMinOS} {
			m[id] = osVersionObservation(uuid.New(), version)
		}
		return m
	}
	for _, tc := range []struct {
		name, platform, version string
		ok                      bool
		reason                  string
	}{
		{"windows newer", "windows", "10.0.26300.9711", true, "all requirements satisfied"},
		{"windows equal", "windows", "10.0.22631", true, "all requirements satisfied"},
		{"windows older", "windows", "10.0.19045.4529", false, "older than 10.0.22631"},
		{"windows unknown", "windows", "unknown", false, "is unknown"},
		{"windows empty", "windows", "", false, "is unknown"},
		{"android older", "android", "12", false, "older than 13"},
		{"android newer", "android", "14", true, "all requirements satisfied"},
	} {
		got := EvaluateProfile(now, now, p, tc.platform, reqs, obs(tc.version))
		if got.Satisfied != tc.ok || !strings.Contains(got.Reason, tc.reason) {
			t.Errorf("%s: satisfied=%v reason=%q, want %v %q", tc.name, got.Satisfied, got.Reason, tc.ok, tc.reason)
		}
	}
	// A requirement with no stored minimum never passes.
	bad := EvaluateProfile(now, now, p, "windows", []Requirement{{CheckID: CheckWindowsMinOS}}, obs("10.0.26300"))
	if bad.Satisfied {
		t.Fatal("a requirement without a minimum passed")
	}
}

// Clients can never report a Minimum OS Version result themselves.
func TestMinOSChecksAreServerOnly(t *testing.T) {
	for _, id := range []string{CheckWindowsMinOS, CheckAndroidMinOS, CheckIOSMinOS} {
		if _, ok := recognizedChecks[id]; ok {
			t.Fatalf("%s must not be client-reportable", id)
		}
		if !IsServerCheck(id) || !IsRecognizedCheck(id) {
			t.Fatalf("%s must be a requirable server check", id)
		}
	}
	if checkPlatform(CheckIOSMinOS) != "ios" || checkPlatform(CheckWindowsMinOS) != "windows" {
		t.Fatal("min OS checks must belong to their platform")
	}
}
