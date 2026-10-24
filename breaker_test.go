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
	"testing/synctest"
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

func newTestBreaker(t testing.TB, next fox.HandlerFunc, opts ...Option) *breaker {
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
		{"custom failure ratio", []Option{WithFailureRatio(0.2)}, 30, 100, 50, 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var fail bool
				b := newTestBreaker(t, func(c *fox.Context) {
					if fail {
						c.Writer().WriteHeader(http.StatusInternalServerError)
					}
				}, tc.opts...)
				for i := range tc.total {
					if i == tc.total-1 {
						time.Sleep(200 * time.Millisecond)
					}
					fail = i < tc.failures
					assert.Zero(t, serveN(b, 1))
				}
				fail = true

				rejected := serveN(b, 1000)
				assert.GreaterOrEqual(t, rejected, tc.min)
				assert.LessOrEqual(t, rejected, tc.max)
			})
		})
	}
}

func TestMiddleware_Recover(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fail := true
		b := newTestBreaker(t, func(c *fox.Context) {
			if fail {
				c.Writer().WriteHeader(http.StatusInternalServerError)
			}
		})
		serveN(b, 100)
		time.Sleep(200 * time.Millisecond)
		serveN(b, 1)
		assert.Positive(t, serveN(b, 100))

		fail = false
		time.Sleep(DefaultWindow)
		serveN(b, 1)
		assert.Zero(t, serveN(b, 1000))
	})
}

func TestMiddleware_RecoverGradually(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fail := true
		b := newTestBreaker(t, func(c *fox.Context) {
			if fail {
				c.Writer().WriteHeader(http.StatusInternalServerError)
			}
		})
		serveN(b, 100)
		time.Sleep(200 * time.Millisecond)
		serveN(b, 1)

		fail = false
		for range 150 {
			serveN(b, 100)
			time.Sleep(200 * time.Millisecond)
		}
		assert.Zero(t, serveN(b, 1000))
	})
}

func TestMiddleware_Panic(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := newTestBreaker(t, func(c *fox.Context) {
			panic("boom")
		})
		for range 30 {
			assert.PanicsWithValue(t, "boom", func() { serve(b, context.Background()) })
		}
		time.Sleep(200 * time.Millisecond)
		assert.PanicsWithValue(t, "boom", func() { serve(b, context.Background()) })
		assert.NotZero(t, b.threshold.Load())
	})
}

func TestMiddleware_RefreshAfterIdle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for range 100 {
			b := newTestBreaker(t, status(http.StatusInternalServerError))
			serveN(b, 100)
			time.Sleep(200 * time.Millisecond)
			serveN(b, 1)
			require.NotZero(t, b.threshold.Load())

			b.next = status(http.StatusOK)
			time.Sleep(DefaultWindow)
			assert.Equal(t, http.StatusOK, serve(b, context.Background()))
		}
	})
}

func TestMiddleware_LongIdle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := newTestBreaker(t, status(http.StatusInternalServerError), WithWindow(time.Second))
		serveN(b, 5)
		time.Sleep(time.Duration(1<<31+10) * 100 * time.Millisecond)
		serveN(b, 100)
		time.Sleep(200 * time.Millisecond)
		serveN(b, 1)
		assert.NotZero(t, b.threshold.Load())
	})
}

func TestMiddleware_FailurePanic(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := newTestBreaker(t, status(http.StatusOK), WithFailure(func(c *fox.Context) bool {
			panic("boom")
		}))
		for range 30 {
			assert.PanicsWithValue(t, "boom", func() { serve(b, context.Background()) })
		}
		time.Sleep(200 * time.Millisecond)
		assert.PanicsWithValue(t, "boom", func() { serve(b, context.Background()) })
		assert.NotZero(t, b.threshold.Load())
	})
}

func TestMiddleware_ClientCanceled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := newTestBreaker(t, status(http.StatusInternalServerError))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		for range 30 {
			serve(b, ctx)
		}
		time.Sleep(200 * time.Millisecond)
		b.next = status(http.StatusOK)
		serve(b, context.Background())
		assert.Zero(t, b.threshold.Load())
	})
}

func TestMiddleware_DeadlineExceeded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := newTestBreaker(t, status(http.StatusServiceUnavailable))
		ctx, cancel := context.WithDeadline(context.Background(), time.Now())
		defer cancel()
		for range 30 {
			serve(b, ctx)
		}
		time.Sleep(200 * time.Millisecond)
		serve(b, ctx)
		assert.NotZero(t, b.threshold.Load())
	})
}

func TestMiddleware_CustomFailureAndReject(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := newTestBreaker(t, status(http.StatusTooManyRequests),
			WithFailure(func(c *fox.Context) bool {
				return c.Writer().Status() == http.StatusTooManyRequests
			}),
			WithRejectHandler(status(http.StatusTeapot)),
		)
		serveN(b, 100)
		time.Sleep(200 * time.Millisecond)
		serveN(b, 1)

		var teapot int
		for range 100 {
			if serve(b, context.Background()) == http.StatusTeapot {
				teapot++
			}
		}
		assert.Positive(t, teapot)
	})
}

func TestMiddleware_DefaultReject(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := newTestBreaker(t, status(http.StatusInternalServerError), WithMaxRejectRatio(1))
		serveN(b, 100)
		time.Sleep(200 * time.Millisecond)
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
	})
}

func TestMiddleware_PerRoute(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
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
	})
}

func TestCounter(t *testing.T) {
	var c counter
	count := func(epoch uint32) uint64 {
		n, newer := c.load(epoch)
		require.False(t, newer)
		return n
	}
	c.inc(0)
	c.inc(0)
	c.inc(0)
	assert.EqualValues(t, 3, count(0))
	assert.EqualValues(t, 3, count(9))
	assert.Zero(t, count(10))

	c.inc(10)
	assert.EqualValues(t, 1, count(10))
	c.inc(9)
	assert.EqualValues(t, 2, count(10))

	n, newer := c.load(9)
	assert.Zero(t, n)
	assert.True(t, newer)
}

func TestCounter_ConcurrentRotation(t *testing.T) {
	var c counter
	for round := range uint32(500) {
		epoch := (round + 1) * numBuckets
		start := make(chan struct{})
		var wg sync.WaitGroup
		for range 16 {
			wg.Go(func() {
				<-start
				for range 10 {
					c.inc(epoch)
				}
			})
		}
		close(start)
		wg.Wait()
		n, _ := c.load(epoch)
		require.EqualValues(t, 160, n)
	}
}

func TestEvaluate_SkipsRestartedBucket(t *testing.T) {
	b := newTestBreaker(t, noop)
	for range 100 {
		b.buckets[1].requests.inc(1)
		b.buckets[1].accepts.inc(1)
	}
	b.buckets[1].accepts.inc(11)
	assert.Zero(t, b.evaluate(10*b.m.width))
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

	var requests, accepts uint64
	for i := range b.buckets {
		requests += b.buckets[i].requests.v.Load() & (1<<32 - 1)
		accepts += b.buckets[i].accepts.v.Load() & (1<<32 - 1)
	}
	assert.EqualValues(t, 16000, requests)
	assert.EqualValues(t, (n+1)/2, accepts)
}
