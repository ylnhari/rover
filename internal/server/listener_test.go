package server_test

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ylnhari/rover/internal/server"
)

func requireLoopbackListener(t *testing.T, ln net.Listener) {
	t.Helper()
	tcpAddr, ok := ln.Addr().(*net.TCPAddr)
	if !ok || tcpAddr.IP == nil || !tcpAddr.IP.IsLoopback() {
		_ = ln.Close()
		t.Fatalf("test listener must be loopback-only; got %q", ln.Addr())
	}
}

func listenOnLoopback(t *testing.T, port string) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		t.Fatalf("listen on loopback: %v", err)
	}
	requireLoopbackListener(t, ln)
	return ln
}

func newLoopbackHTTPTestServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	ts := &httptest.Server{
		Listener: listenOnLoopback(t, "0"),
		Config:   &http.Server{Handler: handler},
	}
	ts.Start()
	requireLoopbackListener(t, ts.Listener)
	return ts
}

func newConfiguredHandler(t *testing.T, cfg server.Config) http.Handler {
	t.Helper()
	if cfg.Addr == "" {
		cfg.Addr = "127.0.0.1:0"
	}
	host, _, err := net.SplitHostPort(cfg.Addr)
	if err != nil {
		t.Fatalf("test server address %q: %v", cfg.Addr, err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		t.Fatalf("test server config must be loopback-only; got %q", cfg.Addr)
	}
	return server.New(cfg).Handler()
}
