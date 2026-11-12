package auditexport

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func s3cfg(t *testing.T, c S3Config) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

var goodS3 = S3Config{Bucket: "zecurity-audit", Region: "eu-west-1", Prefix: "siem/prod", AccessKeyID: "AKIAIOSFODNN7EXAMPLE"}

func TestS3_ValidateConfig(t *testing.T) {
	ok := func(c S3Config) {
		t.Helper()
		if err := (S3{}).ValidateConfig(s3cfg(t, c)); err != nil {
			t.Errorf("%+v: %v", c, err)
		}
	}
	bad := func(c S3Config, why string) {
		t.Helper()
		if err := (S3{}).ValidateConfig(s3cfg(t, c)); err == nil {
			t.Errorf("%s: %+v accepted", why, c)
		}
	}
	ok(goodS3)
	c := goodS3
	c.Prefix = ""
	ok(c)
	c = goodS3
	c.Endpoint = "https://minio.example.com:9000"
	ok(c)

	for _, b := range []string{"", "ab", "UPPER", "-lead", "trail-", "a..b", "has_underscore", strings.Repeat("a", 64)} {
		c = goodS3
		c.Bucket = b
		bad(c, "bucket "+b)
	}
	for _, r := range []string{"", "EU-WEST-1", "eu west 1", "x"} {
		c = goodS3
		c.Region = r
		bad(c, "region "+r)
	}
	for _, p := range []string{"/lead", "trail/", "a//b", "a/../b", "a/./b", "sp ace", "plus+", "q?x", strings.Repeat("a", 257)} {
		c = goodS3
		c.Prefix = p
		bad(c, "prefix "+p)
	}
	for _, k := range []string{"", "ab", "AKIA-DASH", strings.Repeat("A", 129)} {
		c = goodS3
		c.AccessKeyID = k
		bad(c, "access key "+k)
	}
	for _, e := range []string{"http://minio.example.com", "https://x.example.com/path", "https://x.example.com?a=b", "https://u:p@x.example.com", "minio:9000"} {
		c = goodS3
		c.Endpoint = e
		bad(c, "endpoint "+e)
	}
	if err := (S3{}).ValidateConfig(json.RawMessage(`{"bucket":"zecurity-audit","region":"eu-west-1","access_key_id":"AKIAX","secret":"x"}`)); err == nil {
		t.Error("unknown field accepted")
	}
}

func TestS3_DeriveURL(t *testing.T) {
	for _, c := range []struct {
		cfg  S3Config
		want string
	}{
		{goodS3, "https://zecurity-audit.s3.eu-west-1.amazonaws.com"},
		{S3Config{Bucket: "audit.example.com", Region: "us-east-1", AccessKeyID: "AKIAX"}, "https://s3.us-east-1.amazonaws.com/audit.example.com"},
		{S3Config{Bucket: "audit", Region: "us-east-1", AccessKeyID: "minioadmin", Endpoint: "https://minio.example.com:9000/"}, "https://minio.example.com:9000/audit"},
	} {
		got, err := (S3{}).DeriveURL(s3cfg(t, c.cfg))
		if err != nil || got != c.want {
			t.Errorf("%+v: got %q, %v; want %q", c.cfg, got, err, c.want)
		}
		if err := ValidateURL(got, false); err != nil {
			t.Errorf("derived URL %q fails ValidateURL: %v", got, err)
		}
	}
}

func s3Batch(seqs ...int64) Batch {
	b := Batch{DeliveryID: "d-1", WorkspaceID: "616015cb-8c8f-49fc-a6ae-b50fbfdb1bf6", Stream: "admin"}
	for _, s := range seqs {
		b.Events = append(b.Events, Event{WorkspaceID: b.WorkspaceID, Stream: "admin", Seq: s,
			RecordedAt: "2026-11-06T07:59:59.123456Z", Action: "auth.sign_in", Outcome: "success", Details: map[string]any{"html": "<b>&"}})
	}
	return b
}

func TestS3_ObjectKey(t *testing.T) {
	now := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	if got, want := S3ObjectKey("siem/prod", s3Batch(76, 77, 78), now),
		"siem/prod/zecurity/v1/616015cb-8c8f-49fc-a6ae-b50fbfdb1bf6/admin/2026/11/06/07/76-78.jsonl.gz"; got != want {
		t.Errorf("key\n got %s\nwant %s", got, want)
	}
	if got := S3ObjectKey("", s3Batch(5), now); !strings.HasPrefix(got, "zecurity/v1/") || !strings.HasSuffix(got, "/5-5.jsonl.gz") {
		t.Errorf("no prefix: %s", got)
	}
	b := Batch{DeliveryID: "d-9", WorkspaceID: "w", Stream: "admin", Events: []Event{TestEvent("w", "s", now)}}
	if got, want := S3ObjectKey("", b, now), "zecurity/v1/w/admin/2026/11/06/09/export.test-d-9.jsonl.gz"; got != want {
		t.Errorf("test key\n got %s\nwant %s", got, want)
	}
}

// readNDJSONGz decodes a gzip NDJSON body the way Wazuh's aws-s3 reader does
// for .jsonl.gz: one JSON object per line.
func readNDJSONGz(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	var out []map[string]any
	sc := bufio.NewScanner(zr)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("line %q: %v", sc.Text(), err)
		}
		out = append(out, m)
	}
	return out
}

func TestS3_Deliver(t *testing.T) {
	type got struct {
		method, path, rawPath, ctype, auth, meta string
		body                                     []byte
	}
	var g got
	status := http.StatusOK
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		g = got{r.Method, r.URL.Path, r.URL.EscapedPath(), r.Header.Get("Content-Type"), r.Header.Get("Authorization"),
			r.Header.Get("X-Amz-Meta-Zecurity-Signature"), b}
		w.WriteHeader(status)
	}))
	defer srv.Close()

	cfg := goodS3
	cfg.Endpoint = srv.URL
	raw := s3cfg(t, cfg)
	u, err := (S3{}).DeriveURL(raw)
	if err != nil {
		t.Fatal(err)
	}
	d := Destination{SinkID: "s", URL: u, SigningKey: []byte("zsk_test"), AuthHeaderName: S3SecretSlot,
		AuthHeaderValue: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", Config: raw}
	if err := (S3{}).Deliver(context.Background(), srv.Client(), d, s3Batch(1, 2)); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if g.method != http.MethodPut || g.ctype != "application/gzip" {
		t.Errorf("method %s content-type %s", g.method, g.ctype)
	}
	wantPath := "/zecurity-audit/siem/prod/zecurity/v1/616015cb-8c8f-49fc-a6ae-b50fbfdb1bf6/admin/2026/11/06/07/1-2.jsonl.gz"
	if g.path != wantPath || g.rawPath != wantPath {
		t.Errorf("path %s raw %s", g.path, g.rawPath)
	}
	if !strings.HasPrefix(g.auth, "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/") ||
		!strings.Contains(g.auth, "/eu-west-1/s3/aws4_request") ||
		!strings.Contains(g.auth, "x-amz-meta-zecurity-signature") {
		t.Errorf("authorization %s", g.auth)
	}
	if strings.Contains(g.auth, "wJalr") {
		t.Fatal("secret leaked into the request")
	}
	if !strings.HasPrefix(g.meta, "t=") {
		t.Errorf("object signature %q", g.meta)
	}
	lines := readNDJSONGz(t, g.body)
	if len(lines) != 2 || lines[0]["seq"] != float64(1) || lines[1]["seq"] != float64(2) {
		t.Fatalf("lines %v", lines)
	}
	if lines[0]["details"].(map[string]any)["html"] != "<b>&" {
		t.Errorf("details %v", lines[0]["details"])
	}

	status = http.StatusForbidden
	err = (S3{}).Deliver(context.Background(), srv.Client(), d, s3Batch(3))
	if msg := ErrorMessage(err); !strings.Contains(msg, "HTTP 403") || strings.Contains(msg, srv.URL) {
		t.Errorf("403 message %q", msg)
	}

	d.AuthHeaderValue = ""
	if err := (S3{}).Deliver(context.Background(), srv.Client(), d, s3Batch(4)); err == nil {
		t.Error("delivered without a secret")
	}
}

func TestWindowDue(t *testing.T) {
	now := time.Date(2026, 11, 6, 8, 0, 0, 0, time.UTC)
	ev := func(at time.Time) Event { return Event{RecordedAt: ts(at)} }
	if due, wait := windowDue(ev(now.Add(-time.Minute)), S3BatchWindow, now); due || wait != 4*time.Minute {
		t.Errorf("young: due=%v wait=%v", due, wait)
	}
	if due, _ := windowDue(ev(now.Add(-5*time.Minute)), S3BatchWindow, now); !due {
		t.Error("5 min old not due")
	}
	if due, _ := windowDue(Event{RecordedAt: "garbage"}, S3BatchWindow, now); !due {
		t.Error("unreadable time not due")
	}
}
