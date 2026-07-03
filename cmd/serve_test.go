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
		{"", false},           // all-interfaces bind (":2278") is NOT loopback
		{"0.0.0.0", false},    // all interfaces
		{"100.90.58.116", false}, // Tailscale / routable IP
		{"192.168.1.10", false},  // LAN IP
		{"::", false},
	}
	for _, c := range cases {
		if got := isLoopbackBind(c.host); got != c.want {
			t.Errorf("isLoopbackBind(%q) = %v, want %v", c.host, got, c.want)
		}
	}
}
