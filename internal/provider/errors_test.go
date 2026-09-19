package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestUpstreamErrorClassification verifies classification comes from the status
// code, so an error body's wording cannot change the verdict. The bodies below
// are the shapes that defeat substring matching: a rate-limit response whose
// reset timestamp contains "400", and a 404 whose text invites a retry.
func TestUpstreamErrorClassification(t *testing.T) {
	cases := []struct {
		name string
		err  *UpstreamError
		want bool
	}{
		{"rate limited", &UpstreamError{Status: 429}, true},
		{"rate limited with a body full of digits", &UpstreamError{
			Status: 429,
			Body:   `{"error":{"message":"Rate limit exceeded","code":429,"metadata":{"X-RateLimit-Reset":"1758124000000"}}}`,
		}, true},
		{"cloudflare overload", &UpstreamError{Status: 529}, true},
		{"bad gateway", &UpstreamError{Status: 502}, true},
		{"request timeout", &UpstreamError{Status: 408}, true},
		{"unauthorized", &UpstreamError{Status: 401, Body: "invalid api key"}, false},
		{"not found, politely", &UpstreamError{
			Status: 404,
			Body:   `{"error":"no endpoints found for this model, please try again with another"}`,
		}, false},
		{"unprocessable", &UpstreamError{Status: 422}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsTransient(tc.err); got != tc.want {
				t.Errorf("IsTransient(%v) = %v, want %v", tc.err, got, tc.want)
			}
			// Wrapping must not lose the verdict: drivers tag errors with their name.
			wrapped := fmt.Errorf("openrouter: %w", tc.err)
			if got := IsTransient(wrapped); got != tc.want {
				t.Errorf("IsTransient(wrapped) = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestUpstreamErrorMessage pins the rendered form, which users see as the
// assistant turn's error message.
func TestUpstreamErrorMessage(t *testing.T) {
	if got := (&UpstreamError{Status: 429}).Error(); got != "transport: upstream 429" {
		t.Errorf("bodyless = %q", got)
	}
	if got := (&UpstreamError{Status: 400, Body: "bad model"}).Error(); got != "transport: upstream 400: bad model" {
		t.Errorf("with body = %q", got)
	}
}

// TestAPIErrorClassification verifies in-band provider errors classify on their
// error type, never on the free-form message.
func TestAPIErrorClassification(t *testing.T) {
	cases := []struct {
		typ  string
		want bool
	}{
		{"overloaded_error", true},
		{"rate_limit_error", true},
		{"rate_limit_exceeded", true},
		{"api_error", true},
		{"server_error", true},
		{"invalid_request_error", false},
		{"authentication_error", false},
		{"permission_error", false},
		{"insufficient_quota", false},
		{"", false},
	}
	for _, tc := range cases {
		err := &APIError{Family: "anthropic", Type: tc.typ, Message: "please try again in 404 seconds"}
		if got := IsTransient(err); got != tc.want {
			t.Errorf("IsTransient(type=%q) = %v, want %v", tc.typ, got, tc.want)
		}
	}
}

// TestAPIErrorMessage pins the rendered form the decoders used to build by
// hand, since it is what reaches the user.
func TestAPIErrorMessage(t *testing.T) {
	if got := (&APIError{Family: "openai", Type: "rate_limit_exceeded", Message: "slow down"}).Error(); got != "openai rate_limit_exceeded: slow down" {
		t.Errorf("full = %q", got)
	}
	if got := (&APIError{Family: "anthropic"}).Error(); got != "anthropic stream error" {
		t.Errorf("bare = %q", got)
	}
}

// TestIsTransientConnectionFailures verifies the connection-level cases that
// arrive without a typed provider error.
func TestIsTransientConnectionFailures(t *testing.T) {
	transient := []error{
		errStreamIdle,
		errStreamStall,
		io.ErrUnexpectedEOF,
		fmt.Errorf("read error: %w", io.ErrUnexpectedEOF),
		&net.OpError{Op: "read", Err: errors.New("connection reset by peer")},
		&net.DNSError{Err: "server misbehaving", IsTemporary: true},
		errors.New("http2: server sent GOAWAY and closed the connection"),
		errors.New("read tcp 10.0.0.1:443: connection reset by peer"),
	}
	for _, err := range transient {
		if !IsTransient(err) {
			t.Errorf("expected transient: %v", err)
		}
	}

	permanent := []error{
		nil,
		errors.New("boom"),
		context.Canceled,
		fmt.Errorf("transport: canceled: %w", context.Canceled),
		context.DeadlineExceeded,
		&net.DNSError{Err: "no such host", IsNotFound: true},
		errors.New("openrouter: missing API key (set OPENROUTER_API_KEY)"),
	}
	for _, err := range permanent {
		if IsTransient(err) {
			t.Errorf("expected permanent: %v", err)
		}
	}
}

// TestRetryAfterHint verifies the server's hint survives to the policy layer.
func TestRetryAfterHint(t *testing.T) {
	err := fmt.Errorf("openrouter: %w", &UpstreamError{Status: 429, RetryAfter: 30 * time.Second})
	if got := RetryAfterHint(err); got != 30*time.Second {
		t.Errorf("RetryAfterHint = %v, want 30s", got)
	}
	if got := RetryAfterHint(errors.New("boom")); got != 0 {
		t.Errorf("RetryAfterHint(untyped) = %v, want 0", got)
	}
}

// TestConnectSurfacesUpstreamError is the end-to-end check that the transport
// hands the status and the Retry-After hint to its caller rather than a string.
func TestConnectSurfacesUpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":"slow down"}`)
	}))
	defer srv.Close()

	_, err := StreamRequest(context.Background(), TransportConfig{
		MaxConnectRetries: -1, // no transport-level retries: surface the first refusal
		Decoder:           NewOpenAIDecoder(),
		NewRequest: func(ctx context.Context) (*http.Request, error) {
			return http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
		},
	})
	if err == nil {
		t.Fatal("a 429 must fail the connect")
	}
	var upstream *UpstreamError
	if !errors.As(err, &upstream) {
		t.Fatalf("error = %T (%v), want *UpstreamError", err, err)
	}
	if upstream.Status != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", upstream.Status)
	}
	if upstream.RetryAfter != 7*time.Second {
		t.Errorf("RetryAfter = %v, want 7s", upstream.RetryAfter)
	}
	if !IsTransient(err) {
		t.Error("a 429 from the endpoint must classify as transient")
	}
}

// TestDecoderSurfacesAPIError is the same check for an error the provider
// reports inside the stream.
func TestDecoderSurfacesAPIError(t *testing.T) {
	_, err := NewAnthropicDecoder().Decode([]byte(`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`))
	if err == nil {
		t.Fatal("an error event must return an error")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %T (%v), want *APIError", err, err)
	}
	if apiErr.Type != "overloaded_error" || !IsTransient(err) {
		t.Errorf("type = %q, transient = %v", apiErr.Type, IsTransient(err))
	}
	if !strings.Contains(err.Error(), "overloaded_error") {
		t.Errorf("rendered message must name the type: %q", err.Error())
	}
}
