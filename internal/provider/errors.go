// This file defines the typed errors the provider layer raises, and the single
// predicate callers use to ask "could an identical request succeed later?".
//
// The knowledge needed to answer that question only exists down here: the
// transport holds the HTTP status and the Retry-After hint, and the decoders
// hold the provider's own error type ("overloaded_error", "rate_limit_exceeded").
// By the time a failure has been rendered into an assistant message's
// errorMessage it is a free-form string, and any classifier above this layer
// would be reverse-engineering what was already known here. So the structure is
// preserved in the error value, which rides StreamErrorEvent.Err (runtime
// failures) or the returned error (early "cannot build the stream" failures)
// per the dual failure model in provider.go.
package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// transientError is implemented by provider errors that can classify
// themselves. Self-classification always wins over the heuristics in
// IsTransient.
type transientError interface {
	error
	// Transient reports whether an identical request could plausibly succeed on
	// a later attempt.
	Transient() bool
}

// UpstreamError is a non-2xx HTTP response from a provider endpoint. It keeps
// the status code and the server's Retry-After hint structured; Error()
// renders the form callers have always seen ("transport: upstream 429: ...").
type UpstreamError struct {
	// Status is the HTTP status code of the response.
	Status int
	// RetryAfter is the server's Retry-After hint, or 0 when it sent none.
	RetryAfter time.Duration
	// Body is the response body, truncated to errorBodyLimit. Empty for the
	// statuses the transport retries internally, which it drains and discards.
	Body string
}

func (e *UpstreamError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("transport: upstream %d", e.Status)
	}
	return fmt.Sprintf("transport: upstream %d: %s", e.Status, e.Body)
}

// Transient reports whether the status describes a condition the server may
// clear on its own: rate limiting (429, and Cloudflare's 529 which some
// upstreams pass through), a server-side request timeout (408), and every 5xx.
// Everything else — authentication, a malformed request, an unknown model — is
// the caller's to fix and retrying it only wastes the budget.
func (e *UpstreamError) Transient() bool {
	switch e.Status {
	case http.StatusRequestTimeout, http.StatusTooManyRequests:
		return true
	}
	return e.Status >= 500 && e.Status <= 599
}

// APIError is an error the provider reported inside its own stream (an SSE
// "error" event or error-bearing chunk). Family is the wire API the message
// came from ("openai" / "anthropic"), matching AssistantMessage.API; Type is
// the provider's error type verbatim.
type APIError struct {
	Family  string
	Type    string
	Message string
}

func (e *APIError) Error() string {
	msg := e.Family + " stream error"
	if e.Type != "" {
		msg = e.Family + " " + e.Type
	}
	if e.Message != "" {
		msg += ": " + e.Message
	}
	return msg
}

// transientAPIErrorTypes are the error-type tokens both vendors use for a
// temporary server-side condition: "rate_limit_error" / "rate_limit_exceeded",
// "overloaded_error", Anthropic's generic "api_error" (its 5xx), OpenAI's
// "server_error". Matching is on the type — a closed, machine-generated
// vocabulary — never on the free-form message, which routinely quotes
// durations, request ids and advice like "try again" that mean nothing about
// whether a retry can work.
var transientAPIErrorTypes = []string{
	"rate_limit", "overloaded", "server_error", "api_error",
	"service_unavailable", "timeout",
}

// Transient reports whether the provider's error type describes a temporary
// condition. An unknown type is treated as permanent: failing fast on an
// unrecognized error is recoverable for the user, whereas retrying a genuine
// rejection three times is just a slower failure.
func (e *APIError) Transient() bool {
	t := strings.ToLower(e.Type)
	if t == "" {
		return false
	}
	for _, marker := range transientAPIErrorTypes {
		if strings.Contains(t, marker) {
			return true
		}
	}
	return false
}

// networkFailureMarkers is the last-resort text check in IsTransient, for
// connection failures that reach us with no typed cause left to inspect (an
// HTTP/2 stream error, a driver that wrapped a read failure as a string). Every
// entry names a transport condition unambiguously; deliberately absent are
// status codes and phrases like "try again", which appear inside error bodies
// that have nothing to do with whether the connection can be re-established.
var networkFailureMarkers = []string{
	"connection reset", "connection refused", "broken pipe",
	"unexpected eof", "goaway", "stream error",
}

// IsTransient reports whether err describes a failure that an identical,
// later request could survive. It is the single predicate the retry policy
// above this layer consults.
func IsTransient(err error) bool {
	if err == nil {
		return false
	}
	// A typed provider error knows its own answer.
	var typed transientError
	if errors.As(err, &typed) {
		return typed.Transient()
	}
	// Cancellation is a decision, not a failure.
	if errors.Is(err, context.Canceled) {
		return false
	}
	// The bare deadline sentinel is the caller's own context expiring, and the
	// same context governs the retry — there is nothing left to run in. Compared
	// by identity on purpose: a client-side request timeout arrives wrapped in a
	// *url.Error and is classified below as the network timeout it is.
	if err == context.DeadlineExceeded { //nolint:errorlint // identity is the point
		return false
	}
	// The stream watchdogs: the connection was established but produced nothing
	// (idle) or stopped making progress (stall). A fresh request is exactly the
	// right response.
	if errors.Is(err, errStreamIdle) || errors.Is(err, errStreamStall) {
		return true
	}
	// A response body that ended mid-message.
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	// A name that does not resolve stays unresolved; any other DNS failure is
	// worth another look.
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return !dnsErr.IsNotFound
	}
	// Timeouts, resets, refused connections: net.Error covers the client-side
	// failures, net.OpError the ones raised while reading the body.
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	text := strings.ToLower(err.Error())
	for _, marker := range networkFailureMarkers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// RetryAfterHint returns the server's Retry-After delay for err, or 0 when it
// carries none. A caller that waits less than this is guaranteed to be refused
// again, so the retry policy treats it as a floor.
func RetryAfterHint(err error) time.Duration {
	var upstream *UpstreamError
	if errors.As(err, &upstream) {
		return upstream.RetryAfter
	}
	return 0
}
