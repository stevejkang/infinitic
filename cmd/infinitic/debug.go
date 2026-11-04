package main

import (
	"fmt"
	"net/http"
	"os"
	"time"
)

var debugOn bool

// enableDebug turns on --debug: where infinitic keeps its files, and each API request's method,
// URL without its query, status and duration. Headers and bodies carry tokens, so they never show.
func enableDebug() {
	debugOn = true
	cfg := loadConfig()
	debugf("home %s", cfg.home)
	debugf("state %s", cfg.statePath())
	debugf("claude %s, identity %s", cfg.claudeBin, cfg.claudeJSON)
}

func debugf(format string, args ...any) {
	if debugOn {
		fmt.Fprintf(os.Stderr, "debug: "+format+"\n", args...)
	}
}

type debugTransport struct{ base http.RoundTripper }

func (t debugTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()
	resp, err := t.base.RoundTrip(req)
	url := req.URL.Scheme + "://" + req.URL.Host + req.URL.Path
	if err != nil {
		debugf("%s %s failed after %s: %v", req.Method, url, time.Since(start).Round(time.Millisecond), err)
		return resp, err
	}
	debugf("%s %s -> %d (%s)", req.Method, url, resp.StatusCode, time.Since(start).Round(time.Millisecond))
	return resp, nil
}

// debugClient is c, logging its requests when --debug is on.
func debugClient(c *http.Client) *http.Client {
	if !debugOn {
		return c
	}
	base := c.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	c.Transport = debugTransport{base}
	return c
}
