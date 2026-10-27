package claude

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestFetchOAuthUsageFixedReadOnlyRequestAndNoRedirect(t *testing.T) {
	for _, redirect := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "redirect"}[redirect], func(t *testing.T) {
			calls := 0
			auth := &ClaudeAuth{httpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				if calls > 1 {
					t.Fatal("usage followed redirect")
				}
				if req.Method != http.MethodGet || req.URL.String() != UsageURL || req.Body != nil {
					t.Fatalf("unexpected inference/destination: %s %s", req.Method, req.URL)
				}
				if req.Header.Get("Authorization") != "Bearer private-token" || req.Header.Get("Anthropic-Beta") != "oauth-2025-04-20" || req.Header.Get("Cache-Control") != "no-cache" {
					t.Fatal("missing OAuth usage headers")
				}
				if _, deadline := req.Context().Deadline(); deadline {
					t.Fatal("post-connect timeout introduced")
				}
				if redirect {
					return &http.Response{StatusCode: 302, Header: http.Header{"Location": {UsageURL + "/other"}}, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
				}
				return jsonResponse(req, `{"five_hour":null,"seven_day":null}`), nil
			})}}
			_, err := auth.FetchOAuthUsage(context.Background(), "private-token")
			if (err != nil) != redirect {
				t.Fatalf("redirect=%v error=%v", redirect, err)
			}
		})
	}
}

func TestOAuthResponseBoundsEncodedAndDecodedUsage(t *testing.T) {
	large := strings.Repeat("x", (1<<20)+1)
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	_, _ = writer.Write([]byte(large))
	_ = writer.Close()
	for _, tc := range []struct {
		name, encoding string
		body           []byte
	}{{"encoded", "", []byte(large)}, {"decoded", "gzip", compressed.Bytes()}} {
		t.Run(tc.name, func(t *testing.T) {
			resp := &http.Response{Header: http.Header{"Content-Encoding": {tc.encoding}}, Body: io.NopCloser(bytes.NewReader(tc.body))}
			if _, err := readClaudeOAuthResponseBody(resp); err == nil {
				t.Fatal("accepted oversized control-plane body")
			}
		})
	}
}
