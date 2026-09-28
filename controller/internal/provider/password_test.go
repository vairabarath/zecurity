package provider

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/argon2"
)

func TestArgon2id_HashVerify_PHC(t *testing.T) {
	ctx := context.Background()
	phc, err := HashPassword(ctx, "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(phc, "$argon2id$v=19$m=65536,t=3,p=2$") {
		t.Fatalf("unexpected PHC format: %s", phc)
	}
	ok, err := VerifyPassword(ctx, "correct horse battery", phc)
	if err != nil || !ok {
		t.Fatalf("verify correct password: ok=%v err=%v", ok, err)
	}
	ok, err = VerifyPassword(ctx, "wrong horse battery", phc)
	if err != nil || ok {
		t.Fatalf("verify wrong password: ok=%v err=%v", ok, err)
	}

	again, err := HashPassword(ctx, "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if again == phc {
		t.Fatal("two hashes of the same password must differ (random salt)")
	}
}

// Verification reads the parameters from the stored string, so hashes made
// with other (e.g. older, cheaper) parameters keep verifying after a bump.
func TestVerifyPassword_UsesStoredParameters(t *testing.T) {
	salt := []byte("0123456789abcdef")
	key := argon2.IDKey([]byte("legacy-password-1"), salt, 1, 8*1024, 1, 32)
	phc := encodePHC(8*1024, 1, 1, salt, key)

	ok, err := VerifyPassword(context.Background(), "legacy-password-1", phc)
	if err != nil || !ok {
		t.Fatalf("stored-parameter verify: ok=%v err=%v", ok, err)
	}
}

func TestVerifyPassword_MalformedHash(t *testing.T) {
	for _, bad := range []string{
		"",
		"plaintext",
		"$argon2i$v=19$m=65536,t=3,p=2$c2FsdHNhbHQ$a2V5a2V5a2V5a2V5a2V5",    // wrong variant
		"$argon2id$v=16$m=65536,t=3,p=2$c2FsdHNhbHQ$a2V5a2V5a2V5a2V5a2V5",   // wrong version
		"$argon2id$v=19$m=9999999,t=3,p=2$c2FsdHNhbHQ$a2V5a2V5a2V5a2V5a2V5", // memory above bound
		"$argon2id$v=19$m=65536,t=0,p=2$c2FsdHNhbHQ$a2V5a2V5a2V5a2V5a2V5",   // zero time
		"$argon2id$v=19$m=65536,t=3,p=2$!!!$a2V5a2V5a2V5a2V5a2V5",           // bad base64
	} {
		if ok, err := VerifyPassword(context.Background(), "x", bad); ok || !errors.Is(err, errMalformedHash) {
			t.Errorf("%q: ok=%v err=%v, want errMalformedHash", bad, ok, err)
		}
	}
}

func TestPasswordPolicy(t *testing.T) {
	const email = "ops@inkyank.com"
	cases := []struct {
		name, pw, current string
		wantOK            bool
	}{
		{"too short", "short-pw-11", "", false},
		{"minimum", "exactly-12ch", "", true},
		{"maximum", strings.Repeat("a", 128), "", true},
		{"too long", strings.Repeat("a", 129), "", false},
		{"counts runes not bytes", strings.Repeat("é", 12), "", true},
		{"equals email", "OPS@inkyank.com", "", false},
		{"equals current", "same-password-1", "same-password-1", false},
		{"differs from current", "new-password-12", "old-password-12", true},
	}
	for _, c := range cases {
		err := ValidateNewPassword(c.pw, email, c.current)
		if c.wantOK && err != nil {
			t.Errorf("%s: want ok, got %v", c.name, err)
		}
		if !c.wantOK && !errors.Is(err, ErrPasswordPolicy) {
			t.Errorf("%s: want ErrPasswordPolicy, got %v", c.name, err)
		}
	}
}

func TestGenerateTemporaryPassword(t *testing.T) {
	a, err := GenerateTemporaryPassword()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := GenerateTemporaryPassword()
	if len(a) != temporaryPasswordLen || a == b {
		t.Fatalf("temporary passwords: %q %q", a, b)
	}
	for _, r := range a {
		if !strings.ContainsRune(temporaryPasswordAlphabet, r) {
			t.Fatalf("unexpected character %q", r)
		}
	}
	if err := ValidateNewPassword(a, "ops@inkyank.com", ""); err != nil {
		t.Fatalf("temporary password must satisfy the policy: %v", err)
	}
}

func TestVerifyDummy(t *testing.T) {
	if err := VerifyDummy(context.Background(), "anything-at-all"); err != nil {
		t.Fatal(err)
	}
}

// With every slot taken, a hash waits hashSlotWait and then fails with
// ErrHashBusy; once a slot frees up, hashing proceeds.
func TestPasswordHash_ConcurrencyCap(t *testing.T) {
	origWait := hashSlotWait
	hashSlotWait = 50 * time.Millisecond
	t.Cleanup(func() { hashSlotWait = origWait })

	var releases []func()
	for i := 0; i < providerMaxConcurrentHashes; i++ {
		release, err := acquireHashSlot(context.Background())
		if err != nil {
			t.Fatalf("slot %d: %v", i, err)
		}
		releases = append(releases, release)
	}

	start := time.Now()
	if _, err := HashPassword(context.Background(), "blocked-password"); !errors.Is(err, ErrHashBusy) {
		t.Fatalf("want ErrHashBusy with all %d slots held, got %v", providerMaxConcurrentHashes, err)
	}
	if waited := time.Since(start); waited < hashSlotWait {
		t.Fatalf("returned after %v, before the %v wait", waited, hashSlotWait)
	}

	releases[0]()
	if _, err := HashPassword(context.Background(), "unblocked-password"); err != nil {
		t.Fatalf("hash after a slot was released: %v", err)
	}
	for _, release := range releases[1:] {
		release()
	}
}
