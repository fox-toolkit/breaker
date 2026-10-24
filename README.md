[![Go Reference](https://pkg.go.dev/badge/github.com/fox-toolkit/breaker.svg)](https://pkg.go.dev/github.com/fox-toolkit/breaker)
[![tests](https://github.com/fox-toolkit/breaker/actions/workflows/tests.yaml/badge.svg)](https://github.com/fox-toolkit/breaker/actions?query=workflow%3Atests)
![GitHub release (latest SemVer)](https://img.shields.io/github/v/release/fox-toolkit/breaker)
![GitHub go.mod Go version](https://img.shields.io/github/go-mod/go-version/fox-toolkit/breaker)

# Breaker

Breaker is a middleware for [Fox](https://github.com/fox-toolkit/fox) that rejects requests to a failing route, using
the adaptive throttling described in the [Google SRE book](https://sre.google/sre-book/handling-overload/).

## Disclaimer
The current API is not yet stabilized. Breaking changes may occur before v1.0.0 and will be noted on the release note.

## Getting started
### Installation

````shell
go get -u github.com/fox-toolkit/breaker
````
### Feature
- One state per route, no lock on the request path and no allocation.
- Configurable failure ratio, window, cap and minimum number of requests.
- Configurable failure definition and rejection response.

### Usage
````go
package main

import (
	"errors"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"

	"github.com/fox-toolkit/breaker"
	"github.com/fox-toolkit/fox"
)

func main() {
	mw, err := breaker.Middleware(breaker.WithFailureRatio(0.3))
	if err != nil {
		log.Fatalln(err)
	}

	f := fox.MustRouter(fox.WithMiddleware(mw))

	target, _ := url.Parse("http://127.0.0.1:8081")
	f.MustAdd(fox.MethodAny, "/api/*{any}", fox.WrapH(httputil.NewSingleHostReverseProxy(target)))

	if err := http.ListenAndServe(":8080", f); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalln(err)
	}
}
````
