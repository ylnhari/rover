package launcher

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
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

func newTestManager(projectsRoot string) *Manager {
	m := NewManager(projectsRoot)
	m.SetBindHost("127.0.0.1")
	return m
}
