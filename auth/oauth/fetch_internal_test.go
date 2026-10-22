// Internal: publicAddr is unexported, and the exported API can only reach it through loopback.
package oauth

import (
	"net/netip"
	"testing"
)

// publicAddr guards metadata fetches against internal addresses; through DNS a test only reaches loopback.
func TestPublicAddr(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name string
		addr string
		want bool
	}{
		{name: "public IPv4", addr: "93.184.216.34", want: true},
		{name: "public IPv6", addr: "2606:4700::1111", want: true},
		{name: "unspecified", addr: "::", want: false},
		{name: "loopback", addr: "127.0.0.1", want: false},
		{name: "IPv4-mapped loopback", addr: "::ffff:127.0.0.1", want: false},
		{name: "private", addr: "10.0.0.1", want: false},
		{name: "link-local", addr: "169.254.169.254", want: false},
		{name: "link-local multicast", addr: "ff02::1", want: false},
		{name: "interface-local multicast", addr: "ff01::1", want: false},
		{name: "global multicast", addr: "239.1.1.1", want: false},
		{name: "IPv4-compatible loopback", addr: "::127.0.0.1", want: false},
		{name: "NAT64 loopback", addr: "64:ff9b::7f00:1", want: false},
	}
	for _, p := range specialPurpose {
		testCases = append(testCases, struct {
			name string
			addr string
			want bool
		}{name: "special-purpose " + p.String(), addr: p.Addr().String(), want: false})
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			if got := publicAddr(netip.MustParseAddr(testCase.addr)); got != testCase.want {
				t.Errorf("publicAddr(%s): got %v, want %v", testCase.addr, got, testCase.want)
			}
		})
	}
}
