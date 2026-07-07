package cmd

import "testing"

func TestIsLoopbackBind(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		{"127.0.0.1", true},
		{"localhost", true},
		{"::1", true},
		{"", false},              // all-interfaces bind (":2278") is NOT loopback
		{"0.0.0.0", false},       // all interfaces
		{"100.100.20.30", false}, // Tailscale / routable IP
		{"192.168.1.10", false},  // LAN IP
		{"::", false},
	}
	for _, c := range cases {
		if got := isLoopbackBind(c.host); got != c.want {
			t.Errorf("isLoopbackBind(%q) = %v, want %v", c.host, got, c.want)
		}
	}
}

func TestIsTailnetBind(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		{"100.100.20.30", true}, // Tailscale CGNAT
		{"100.64.0.1", true},    // CGNAT range start
		{"100.127.255.254", true},
		{"100.128.0.1", false}, // just past the /10
		{"192.168.1.10", false},
		{"127.0.0.1", false},
		{"", false},
		{"not-an-ip", false},
	}
	for _, c := range cases {
		if got := isTailnetBind(c.host); got != c.want {
			t.Errorf("isTailnetBind(%q) = %v, want %v", c.host, got, c.want)
		}
	}
}

func TestResolveProxyAuth(t *testing.T) {
	cases := []struct {
		mode, host, secret string
		want               bool
		wantErr            bool
	}{
		{"on", "192.168.1.10", "s", true, false},
		{"on", "192.168.1.10", "", false, true}, // on requires a secret
		{"off", "192.168.1.10", "s", false, false},
		{"auto", "127.0.0.1", "s", false, false},     // loopback: network is trusted
		{"auto", "100.100.20.30", "s", false, false}, // tailnet: WireGuard is the boundary
		{"auto", "192.168.1.10", "s", true, false},   // LAN: gate on
		{"auto", "", "s", true, false},               // all interfaces: gate on
		{"bogus", "127.0.0.1", "s", false, true},
	}
	for _, c := range cases {
		got, err := resolveProxyAuth(c.mode, c.host, c.secret)
		if (err != nil) != c.wantErr {
			t.Errorf("resolveProxyAuth(%q,%q,secret=%v) err = %v, wantErr %v", c.mode, c.host, c.secret != "", err, c.wantErr)
			continue
		}
		if got != c.want {
			t.Errorf("resolveProxyAuth(%q,%q,secret=%v) = %v, want %v", c.mode, c.host, c.secret != "", got, c.want)
		}
	}
}
