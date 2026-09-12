package circuitbreaker

import (
	"fmt"
	"math"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sony/gobreaker/v2"

	"github.com/ilter-ai/ilter/internal/config"
)

// toUint32Clamped converts n to uint32, clamping negative values to 0 and
// values above math.MaxUint32 to math.MaxUint32, so callers can safely
// narrow config-provided ints without risking a silent overflow.
func toUint32Clamped(n int) uint32 {
	if n < 0 {
		return 0
	}
	if n > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(n) //nolint:gosec // bounds-checked above
}

// State returns the current circuit breaker state ("closed", "open", or
// "half-open") for rt, or "unknown" if rt is not an *HTTPBreaker.
func State(rt http.RoundTripper) string {
	t, ok := rt.(*HTTPBreaker)
	if !ok {
		return "unknown"
	}
	if t.forceOpen.Load() {
		return "open"
	}
	switch t.cb.State() {
	case gobreaker.StateClosed:
		return "closed"
	case gobreaker.StateOpen:
		return "open"
	case gobreaker.StateHalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}

// Counts returns the circuit breaker's request/failure counters for rt, or
// nil if rt is not an *HTTPBreaker.
func Counts(rt http.RoundTripper) *gobreaker.Counts {
	t, ok := rt.(*HTTPBreaker)
	if !ok {
		return nil
	}
	c := t.cb.Counts()
	return &c
}

// Metrics returns aggregate request/error counts and the last error/success
// times for rt's circuit breaker, or zero values if rt is not an *HTTPBreaker.
func Metrics(rt http.RoundTripper) (totalRequests, totalErrors int64, lastErrorTime, lastSuccessTime *time.Time) {
	t, ok := rt.(*HTTPBreaker)
	if !ok {
		return 0, 0, nil, nil
	}
	return t.Metrics()
}

// HTTPBreaker is an http.RoundTripper that wraps another transport with a
// circuit breaker, short-circuiting requests to a failing provider once it
// trips open instead of letting them pile up against it.
type HTTPBreaker struct {
	cb        *gobreaker.CircuitBreaker[*http.Response]
	transport http.RoundTripper
	name      string
	cfg       config.CircuitBreakerConfig

	enabled   atomic.Bool
	forceOpen atomic.Bool

	lastErrorTime   time.Time
	lastSuccessTime time.Time
	totalRequests   int64
	totalErrors     int64
	mu              sync.Mutex
}

// NewHTTPBreaker wraps transport in a circuit breaker configured from cfg,
// using name to identify it in the breaker's internal settings/metrics.
func NewHTTPBreaker(transport http.RoundTripper, name string, cfg config.CircuitBreakerConfig) *HTTPBreaker {
	// An unset (zero) MaxFailures makes ReadyToTrip's ConsecutiveFailures>=0
	// always true, tripping the breaker fully open after a single failed
	// request — taking every model on that provider down with it. No config
	// path currently sets this per-provider, so default it here.
	maxFailures := cfg.MaxFailures
	if maxFailures <= 0 {
		maxFailures = 5
	}

	st := gobreaker.Settings{
		Name:        name,
		MaxRequests: toUint32Clamped(cfg.HalfOpenMaxRequests),
		Interval:    0, // zero means clear counts on open/close
		Timeout:     cfg.Timeout,
		ReadyToTrip: func(counts gobreaker.Counts) bool {
			return counts.ConsecutiveFailures >= toUint32Clamped(maxFailures)
		},
	}

	//nolint:bodyclose // body is closed on error paths above; on success it's left open for RoundTrip's caller, per http.RoundTripper's contract — bodyclose can't trace that through the generic Execute closure.
	cb := gobreaker.NewCircuitBreaker[*http.Response](st)

	t := &HTTPBreaker{
		cb:        cb,
		transport: transport,
		name:      name,
		cfg:       cfg,
	}
	t.enabled.Store(true)
	return t
}

// RoundTrip executes req through the wrapped transport, recording failures
// (transport errors and 5xx responses) against the circuit breaker and
// short-circuiting with gobreaker.ErrOpenState while the breaker is open or
// forced open.
func (t *HTTPBreaker) RoundTrip(req *http.Request) (*http.Response, error) {
	if !t.enabled.Load() {
		return t.transport.RoundTrip(req)
	}
	if t.forceOpen.Load() {
		return nil, gobreaker.ErrOpenState
	}

	t.mu.Lock()
	t.totalRequests++
	t.mu.Unlock()

	resp, err := t.cb.Execute(func() (*http.Response, error) {
		r, e := t.transport.RoundTrip(req)
		if e != nil {
			t.mu.Lock()
			t.totalErrors++
			t.lastErrorTime = time.Now()
			t.mu.Unlock()
			return nil, e
		}
		if r.StatusCode >= 500 {
			_ = r.Body.Close()
			t.mu.Lock()
			t.totalErrors++
			t.lastErrorTime = time.Now()
			t.mu.Unlock()
			return nil, fmt.Errorf("provider returned status %d", r.StatusCode)
		}
		t.mu.Lock()
		t.lastSuccessTime = time.Now()
		t.mu.Unlock()
		return r, nil
	})

	return resp, err
}

// SetEnabled toggles whether the breaker is active; when disabled, RoundTrip
// passes requests straight through to the wrapped transport.
func (t *HTTPBreaker) SetEnabled(v bool) { t.enabled.Store(v) }

// SetForceOpen forces the breaker into (or out of) the open state regardless
// of its failure counts, for manual/admin overrides.
func (t *HTTPBreaker) SetForceOpen(v bool) { t.forceOpen.Store(v) }

// Enabled reports whether the breaker is currently active.
func (t *HTTPBreaker) Enabled() bool { return t.enabled.Load() }

// Reset rebuilds the underlying circuit breaker from scratch, clearing all
// counters and forcing it back to the closed, enabled state.
func (t *HTTPBreaker) Reset() {
	st := gobreaker.Settings{
		Name:        t.name,
		MaxRequests: toUint32Clamped(t.cfg.HalfOpenMaxRequests),
		Interval:    0,
		Timeout:     t.cfg.Timeout,
		ReadyToTrip: func(counts gobreaker.Counts) bool {
			return counts.ConsecutiveFailures >= toUint32Clamped(t.cfg.MaxFailures)
		},
	}
	t.mu.Lock()
	//nolint:bodyclose // body is closed on error paths in RoundTrip; on success it's left open for RoundTrip's caller, per http.RoundTripper's contract — bodyclose can't trace that through the generic Execute closure.
	t.cb = gobreaker.NewCircuitBreaker[*http.Response](st)
	t.totalRequests = 0
	t.totalErrors = 0
	t.lastErrorTime = time.Time{}
	t.lastSuccessTime = time.Time{}
	t.mu.Unlock()
	t.forceOpen.Store(false)
	t.enabled.Store(true)
}

// Metrics returns this breaker's request/error counters and last error/success times.
func (t *HTTPBreaker) Metrics() (totalRequests, totalErrors int64, lastErrorTime, lastSuccessTime *time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()

	var lastErr *time.Time
	if !t.lastErrorTime.IsZero() {
		lastErr = &t.lastErrorTime
	}
	var lastSuc *time.Time
	if !t.lastSuccessTime.IsZero() {
		lastSuc = &t.lastSuccessTime
	}

	return t.totalRequests, t.totalErrors, lastErr, lastSuc
}
