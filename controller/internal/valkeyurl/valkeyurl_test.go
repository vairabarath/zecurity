package valkeyurl

import "testing"

func TestParse(t *testing.T) {
	cases := []struct {
		raw  string
		want Options
	}{
		{"redis://localhost:6379", Options{Addr: "localhost:6379", DB: 0}},
		{"redis://localhost:6379/", Options{Addr: "localhost:6379", DB: 0}},
		{"redis://localhost:6379/0", Options{Addr: "localhost:6379", DB: 0}},
		{"redis://localhost:6379/15", Options{Addr: "localhost:6379", DB: 15}}, // CI's AUTH_TEST_VALKEY_URL
		{"redis://valkey:6380/3", Options{Addr: "valkey:6380", DB: 3}},
		{"redis://10.0.0.5:6379/1", Options{Addr: "10.0.0.5:6379", DB: 1}},
		{"redis://[::1]:6379/2", Options{Addr: "[::1]:6379", DB: 2}},
		{"redis://user:secret@localhost:6379/4", Options{Addr: "localhost:6379", DB: 4}}, // userinfo ignored, as before
		{"redis://:p@ss@word@host:1/5", Options{Addr: "host:1", DB: 5}},                  // last @ ends userinfo
		{"redis://localhost", Options{Addr: "localhost", DB: 0}},
	}
	for _, c := range cases {
		got, err := Parse(c.raw)
		if err != nil || got != c.want {
			t.Errorf("Parse(%q) = %+v, %v; want %+v", c.raw, got, err, c.want)
		}
	}
}

func TestParse_Rejects(t *testing.T) {
	for _, raw := range []string{
		"",
		"localhost:6379",
		"http://localhost:6379",
		"rediss://localhost:6379", // TLS scheme not supported here
		"redis://",
		"redis:///3",
		"redis://user@/3",
		"redis://localhost:6379/-1",
		"redis://localhost:6379/abc",
		"redis://localhost:6379/1/2",
		"redis://localhost:6379/15?timeout=1s",
		"redis://localhost:6379#frag",
	} {
		if got, err := Parse(raw); err == nil {
			t.Errorf("Parse(%q) = %+v, want an error", raw, got)
		}
	}
}
