package mcp

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"testing"
)

func TestDescribeFailureDoesNotTreatURLDigitsAsHTTPStatus(t *testing.T) {
	for _, port := range []string{"40159", "40359", "40459", "401", "403", "404"} {
		t.Run(port, func(t *testing.T) {
			for _, tc := range []struct {
				err  error
				want string
			}{
				{context.DeadlineExceeded, "timed out while connecting"},
				{errors.New("connection refused"), "connection refused"},
			} {
				err := &url.Error{Op: "Post", URL: "http://127.0.0.1:" + port + "/mcp", Err: tc.err}
				if got := describeFailure(fmt.Errorf("initialize: %w", err)); got != tc.want {
					t.Errorf("describeFailure(%q) = %q, want %q", err, got, tc.want)
				}
			}
		})
	}
}

func TestDescribeFailureHTTPStatus(t *testing.T) {
	for _, tc := range []struct {
		text string
		want string
	}{
		{"request failed with status 401: response body", "server rejected the credentials (401)"},
		{"unexpected status code: 403", "access forbidden (403)"},
		{"request failed with status 404: response body", "endpoint not found (404)"},
		{"HTTP 401 Unauthorized", "server rejected the credentials (401)"},
		{"HTTP/1.1 403 Forbidden", "access forbidden (403)"},
		{"authorization required", "server rejected the credentials (401)"},
		{"request failed with status 404: http://localhost:40159/mcp", "endpoint not found (404)"},
		{"request failed with status 4010", "connection or tool discovery failed"},
	} {
		t.Run(tc.text, func(t *testing.T) {
			if got := describeFailure(errors.New(tc.text)); got != tc.want {
				t.Errorf("describeFailure() = %q, want %q", got, tc.want)
			}
		})
	}
}
