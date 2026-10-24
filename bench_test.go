// Copyright 2026 Sylvain Müller. All rights reserved.
// Mount of this source code is governed by a MIT license that can be found
// at https://github.com/fox-toolkit/breaker/blob/main/LICENSE.txt.

package breaker

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/fox-toolkit/fox"
)

func noop(*fox.Context) {}

func BenchmarkMiddleware(b *testing.B) {
	br := newTestBreaker(b, noop)
	c := fox.NewTestContextOnly(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	b.ReportAllocs()
	for b.Loop() {
		br.serve(c)
	}
}

func BenchmarkMiddleware_Parallel(b *testing.B) {
	br := newTestBreaker(b, noop)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		c := fox.NewTestContextOnly(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
		for pb.Next() {
			br.serve(c)
		}
	})
}

func BenchmarkMiddleware_Rejecting(b *testing.B) {
	br := newTestBreaker(b, noop, WithMinRequests(0), WithRejectHandler(noop))
	br.m.k = 0
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		c := fox.NewTestContextOnly(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
		for pb.Next() {
			br.serve(c)
		}
	})
}
