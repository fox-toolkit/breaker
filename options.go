// Copyright 2026 Sylvain Müller. All rights reserved.
// Mount of this source code is governed by a MIT license that can be found
// at https://github.com/fox-toolkit/breaker/blob/main/LICENSE.txt.

package breaker

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/fox-toolkit/fox"
)

var ErrInvalidConfig = errors.New("invalid config")

const (
	// DefaultFailureRatio is the default failure ratio above which requests are rejected.
	DefaultFailureRatio = 0.5
	// DefaultMinRequests is the default number of requests in the window below which no request is rejected.
	DefaultMinRequests = 20
	// DefaultWindow is the default duration of the measurement window.
	DefaultWindow = 10 * time.Second
	// DefaultMaxRejectRatio is the default maximum ratio of rejected requests.
	DefaultMaxRejectRatio = 0.9
)

const (
	minWindow = time.Second
	maxWindow = 10 * time.Minute
)

// Option configures the middleware.
type Option interface {
	apply(*config) error
}

type optionFunc func(*config) error

func (f optionFunc) apply(c *config) error {
	return f(c)
}

type config struct {
	failure        func(c *fox.Context) bool
	reject         fox.HandlerFunc
	window         time.Duration
	failureRatio   float64
	maxRejectRatio float64
	minRequests    uint64
}

func defaultConfig() *config {
	return &config{
		failure:        DefaultFailure,
		reject:         DefaultReject,
		window:         DefaultWindow,
		failureRatio:   DefaultFailureRatio,
		maxRejectRatio: DefaultMaxRejectRatio,
		minRequests:    DefaultMinRequests,
	}
}

// WithFailureRatio sets the failure ratio, in [0, 1), above which requests start to be rejected. The default is
// [DefaultFailureRatio].
func WithFailureRatio(ratio float64) Option {
	return optionFunc(func(c *config) error {
		if !(ratio >= 0 && ratio < 1) {
			return fmt.Errorf("%w: failure ratio must be in [0, 1)", ErrInvalidConfig)
		}
		c.failureRatio = ratio
		return nil
	})
}

// WithMaxRejectRatio sets the maximum ratio, in (0, 1], of rejected requests. The remaining requests always reach the
// handler, which lets the middleware detect a recovery. The default is [DefaultMaxRejectRatio].
func WithMaxRejectRatio(ratio float64) Option {
	return optionFunc(func(c *config) error {
		if !(ratio > 0 && ratio <= 1) {
			return fmt.Errorf("%w: max reject ratio must be in (0, 1]", ErrInvalidConfig)
		}
		c.maxRejectRatio = ratio
		return nil
	})
}

// WithMinRequests sets the number of requests in the window below which no request is rejected. The default is
// [DefaultMinRequests].
func WithMinRequests(n int) Option {
	return optionFunc(func(c *config) error {
		if n < 0 {
			return fmt.Errorf("%w: negative min requests", ErrInvalidConfig)
		}
		c.minRequests = uint64(n)
		return nil
	})
}

// WithWindow sets the duration of the measurement window, between 1s and 10m. The default is [DefaultWindow].
func WithWindow(d time.Duration) Option {
	return optionFunc(func(c *config) error {
		if d < minWindow || d > maxWindow {
			return fmt.Errorf("%w: window must be between %s and %s", ErrInvalidConfig, minWindow, maxWindow)
		}
		c.window = d
		return nil
	})
}

// WithFailure sets the function that reports whether a handled request failed. The default is [DefaultFailure].
func WithFailure(fn func(c *fox.Context) bool) Option {
	return optionFunc(func(c *config) error {
		if fn == nil {
			return fmt.Errorf("%w: nil failure function", ErrInvalidConfig)
		}
		c.failure = fn
		return nil
	})
}

// WithRejectHandler sets the handler called for rejected requests. The default is [DefaultReject].
func WithRejectHandler(handler fox.HandlerFunc) Option {
	return optionFunc(func(c *config) error {
		if handler == nil {
			return fmt.Errorf("%w: nil reject handler", ErrInvalidConfig)
		}
		c.reject = handler
		return nil
	})
}

// DefaultFailure reports a response status of 502, 503 or 504 as a failure.
func DefaultFailure(c *fox.Context) bool {
	switch c.Writer().Status() {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// DefaultReject responds with 503 Service Unavailable.
func DefaultReject(c *fox.Context) {
	http.Error(c.Writer(), http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
}
