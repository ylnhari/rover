package server

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRequestFromHostMachine(t *testing.T) {
	mk := func(remoteAddr, xff string) *http.Request {
		r := httptest.NewRequest("GET", "/api/projects", nil)
		r.RemoteAddr = remoteAddr
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		return r
	}

	// Direct loopback connection: the browser is on the host.
	if !requestFromHostMachine(mk("127.0.0.1:54321", "")) {
		t.Error("loopback connection should be recognized as the host machine")
	}

	// A local front (tailscale serve) forwarding for a REMOTE client: the
	// connection is loopback but X-Forwarded-For names the real client, which
	// must not be mistaken for the host.
	if requestFromHostMachine(mk("127.0.0.1:54321", "100.101.102.103")) {
		t.Error("forwarded remote client must not be treated as the host machine")
	}

	// A local front forwarding for a local browser stays the host.
	if !requestFromHostMachine(mk("127.0.0.1:54321", "127.0.0.1")) {
		t.Error("forwarded loopback client should be recognized as the host machine")
	}

	// A remote client cannot spoof host status with a forged header: the
	// header is only trusted when the connection itself comes from loopback.
	if requestFromHostMachine(mk("203.0.113.9:44444", "127.0.0.1")) {
		t.Error("X-Forwarded-For from a non-loopback connection must be ignored")
	}

	// Plain remote connection.
	if requestFromHostMachine(mk("203.0.113.9:44444", "")) {
		t.Error("remote client should not be treated as the host machine")
	}

	// Garbage RemoteAddr never passes.
	if requestFromHostMachine(mk("not-an-address", "")) {
		t.Error("unparseable RemoteAddr must not be treated as the host machine")
	}

	// A connection arriving from one of this machine's own interface
	// addresses (how the host laptop reaches rover when it is bound to a
	// VPN/LAN IP) is the host.
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Skipf("cannot enumerate interfaces: %v", err)
	}
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok || ipn.IP.IsLoopback() || ipn.IP.To4() == nil {
			continue
		}
		if !requestFromHostMachine(mk(net.JoinHostPort(ipn.IP.String(), "50000"), "")) {
			t.Errorf("connection from own interface %s should be recognized as the host machine", ipn.IP)
		}
		return
	}
	t.Log("no non-loopback IPv4 interface; own-address case skipped")
}
