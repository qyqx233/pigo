// This file owns the agent-level retry *policy* for transient provider
// failures (the pigo port of pi's `retry` settings): how many attempts a run
// may spend and how long to wait between them.
//
// It deliberately owns nothing else. Classification ("is this failure worth
// another attempt?") belongs to the provider layer, which still holds the HTTP
// status, the Retry-After hint and the provider's own error type — see
// provider/errors.go. The decision point is the request boundary in
// stream_response.go, which is the only place that can unwind a failed attempt
// before replacing it.
package runtime

import (
	"context"
	"time"

	"github.com/smallnest/pigo/internal/provider"
)

// Defaults for RetrySettings, mirroring pi: three attempts with a 2s → 4s → 8s
// backoff, never waiting longer than a minute.
const (
	defaultMaxRetries = 3
	defaultBaseDelay  = 2 * time.Second
	defaultMaxDelay   = 60 * time.Second
)

// RetrySettings tunes agent-level retry of transient provider failures.
//
// The zero value is "enabled, with the defaults below", so a caller that never
// thinks about retry still gets it, and every numeric field that is left unset
// falls back to its default independently. Turning retry off is therefore
// always an explicit act (Disabled: true) that cannot be expressed by accident.
type RetrySettings struct {
	// Disabled turns agent-level retry off entirely.
	Disabled bool
	// MaxRetries is how many extra attempts a single run may spend in total,
	// across all of its turns. <= 0 uses defaultMaxRetries.
	MaxRetries int
	// BaseDelay is the wait before the first retry; each further retry doubles
	// it. <= 0 uses defaultBaseDelay.
	BaseDelay time.Duration
	// MaxDelay caps the backoff, and with it the longest stall a run will
	// tolerate before giving up on a server-supplied Retry-After hint.
	// <= 0 uses defaultMaxDelay.
	MaxDelay time.Duration
}

// DefaultRetrySettings returns the settings a zero RetrySettings resolves to.
func DefaultRetrySettings() RetrySettings {
	return RetrySettings{
		MaxRetries: defaultMaxRetries,
		BaseDelay:  defaultBaseDelay,
		MaxDelay:   defaultMaxDelay,
	}
}

// withDefaults fills every unset numeric field from the defaults, leaving
// Disabled untouched.
func (s RetrySettings) withDefaults() RetrySettings {
	d := DefaultRetrySettings()
	if s.MaxRetries <= 0 {
		s.MaxRetries = d.MaxRetries
	}
	if s.BaseDelay <= 0 {
		s.BaseDelay = d.BaseDelay
	}
	if s.MaxDelay <= 0 {
		s.MaxDelay = d.MaxDelay
	}
	return s
}

// retryBudget is a run's allowance of transient-failure retries. It is created
// once per run and shared by every turn, so a long multi-turn task cannot
// accumulate retries indefinitely — this is the deliberate difference from pi,
// which counts per request.
//
// It is consulted only from the run's own goroutine (the request boundary in
// streamAssistantResponse), so it needs no locking.
type retryBudget struct {
	settings RetrySettings
	used     int
}

// newRetryBudget resolves settings and returns the run's budget.
func newRetryBudget(settings RetrySettings) *retryBudget {
	return &retryBudget{settings: settings.withDefaults()}
}

// maxRetries is the budget's total allowance, for reporting in RetryEvent.
func (b *retryBudget) maxRetries() int {
	if b == nil {
		return 0
	}
	return b.settings.MaxRetries
}

// next decides whether the failure described by err earns another attempt.
// When it does it consumes one unit of the budget and returns the wait before
// that attempt plus its 1-based number; otherwise it returns ok == false and
// the caller surfaces the failure.
func (b *retryBudget) next(err error) (delay time.Duration, attempt int, ok bool) {
	if b == nil || b.settings.Disabled || b.used >= b.settings.MaxRetries {
		return 0, 0, false
	}
	if !provider.IsTransient(err) {
		return 0, 0, false
	}
	delay = b.backoff(b.used + 1)
	// The server's own Retry-After is a floor: coming back sooner than it said
	// only spends the budget on a guaranteed refusal. A hint longer than
	// MaxDelay is a wait this run is not willing to sit through, so stop now
	// rather than sleep the cap and fail anyway.
	if hint := provider.RetryAfterHint(err); hint > 0 {
		if hint > b.settings.MaxDelay {
			return 0, 0, false
		}
		if hint > delay {
			delay = hint
		}
	}
	b.used++
	return delay, b.used, true
}

// backoff returns the wait before the given 1-based retry attempt:
// BaseDelay << (attempt-1), capped at MaxDelay.
func (b *retryBudget) backoff(attempt int) time.Duration {
	d := b.settings.BaseDelay
	for i := 1; i < attempt && d < b.settings.MaxDelay; i++ {
		d *= 2
	}
	if d > b.settings.MaxDelay {
		d = b.settings.MaxDelay
	}
	return d
}

// sleepContext waits for d, reporting false if ctx was cancelled first.
func sleepContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
