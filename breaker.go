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

// Middleware returns a [fox.MiddlewareFunc] that rejects a share of requests when the failure ratio goes above the
// configured ratio, based on the adaptive throttling described in the Google SRE book. Each route has its own state.
// A panic counts as a failure, and a request canceled by the client is not counted.
func Middleware(opts ...Option) (fox.MiddlewareFunc, error) {
	m, err := newMiddleware(opts...)
	if err != nil {
		return nil, err
	}
	return m.wrap, nil
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

func (m *middleware) wrap(next fox.HandlerFunc) fox.HandlerFunc {
	return m.newBreaker(next).serve
}

func (m *middleware) newBreaker(next fox.HandlerFunc) *breaker {
	return &breaker{
		m:     m,
		next:  next,
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

type breaker struct {
	start     time.Time
	m         *middleware
	next      fox.HandlerFunc
	threshold atomic.Uint64
	lastEval  atomic.Int64
	// Keep the fields read on each request off the buckets written on each request, with 64-byte cache lines.
	_       [8]byte
	buckets [numBuckets]bucket
}

type bucket struct {
	requests counter
	accepts  counter
}

func (b *breaker) serve(c *fox.Context) {
	if t := b.threshold.Load(); t != 0 && rand.Uint64() < t { // nolint:gosec // load shedding, not security
		now := b.elapsed()
		b.record(now, false)
		b.m.reject(c)
		return
	}

	ctx := c.Request().Context()
	ok := false
	defer func() {
		if !ok && ctx.Err() != context.Canceled {
			b.record(b.elapsed(), false)
		}
	}()
	b.next(c)
	ok = true
	if ctx.Err() != context.Canceled {
		b.record(b.elapsed(), !b.m.failure(c))
	}
}

func (b *breaker) elapsed() int64 {
	return int64(time.Since(b.start))
}

// record counts a request in the bucket of now and updates the reject threshold at most every evalInterval.
func (b *breaker) record(now int64, accepted bool) {
	epoch := uint32(now / b.m.width) // nolint:gosec // wraps after 13 years at the smallest width
	bk := &b.buckets[epoch%numBuckets]
	bk.requests.inc(epoch)
	if accepted {
		bk.accepts.inc(epoch)
	}

	last := b.lastEval.Load()
	if now-last < evalInterval || !b.lastEval.CompareAndSwap(last, now) {
		return
	}
	var requests, accepts uint64
	for i := range b.buckets {
		requests += b.buckets[i].requests.load(epoch)
		accepts += b.buckets[i].accepts.load(epoch)
	}
	b.threshold.Store(b.m.threshold(requests, accepts))
}

// counter holds the bucket epoch in the high 32 bits and the count in the low 32 bits.
type counter struct {
	v atomic.Uint64
}

func (c *counter) inc(epoch uint32) {
	for {
		v := c.v.Add(1)
		// Counted in the current or a newer bucket.
		if int32(uint32(v>>32)-epoch) >= 0 { // nolint:gosec // wrapping difference
			return
		}
		// Stale bucket: restart it with this request, or retry if another request changed it.
		if c.v.CompareAndSwap(v, uint64(epoch)<<32|1) {
			return
		}
	}
}

// load returns the count if the bucket is within the window ending at epoch.
func (c *counter) load(epoch uint32) uint64 {
	v := c.v.Load()
	if epoch-uint32(v>>32) >= numBuckets {
		return 0
	}
	return v & (1<<32 - 1)
}
