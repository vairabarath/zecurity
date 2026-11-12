package auditexport

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// AWS's published examples: the SigV4 test suite's get-vanilla, and the S3
// developer guide's "PUT Object" example (header-based auth).
func TestSigV4_AWSPublishedExamples(t *testing.T) {
	t.Run("get-vanilla", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, "https://example.amazonaws.com/", nil)
		ts := time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)
		signV4(req, sigV4Credentials{"AKIDEXAMPLE", "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"},
			"us-east-1", "service", sha256Hex(nil), ts)
		want := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, " +
			"SignedHeaders=host;x-amz-date, Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31"
		if got := req.Header.Get("Authorization"); got != want {
			t.Fatalf("Authorization\n got %s\nwant %s", got, want)
		}
	})
	t.Run("s3-put-object", func(t *testing.T) {
		body := "Welcome to Amazon S3."
		req, _ := http.NewRequest(http.MethodPut, "https://examplebucket.s3.amazonaws.com/test$file.text", strings.NewReader(body))
		req.Header.Set("Date", "Fri, 24 May 2013 00:00:00 GMT")
		req.Header.Set("X-Amz-Storage-Class", "REDUCED_REDUNDANCY")
		ts := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)
		signV4(req, sigV4Credentials{"AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"},
			"us-east-1", "s3", sha256Hex([]byte(body)), ts)
		if got := req.Header.Get("X-Amz-Content-Sha256"); got != "44ce7dd67c959e0d3524ffac1771dfbba87d2b6b4b4e99e42034a8b803f8b072" {
			t.Fatalf("payload hash %s", got)
		}
		want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, " +
			"SignedHeaders=date;host;x-amz-content-sha256;x-amz-date;x-amz-storage-class, " +
			"Signature=98ad721746da40c64f1a55b78f14c238d841ea1380cd77a1b5971af0ece108bd"
		if got := req.Header.Get("Authorization"); got != want {
			t.Fatalf("Authorization\n got %s\nwant %s", got, want)
		}
	})
}

// Reference signatures produced with botocore's S3SigV4Auth (2026-11-06) for
// the request shape the S3 sink sends, including awkward object keys.
func TestSigV4_MatchesBotocore(t *testing.T) {
	creds := sigV4Credentials{"AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"}
	ts := time.Date(2026, 11, 6, 8, 33, 0, 0, time.UTC)
	cases := []struct {
		name, url, region, body, sig string
	}{
		{"virtual-hosted", "https://examplebucket.s3.eu-west-1.amazonaws.com/logs/zecurity/v1/ws/admin/2026/11/06/07/1-75.jsonl.gz",
			"eu-west-1", "hello\n", "c5ccafcca82c5ee04791277e2575d10505c0f1a554e02a738a95391736688ab8"},
		{"path-style odd key", "https://minio.example.com:9000/my-bucket/pre%20fix/ab/c~d_e.f/%24weird%C3%A9.jsonl.gz",
			"us-east-1", "", "ef4363e8e3c31dcd98126e345aab28f5f0cdf7a77a44d8f3503533a1fc3cf003"},
		{"dollar key", "https://examplebucket.s3.amazonaws.com/test%24file.text",
			"us-east-1", "Welcome to Amazon S3.", "add91dd781a5330817493cb3a2c8a6c53c68b2d40da69e0f3167d62b6b0f76a8"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPut, c.url, strings.NewReader(c.body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/gzip")
			signV4(req, creds, c.region, "s3", sha256Hex([]byte(c.body)), ts)
			got := req.Header.Get("Authorization")
			if !strings.HasSuffix(got, "Signature="+c.sig) {
				t.Fatalf("Authorization\n got %s\nwant signature %s", got, c.sig)
			}
			if !strings.Contains(got, "SignedHeaders=content-type;host;x-amz-content-sha256;x-amz-date,") {
				t.Fatalf("signed headers: %s", got)
			}
		})
	}
}

func TestURIEncode(t *testing.T) {
	for in, want := range map[string]string{
		"/a b/c+d/é$~_.-": "/a%20b/c%2Bd/%C3%A9%24~_.-",
		"a/b":             "a/b",
	} {
		if got := uriEncode(in, false); got != want {
			t.Errorf("uriEncode(%q) = %q, want %q", in, got, want)
		}
	}
	if got := uriEncode("a/b", true); got != "a%2Fb" {
		t.Errorf("encodeSlash: %q", got)
	}
}
