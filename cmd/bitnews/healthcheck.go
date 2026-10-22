package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"
)

// healthcheckTimeout bounds one health probe; the Docker HEALTHCHECK has its
// own, larger timeout.
const healthcheckTimeout = 3 * time.Second

// healthcheck implements `bitnews healthcheck` for the container HEALTHCHECK
// (the image has no shell or curl). It asks the daemon on the loopback
// interface, on the port taken from BITNEWS_LISTEN, for /api/v1/health and
// exits 0 on a 200 within three seconds, 1 on any other outcome, and 2 when
// BITNEWS_LISTEN is invalid. A daemon bound to one specific non-loopback
// address is therefore reported unhealthy.
func healthcheck(ctx context.Context, getenv func(string) (string, bool), stdout, stderr io.Writer) int {
	listen := "0.0.0.0:9494"
	if v, ok := getenv("BITNEWS_LISTEN"); ok && v != "" {
		listen = v
	}
	_, port, err := net.SplitHostPort(listen)
	if n, perr := strconv.Atoi(port); err != nil || perr != nil || n < 1 || n > 65535 {
		fmt.Fprintf(stderr, "bitnews healthcheck: BITNEWS_LISTEN=%q: must be host:port\n", listen)
		return 2
	}

	ctx, cancel := context.WithTimeout(ctx, healthcheckTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort("127.0.0.1", port)+"/api/v1/health", nil)
	if err != nil {
		fmt.Fprintf(stderr, "bitnews healthcheck: %v\n", err)
		return 1
	}
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(stderr, "bitnews healthcheck: %v\n", err)
		return 1
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10))
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(stderr, "bitnews healthcheck: unhealthy: status %d\n", resp.StatusCode)
		return 1
	}
	fmt.Fprintln(stdout, "ok")
	return 0
}
