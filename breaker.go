// Copyright 2026 Sylvain Müller. All rights reserved.
// Mount of this source code is governed by a MIT license that can be found
// at https://github.com/fox-toolkit/breaker/blob/main/LICENSE.txt.

package breaker

import (
	"context"
	"math/rand/v2"
	"sync/atomic"
	"time"

	"github.com/fox-toolkit/fox"
)

const (
	numBuckets   = 10
	evalInterval = int64(100 * time.Millisecond)
)

type middleware struct {
	failure     func(c *fox.Context) bool
	reject      fox.HandlerFunc
	k           float64
	maxReject   float64
	minRequests uint64
	width       int64
}

// Middleware returns a [fox.MiddlewareFunc] that probabilistically rejects requests when the failure ratio goes above
// the configured ratio, based on the adaptive throttling described in the Google SRE book.
// Each route gets its own instance of a given breaker. One instance can reject requests while another does not,
// their state is not shared. To share state across routes, use [New].
func Middleware(opts ...Option) (fox.MiddlewareFunc, error) {
	m, err := newMiddleware(opts...)
	if err != nil {
		return nil, err
	}
	return func(next fox.HandlerFunc) fox.HandlerFunc {
		return m.newBreaker().wrap(next)
	}, nil
}

// New returns a [Breaker] that probabilistically rejects requests when the failure ratio goes above the configured
// ratio. Its state is shared by every handler wrapped by [Breaker.Middleware].
func New(opts ...Option) (*Breaker, error) {
	m, err := newMiddleware(opts...)
	if err != nil {
		return nil, err
	}
	return m.newBreaker(), nil
}

func newMiddleware(opts ...Option) (*middleware, error) {
	cfg := defaultConfig()
	for _, opt := range opts {
		if err := opt.apply(cfg); err != nil {
			return nil, err
		}
	}
	return &middleware{
		failure:     cfg.failure,
		reject:      cfg.reject,
		k:           1 / (1 - cfg.failureRatio),
		maxReject:   cfg.maxRejectRatio,
		minRequests: cfg.minRequests,
		width:       int64(cfg.window / numBuckets),
	}, nil
}

func (m *middleware) newBreaker() *Breaker {
	return &Breaker{
		m:     m,
		start: time.Now(),
	}
}

// threshold returns the reject probability scaled to the uint64 range, following
// p = (requests - k * accepts) / (requests + 1).
func (m *middleware) threshold(requests, accepts uint64) uint64 {
	if requests < m.minRequests {
		return 0
	}
	r := float64(requests)
	p := (r - m.k*float64(accepts)) / (r + 1)
	if p <= 0 {
		return 0
	}
	// p < 1, so the product stays below 1<<64.
	return uint64(min(p, m.maxReject) * (1 << 64))
}

// Breaker is the state shared by every handler wrapped by its middleware.
type Breaker struct {
	start     time.Time
	m         *middleware
	threshold atomic.Uint64
	lastEval  atomic.Int64
	buckets   [numBuckets]bucket
	lastEpoch atomic.Int64
}

type bucket struct {
	requests counter
	accepts  counter
}

// Middleware returns a [fox.MiddlewareFunc] that uses the state of b on every handler it wraps.
func (b *Breaker) Middleware() fox.MiddlewareFunc {
	return b.wrap
}

// RejectRatio returns the probability that the next request is rejected.
func (b *Breaker) RejectRatio() float64 {
	return float64(b.evaluate(b.elapsed())) / (1 << 64)
}

func (b *Breaker) wrap(next fox.HandlerFunc) fox.HandlerFunc {
	return func(c *fox.Context) {
		b.serve(c, next)
	}
}

func (b *Breaker) serve(c *fox.Context, next fox.HandlerFunc) {
	if t := b.threshold.Load(); t != 0 {
		if r := rand.Uint64(); r < t { // nolint:gosec // used to draw rejected requests
			now := b.elapsed()
			// The threshold can predate an idle period.
			if now-b.lastEval.Load() < evalInterval || r < b.evaluate(now) {
				b.record(now, false)
				b.m.reject(c)
				return
			}
		}
	}

	ctx := c.Request().Context()
	ok := false
	defer func() {
		if !ok && ctx.Err() != context.Canceled {
			b.record(b.elapsed(), false)
		}
	}()
	next(c)
	if ctx.Err() == context.Canceled {
		ok = true
		return
	}
	accepted := !b.m.failure(c)
	ok = true
	b.record(b.elapsed(), accepted)
}

func (b *Breaker) elapsed() int64 {
	return int64(time.Since(b.start))
}

func (b *Breaker) record(now int64, accepted bool) {
	e := now / b.m.width
	epoch := uint32(e) // nolint:gosec // the epoch comparisons handle the wrap
	if last := b.lastEpoch.Load(); e > last {
		// After a very long idle period, the stored epochs may have wrapped. Reset the buckets.
		if e-last >= 1<<30 {
			b.reset(epoch)
		}
		b.lastEpoch.CompareAndSwap(last, e)
	}
	bk := &b.buckets[e%numBuckets]
	// Accept before request, so a concurrent sum never counts a request without its accept.
	if accepted {
		bk.accepts.inc(epoch)
	}
	bk.requests.inc(epoch)
	b.evaluate(now)
}

func (b *Breaker) evaluate(now int64) uint64 {
	last := b.lastEval.Load()
	if now-last < evalInterval || !b.lastEval.CompareAndSwap(last, now) {
		return b.threshold.Load()
	}
	epoch := uint32(now / b.m.width) // nolint:gosec // the epoch comparisons handle the wrap
	var requests, accepts uint64
	for i := range b.buckets {
		r, rNewer := b.buckets[i].requests.load(epoch)
		a, aNewer := b.buckets[i].accepts.load(epoch)
		// Restarted with a newer epoch.
		if rNewer || aNewer {
			continue
		}
		requests += r
		accepts += a
	}
	t := b.m.threshold(requests, accepts)
	b.threshold.Store(t)
	return t
}

func (b *Breaker) reset(epoch uint32) {
	for i := range b.buckets {
		b.buckets[i].requests.reset(epoch)
		b.buckets[i].accepts.reset(epoch)
	}
}

// counter holds the bucket epoch in the high 32 bits and the count in the low 32 bits.
type counter struct {
	v atomic.Uint64
}

func (c *counter) inc(epoch uint32) {
	v := c.v.Add(1)
	// A retry never writes a stale word.
	for int32(uint32(v>>32)-epoch) < 0 { // nolint:gosec // wrapping difference
		if c.v.CompareAndSwap(v, uint64(epoch)<<32|1) {
			return
		}
		if v = c.v.Load(); int32(uint32(v>>32)-epoch) >= 0 { // nolint:gosec // wrapping difference
			c.v.Add(1)
			return
		}
	}
}

// load returns the count within the window ending at epoch, and whether the bucket is newer.
func (c *counter) load(epoch uint32) (uint64, bool) {
	v := c.v.Load()
	d := int32(uint32(v>>32) - epoch) // nolint:gosec // wrapping difference
	if d > 0 {
		return 0, true
	}
	if d <= -numBuckets {
		return 0, false
	}
	return v & (1<<32 - 1), false
}

func (c *counter) reset(epoch uint32) {
	for {
		v := c.v.Load()
		if uint32(v>>32) == epoch || c.v.CompareAndSwap(v, uint64(epoch)<<32) {
			return
		}
	}
}
