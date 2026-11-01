package oauth

import (
	"net/netip"
	"testing"
)

func TestPublicAddr(t *testing.T) {
	t.Parallel()
	cases := []struct {
		addr   string
		public bool
	}{
		{"93.184.216.34", true},
		{"2606:2800:220:1:248:1893:25c8:1946", true},
		{"127.0.0.1", false},
		{"10.1.2.3", false},
		{"192.168.1.20", false},
		{"169.254.169.254", false},
		{"100.101.102.103", false}, // CGNAT, used by tailnets
		{"198.18.0.1", false},
		{"192.0.0.8", false},
		{"0.1.2.3", false},
		{"::ffff:10.0.0.1", false},
		{"fd7a:115c:a1e0::1", false}, // tailnet ULA
		{"64:ff9b::a00:1", false},    // NAT64 of 10.0.0.1
		{"64:ff9b::5db8:d822", true}, // NAT64 of a public address
		{"fe80::1", false},
	}
	for _, tc := range cases {
		if got := publicAddr(netip.MustParseAddr(tc.addr)); got != tc.public {
			t.Errorf("publicAddr(%s) = %v, want %v", tc.addr, got, tc.public)
		}
	}
}
