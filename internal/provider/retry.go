package provider

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

// streamRetryAttempts is the maximum number of silent retries for
// transient connect failures before the streaming request is opened.
// 2 retries (so up to 3 total attempts) covers the common case where
// the upstream provider's edge proxy briefly returns 502/503/504 or
// resets the connection before headers, without making the user wait
// noticeably long if the outage is real.
const streamRetryAttempts = 2

// streamRetryBackoff returns the wait duration before retry attempt n
// (1-based). Short, fixed backoff: 250ms, then 750ms. Anything longer
// would feel like the agent froze; anything shorter starts hammering
// the provider when it's actually struggling.
func streamRetryBackoff(attempt int) time.Duration {
	switch attempt {
	case 1:
		return 250 * time.Millisecond
	default:
		return 750 * time.Millisecond
	}
}

// doStreamWithRetry performs an HTTP request that begins a streaming
// response, with up to streamRetryAttempts silent retries for transient
// connect failures. Successful responses (status 200) and non-transient
// failures (4xx, malformed bodies, etc.) are returned immediately.
//
// Retries fire on:
//   - net.Conn dial errors (connection refused, no such host, EOF
//     before headers, "upstream connect error", "connection reset")
//   - HTTP 502/503/504
//
// The handler is responsible for re-reading the request body each
// attempt; we re-create the request via newReq() because http.Request
// bodies are single-use. Callers in this package always Marshal a JSON
// body up-front, so newReq just rebuilds with bytes.NewReader on the
// captured payload.
//
// If every attempt fails, the last error/response is returned so the
// caller can format a normal error message. On context cancellation
// the loop bails out immediately with ctx.Err().
func doStreamWithRetry(ctx context.Context, client *http.Client, newReq func() (*http.Request, error)) (*http.Response, error) {
	var lastErr error
	for attempt := 0; attempt <= streamRetryAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(streamRetryBackoff(attempt)):
			}
		}
		req, err := newReq()
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			if !isTransientConnectError(err) || ctx.Err() != nil {
				return nil, err
			}
			continue
		}
		if isTransientHTTPStatus(resp.StatusCode) {
			// Drain a bit of the body so we can include it in the
			// final error if every retry exhausts. We cap the read
			// at 4 KiB because edge proxies sometimes send pages.
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			lastErr = &transientHTTPError{Status: resp.StatusCode, Body: strings.TrimSpace(string(body))}
			if attempt == streamRetryAttempts {
				// Wrap as a real *http.Response shape the caller
				// expects so it formats the error identically to a
				// non-retried failure.
				return synthesizeResponse(resp.StatusCode, body), nil
			}
			continue
		}
		return resp, nil
	}
	if lastErr == nil {
		lastErr = errors.New("retry loop exhausted")
	}
	return nil, lastErr
}

// transientHTTPError is the placeholder error returned while we're
// still inside the retry loop. Callers never observe it directly;
// the loop converts the final exhausted attempt into a synthesized
// http.Response so the caller's existing "http NNN: body" formatting
// keeps working unchanged.
type transientHTTPError struct {
	Status int
	Body   string
}

func (e *transientHTTPError) Error() string { return "transient http error" }

// synthesizeResponse rebuilds a closable response wrapping the captured
// body bytes so the caller's existing non-200 handling path keeps
// working without needing to know retries happened.
func synthesizeResponse(status int, body []byte) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
}

// isTransientConnectError reports whether err looks like a transient
// network failure that's worth retrying. The classic "upstream connect
// error or disconnect/reset before headers" from edge proxies (Envoy,
// Cloudflare, GFE) shows up as several different concrete error types
// across HTTP/1, HTTP/2, and TLS handshakes — easier to match by
// substring on the rendered message than to enumerate every type.
func isTransientConnectError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "unexpected eof") ||
		strings.Contains(msg, "eof") ||
		strings.Contains(msg, "no such host") ||
		strings.Contains(msg, "tls handshake") ||
		strings.Contains(msg, "i/o timeout") ||
		strings.Contains(msg, "transport failure") ||
		strings.Contains(msg, "upstream connect error") ||
		strings.Contains(msg, "disconnect/reset before headers")
}

// isTransientHTTPStatus reports whether a non-200 status code is
// likely transient (502/503/504). 5xx outside this range is surfaced
// immediately because it usually indicates a real backend bug.
func isTransientHTTPStatus(code int) bool {
	switch code {
	case http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	}
	return false
}
