package posture

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// ADR-031: per-platform evaluation.
func TestEvaluateProfilePerPlatform(t *testing.T) {
	now := time.Now().UTC()
	pass := func(id string) Observation { return Observation{CheckID: id, Status: ObservationStatusPass} }
	fail := func(id string) Observation { return Observation{CheckID: id, Status: ObservationStatusFail} }
	both := Profile{ID: uuid.New(), Revision: 2, AllowedPlatforms: []string{"linux", "android"}}
	linuxOnly := Profile{ID: uuid.New(), Revision: 2, AllowedPlatforms: []string{"linux"}}
	none := Profile{ID: uuid.New(), Revision: 2}
	reqs := []Requirement{{CheckID: CheckLUKS}, {CheckID: CheckAndroidScreenLock}}

	tests := []struct {
		name     string
		profile  Profile
		platform string
		reqs     []Requirement
		obs      map[string]Observation
		ok       bool
		reason   string
	}{
		{"android device: only android checks apply", both, "android", reqs,
			map[string]Observation{CheckAndroidScreenLock: pass(CheckAndroidScreenLock)}, true, "all requirements satisfied"},
		{"linux device: only linux checks apply", both, "linux", reqs,
			map[string]Observation{CheckLUKS: pass(CheckLUKS)}, true, "all requirements satisfied"},
		{"android check fails", both, "android", reqs,
			map[string]Observation{CheckAndroidScreenLock: fail(CheckAndroidScreenLock)}, false, CheckAndroidScreenLock},
		{"platform switched off", linuxOnly, "android", reqs,
			map[string]Observation{CheckAndroidScreenLock: pass(CheckAndroidScreenLock)}, false, "platform android is not allowed"},
		{"no platform allowed (empty profile stays closed)", none, "linux", nil,
			map[string]Observation{}, false, "platform linux is not allowed"},
		{"allowed platform with nothing required passes", both, "android", nil,
			map[string]Observation{}, true, "all requirements satisfied"},
		{"unknown platform fails closed", both, "", reqs,
			map[string]Observation{}, false, "device platform unknown"},
		{"platform outside the profile fails", both, "windows", nil,
			map[string]Observation{}, false, "platform windows is not allowed"},
		{"server check applies to every platform", both, "android", []Requirement{{CheckID: CheckDeviceApproved}},
			map[string]Observation{CheckDeviceApproved: fail(CheckDeviceApproved)}, false, CheckDeviceApproved},
		{"unrecognized check fails closed on every platform", both, "android", []Requirement{{CheckID: "future.check"}},
			map[string]Observation{}, false, "missing required check future.check"},
		{"platform is case-insensitive", both, " Android ", nil,
			map[string]Observation{}, true, "all requirements satisfied"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EvaluateProfile(now, now, tt.profile, tt.platform, tt.reqs, tt.obs)
			if got.Satisfied != tt.ok || !strings.Contains(got.Reason, tt.reason) {
				t.Fatalf("satisfied=%v reason=%q, want %v containing %q", got.Satisfied, got.Reason, tt.ok, tt.reason)
			}
		})
	}

	stale := EvaluateProfile(now, now.Add(-MaxReportAge-time.Second), both, "android", nil, map[string]Observation{})
	if stale.Satisfied || !strings.Contains(stale.Reason, "stale") {
		t.Fatalf("an allowed platform with nothing required still needs a fresh report: %+v", stale)
	}
}

func TestNormalizePlatforms(t *testing.T) {
	got, err := NormalizePlatforms([]string{"Android", "linux", "android", " ios "})
	if err != nil || !reflect.DeepEqual(got, []string{"linux", "android", "ios"}) {
		t.Fatalf("got %v, %v", got, err)
	}
	if got, err := NormalizePlatforms(nil); err != nil || len(got) != 0 {
		t.Fatalf("empty: %v, %v", got, err)
	}
	if _, err := NormalizePlatforms([]string{"symbian"}); !errors.Is(err, ErrUnknownPlatform) {
		t.Fatalf("unknown platform: %v", err)
	}
}

func TestAndroidChecksAreRecognized(t *testing.T) {
	for _, id := range []string{CheckAndroidEncryption, CheckAndroidScreenLock, CheckAndroidBiometric} {
		if _, ok := recognizedChecks[id]; !ok {
			t.Fatalf("%s must be a recognized client check", id)
		}
		if checkPlatform(id) != "android" {
			t.Fatalf("%s must belong to android", id)
		}
	}
}
