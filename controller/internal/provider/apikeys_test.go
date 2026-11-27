package provider

import (
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// D-76: a key is read-only whatever its role: every write action is refused
// for every key role, every read follows the role matrix, and a key with
// the super-admin role (which the DB refuses anyway) gets nothing.
func TestDecide_APIKeyReadOnly(t *testing.T) {
	consts := actionConstants(t)
	for _, role := range append(append([]string{}, APIKeyRoles...), RoleSuperAdmin, "", "admin") {
		key := Actor{Kind: ActorKindAPIKey, Role: role, KeyID: "k"}
		person := Actor{Role: role}
		for name, action := range consts {
			got := decide(key, action, Target{}) == nil
			want := role != RoleSuperAdmin && !IsWriteAction(action) && decide(person, action, Target{}) == nil
			if got != want {
				t.Errorf("key role %q %s (%s): allowed=%v, want %v", role, name, action, got, want)
			}
			if !got && !errors.Is(decide(key, action, Target{}), ErrForbidden) {
				t.Errorf("key role %q %s: not ErrForbidden", role, name)
			}
		}
		// Unknown actions count as writes.
		if decide(key, "relay.new_verb", Target{}) == nil {
			t.Errorf("key role %q allowed an unknown action", role)
		}
	}
	// Keys never manage keys or operators (super-admin only, session only).
	for _, a := range []string{ActionAPIKeyRead, ActionAPIKeyCreate, ActionAPIKeyRevoke, ActionProviderUserManage} {
		for _, role := range Roles[1:] {
			if decide(Actor{Role: role}, a, Target{}) == nil {
				t.Errorf("%s may %s", role, a)
			}
		}
	}
}

func TestAPIKeyFormat(t *testing.T) {
	id := "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0"
	key, prefix, sum, err := generateAPIKey(id)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^zpk_[0-9a-f]{32}_[A-Za-z0-9_-]{43}$`).MatchString(key) || prefix != key[:12] || len(sum) != 32 {
		t.Fatalf("key %q prefix %q", key, prefix)
	}
	if got, ok := parseAPIKey(key); !ok || got != id {
		t.Fatalf("parse: %q %v", got, ok)
	}
	key2, _, sum2, _ := generateAPIKey(id)
	if key2 == key || sum2 == sum {
		t.Fatal("two keys for one id are equal")
	}
	for _, bad := range []string{"", "zpk_", "zpk_abc_def", key + "x", key[:len(key)-1], strings.ToUpper(key[:36]) + key[36:],
		"zpk_" + strings.Repeat("g", 32) + key[36:], "zpx" + key[3:], strings.Replace(key, "_", "-", 2)} {
		if _, ok := parseAPIKey(bad); ok {
			t.Errorf("parsed %q", bad)
		}
	}
	if !IsAPIKeyToken(key) || IsAPIKeyToken("eyJhbGciOi") {
		t.Fatal("IsAPIKeyToken")
	}
}

func TestValidateNewAPIKey(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	ok, err := ValidateNewAPIKey("  ci  ", RoleBilling, now.Add(364*24*time.Hour), now, []string{"192.168.1.77/24", "2001:db8::1/64"})
	if err != nil || ok.Name != "ci" || ok.AllowedCIDRs[0] != "192.168.1.0/24" || ok.AllowedCIDRs[1] != "2001:db8::/64" {
		t.Fatalf("valid: %+v %v", ok, err)
	}
	for name, c := range map[string]struct {
		name, role string
		exp        time.Time
		cidrs      []string
		want       error
	}{
		"super-admin":  {"x", RoleSuperAdmin, now.Add(time.Hour), nil, ErrAPIKeyInvalidRole},
		"empty role":   {"x", "", now.Add(time.Hour), nil, ErrAPIKeyInvalidRole},
		"365 days":     {"x", RoleAuditor, now.Add(APIKeyMaxLifetime), nil, ErrAPIKeyInvalidExpiry},
		"now":          {"x", RoleAuditor, now, nil, ErrAPIKeyInvalidExpiry},
		"long name":    {strings.Repeat("a", 65), RoleAuditor, now.Add(time.Hour), nil, ErrAPIKeyInvalidName},
		"control char": {"a\nb", RoleAuditor, now.Add(time.Hour), nil, ErrAPIKeyInvalidName},
		"bad cidr":     {"x", RoleAuditor, now.Add(time.Hour), []string{"10.0.0.0/33"}, ErrAPIKeyInvalidCIDR},
		"bare ip":      {"x", RoleAuditor, now.Add(time.Hour), []string{"10.0.0.1"}, ErrAPIKeyInvalidCIDR},
		"too many":     {"x", RoleAuditor, now.Add(time.Hour), make([]string, APIKeyMaxCIDRs+1), ErrAPIKeyInvalidCIDR},
	} {
		if _, err := ValidateNewAPIKey(c.name, c.role, c.exp, now, c.cidrs); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
}

func TestIPAllowed(t *testing.T) {
	for _, c := range []struct {
		ip    string
		cidrs []string
		want  bool
	}{
		{"1.2.3.4", nil, true},
		{"10.1.2.3", []string{"10.0.0.0/8"}, true},
		{"11.1.2.3", []string{"10.0.0.0/8"}, false},
		{"::ffff:10.1.2.3", []string{"10.0.0.0/8"}, true},
		{"not-an-ip", []string{"10.0.0.0/8"}, false},
		{"", []string{"10.0.0.0/8"}, false},
	} {
		if got := ipAllowed(c.ip, c.cidrs); got != c.want {
			t.Errorf("ipAllowed(%q, %v) = %v", c.ip, c.cidrs, got)
		}
	}
}

func TestTokenBucketLimiter(t *testing.T) {
	l := NewTokenBucketLimiter(1, 2)
	t0 := time.Unix(1000, 0)
	for i, want := range []bool{true, true, false} {
		if ok, err := l.Allow("k", t0); err != nil || ok != want {
			t.Fatalf("call %d: %v %v", i, ok, err)
		}
	}
	if ok, _ := l.Allow("other", t0); !ok {
		t.Fatal("keys share a bucket")
	}
	if ok, _ := l.Allow("k", t0.Add(time.Second)); !ok {
		t.Fatal("no refill after 1 s")
	}
	if _, err := (&TokenBucketLimiter{}).Allow("k", t0); err == nil {
		t.Fatal("an unconfigured limiter must fail (closed)")
	}
}

func TestActorAudit(t *testing.T) {
	key := Actor{Kind: ActorKindAPIKey, KeyID: "kid", Email: "api_key:x (zpk_12345678)", Role: RoleSupport}
	if key.AuditUserID() != nil {
		t.Fatal("a key has an audit user id")
	}
	d := map[string]any{"view": "detail"}
	if got := key.AuditDetails(d); got["key_id"] != "kid" || got["view"] != "detail" || len(d) != 1 {
		t.Fatalf("key audit details %v (input changed: %v)", got, d)
	}
	person := Actor{UserID: "u1", Role: RoleSupport}
	if id := person.AuditUserID(); id == nil || *id != "u1" {
		t.Fatal("person audit user id")
	}
	if got := person.AuditDetails(d); got["key_id"] != nil {
		t.Fatal("person audit details got a key_id")
	}
}

// Static guards: the key code compares hashes in constant time, and no key
// code path can mint a session.
func TestAPIKeyCode_StaticGuards(t *testing.T) {
	src, err := os.ReadFile("apikeys.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	if !strings.Contains(s, "subtle.ConstantTimeCompare(sum[:], row.sum)") {
		t.Error("apikeys.go must compare the key hash with subtle.ConstantTimeCompare")
	}
	for _, bad := range []string{"bytes.Equal", "string(row.sum)", "row.sum ==", "== row.sum"} {
		if strings.Contains(s, bad) {
			t.Errorf("apikeys.go: non-constant-time comparison %q", bad)
		}
	}
	for _, f := range []string{"apikeys.go", "apikey_handlers.go", "../middleware/provider.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range []string{"IssueSession", "IssueMFAToken", "IssuePasswordChangeToken"} {
			if strings.Contains(string(b), bad+"(") {
				t.Errorf("%s calls %s: a key must never mint a session", f, bad)
			}
		}
	}
}
