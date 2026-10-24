// Copyright 2026 Sylvain Müller. All rights reserved.
// Mount of this source code is governed by a MIT license that can be found
// at https://github.com/fox-toolkit/breaker/blob/main/LICENSE.txt.

package breaker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/fox-toolkit/fox"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func status(code int) fox.HandlerFunc {
	return func(c *fox.Context) {
		c.Writer().WriteHeader(code)
	}
}

func newTestBreaker(t *testing.T, next fox.HandlerFunc, opts ...Option) *breaker {
	t.Helper()
	m, err := newMiddleware(opts...)
	require.NoError(t, err)
	return m.newBreaker(next)
}

func serve(b *breaker, ctx context.Context) int {
	w := httptest.NewRecorder()
	r := httptest.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
	b.serve(fox.NewTestContextOnly(w, r))
	return w.Code
}

func serveN(b *breaker, n int) (rejected int) {
	for range n {
		if serve(b, context.Background()) == http.StatusServiceUnavailable {
			rejected++
		}
	}
	return rejected
}

func advance(b *breaker, d time.Duration) {
	b.start = b.start.Add(-d)
}

func TestOptions_Invalid(t *testing.T) {
	cases := []struct {
		name string
		opt  Option
	}{
		{"negative failure ratio", WithFailureRatio(-0.1)},
		{"failure ratio of one", WithFailureRatio(1)},
		{"zero max reject ratio", WithMaxRejectRatio(0)},
		{"max reject ratio above one", WithMaxRejectRatio(1.1)},
		{"negative min requests", WithMinRequests(-1)},
		{"window too short", WithWindow(999 * time.Millisecond)},
		{"window too long", WithWindow(10*time.Minute + 1)},
		{"nil failure", WithFailure(nil)},
		{"nil reject handler", WithRejectHandler(nil)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Middleware(tc.opt)
			assert.ErrorIs(t, err, ErrInvalidConfig)
		})
	}
}

func TestMiddleware_Reject(t *testing.T) {
	cases := []struct {
		name     string
		opts     []Option
		failures int
		total    int
		min, max int
	}{
		{"all failures capped at default", nil, 100, 100, 830, 970},
		{"custom max reject ratio", []Option{WithMaxRejectRatio(0.5)}, 100, 100, 400, 600},
		{"below failure ratio", nil, 30, 100, 0, 0},
		{"above failure ratio", nil, 60, 100, 100, 300},
		{"below min requests", nil, 19, 19, 0, 0},
		{"custom min requests", []Option{WithMinRequests(5)}, 10, 10, 830, 970},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var fail bool
			b := newTestBreaker(t, func(c *fox.Context) {
				if fail {
					c.Writer().WriteHeader(http.StatusInternalServerError)
				}
			}, tc.opts...)
			for i := range tc.total {
				if i == tc.total-1 {
					advance(b, 200*time.Millisecond)
				}
				fail = i < tc.failures
				assert.Zero(t, serveN(b, 1))
			}
			fail = true

			rejected := serveN(b, 1000)
			assert.GreaterOrEqual(t, rejected, tc.min)
			assert.LessOrEqual(t, rejected, tc.max)
		})
	}
}

func TestMiddleware_Recover(t *testing.T) {
	fail := true
	b := newTestBreaker(t, func(c *fox.Context) {
		if fail {
			c.Writer().WriteHeader(http.StatusInternalServerError)
		}
	})
	serveN(b, 100)
	advance(b, 200*time.Millisecond)
	serveN(b, 1)
	assert.Positive(t, serveN(b, 100))

	fail = false
	advance(b, DefaultWindow)
	serveN(b, 1)
	assert.Zero(t, serveN(b, 1000))
}

func TestMiddleware_RecoverGradually(t *testing.T) {
	fail := true
	b := newTestBreaker(t, func(c *fox.Context) {
		if fail {
			c.Writer().WriteHeader(http.StatusInternalServerError)
		}
	})
	serveN(b, 100)
	advance(b, 200*time.Millisecond)
	serveN(b, 1)

	fail = false
	for range 150 {
		serveN(b, 100)
		advance(b, 200*time.Millisecond)
	}
	assert.Zero(t, serveN(b, 1000))
}

func TestMiddleware_Panic(t *testing.T) {
	b := newTestBreaker(t, func(c *fox.Context) {
		panic("boom")
	})
	for range 30 {
		assert.PanicsWithValue(t, "boom", func() { serve(b, context.Background()) })
	}
	advance(b, 200*time.Millisecond)
	assert.PanicsWithValue(t, "boom", func() { serve(b, context.Background()) })
	assert.NotZero(t, b.threshold.Load())
}

func TestMiddleware_ClientCanceled(t *testing.T) {
	b := newTestBreaker(t, status(http.StatusInternalServerError))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for range 30 {
		serve(b, ctx)
	}
	advance(b, 200*time.Millisecond)
	b.next = status(http.StatusOK)
	serve(b, context.Background())
	assert.Zero(t, b.threshold.Load())
}

func TestMiddleware_DeadlineExceeded(t *testing.T) {
	b := newTestBreaker(t, status(http.StatusServiceUnavailable))
	ctx, cancel := context.WithDeadline(context.Background(), time.Now())
	defer cancel()
	for range 30 {
		serve(b, ctx)
	}
	advance(b, 200*time.Millisecond)
	serve(b, ctx)
	assert.NotZero(t, b.threshold.Load())
}

func TestMiddleware_CustomFailureAndReject(t *testing.T) {
	b := newTestBreaker(t, status(http.StatusTooManyRequests),
		WithFailure(func(c *fox.Context) bool {
			return c.Writer().Status() == http.StatusTooManyRequests
		}),
		WithRejectHandler(status(http.StatusTeapot)),
	)
	serveN(b, 100)
	advance(b, 200*time.Millisecond)
	serveN(b, 1)

	var teapot int
	for range 100 {
		if serve(b, context.Background()) == http.StatusTeapot {
			teapot++
		}
	}
	assert.Positive(t, teapot)
}

func TestMiddleware_DefaultReject(t *testing.T) {
	b := newTestBreaker(t, status(http.StatusInternalServerError), WithMaxRejectRatio(1))
	serveN(b, 100)
	advance(b, 200*time.Millisecond)
	serveN(b, 1)

	for range 1000 {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		b.serve(fox.NewTestContextOnly(w, r))
		if w.Code == http.StatusServiceUnavailable {
			assert.Equal(t, http.StatusText(http.StatusServiceUnavailable)+"\n", w.Body.String())
			return
		}
		require.Equal(t, http.StatusInternalServerError, w.Code)
	}
	t.Fatal("no request rejected")
}

func TestMiddleware_PerRoute(t *testing.T) {
	mw, err := Middleware()
	require.NoError(t, err)
	f := fox.MustRouter(fox.WithMiddleware(mw))
	f.MustAdd(fox.MethodGet, "/fail", status(http.StatusInternalServerError))
	f.MustAdd(fox.MethodGet, "/ok", status(http.StatusOK))

	call := func(path string) int {
		w := httptest.NewRecorder()
		f.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		return w.Code
	}
	for range 100 {
		call("/fail")
	}
	time.Sleep(150 * time.Millisecond)
	call("/fail")

	var rejected int
	for range 100 {
		if call("/fail") == http.StatusServiceUnavailable {
			rejected++
		}
		assert.Equal(t, http.StatusOK, call("/ok"))
	}
	assert.Positive(t, rejected)
}

func TestCounter(t *testing.T) {
	var c counter
	c.inc(0)
	c.inc(0)
	c.inc(0)
	assert.EqualValues(t, 3, c.load(0))
	assert.EqualValues(t, 3, c.load(9))
	assert.Zero(t, c.load(10))

	c.inc(10)
	assert.EqualValues(t, 1, c.load(10))
	c.inc(9)
	assert.EqualValues(t, 2, c.load(10))
}

func TestCounter_ConcurrentRotation(t *testing.T) {
	var c counter
	for range 5 {
		c.inc(0)
	}
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			for range 1000 {
				c.inc(10)
			}
		})
	}
	wg.Wait()
	assert.EqualValues(t, 16000, c.load(10))
}

func TestMiddleware_Concurrent(t *testing.T) {
	var n int
	var mu sync.Mutex
	b := newTestBreaker(t, func(c *fox.Context) {
		mu.Lock()
		n++
		fail := n%2 == 0
		mu.Unlock()
		if fail {
			c.Writer().WriteHeader(http.StatusInternalServerError)
		}
	}, WithWindow(time.Second))
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			serveN(b, 2000)
		})
	}
	wg.Wait()
}
