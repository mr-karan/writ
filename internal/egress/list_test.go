package egress

import (
	"net/netip"
	"strings"
	"testing"
)

func TestParseList(t *testing.T) {
	for _, c := range []struct {
		item string
		want entry
		err  string
	}{
		{item: "registry.opentofu.org", want: entry{host: "registry.opentofu.org", port: 443}},
		{item: "Nomad.Example.Internal.:80", want: entry{host: "nomad.example.internal", port: 80}},
		{item: "*.amazonaws.com", want: entry{host: "amazonaws.com", wildcard: true, port: 443}},
		{item: "*.s3.amazonaws.com:8443", want: entry{host: "s3.amazonaws.com", wildcard: true, port: 8443}},
		{item: "203.0.113.7", want: entry{ip: netip.MustParseAddr("203.0.113.7"), port: 443}},
		{item: "203.0.113.7:9000", want: entry{ip: netip.MustParseAddr("203.0.113.7"), port: 9000}},
		{item: "2001:db8::7", want: entry{ip: netip.MustParseAddr("2001:db8::7"), port: 443}},
		{item: "[2001:db8::7]", want: entry{ip: netip.MustParseAddr("2001:db8::7"), port: 443}},
		{item: "[2001:db8::7]:80", want: entry{ip: netip.MustParseAddr("2001:db8::7"), port: 80}},
		{item: "[::ffff:203.0.113.7]:80", want: entry{ip: netip.MustParseAddr("203.0.113.7"), port: 80}},
		{item: "10.0.0.5:8080", want: entry{ip: netip.MustParseAddr("10.0.0.5"), port: 8080}},

		{item: "*", err: "not HOST"},
		{item: "*.com", err: "two labels or more"},
		{item: "*.*.example.com", err: "is not a host name"},
		{item: "api.*.example.com", err: "a wildcard may only start an item"},
		{item: "https://example.com", err: "not HOST"},
		{item: "example.com/path", err: "not HOST"},
		{item: "example.com:0", err: "port \"0\""},
		{item: "example.com:65536", err: "port \"65536\""},
		{item: "example.com:https", err: "port \"https\""},
		{item: "[2001:db8::7", err: "not HOST"},
		{item: "-bad.example.com", err: "not HOST"},
		{item: "", err: "not HOST"},
		{item: "127.0.0.1:8080", err: "never connects to a loopback address"},
		{item: "[::1]", err: "never connects to a loopback address"},
		{item: "169.254.169.254:80", err: "never connects to a link-local address"},
		{item: "[fe80::1]:80", err: "never connects to a link-local address"},
		{item: "[fd00:ec2::254]:80", err: "never connects to the instance metadata address"},
		{item: "0.0.0.0", err: "never connects to an unspecified address"},
		{item: "224.0.0.1", err: "never connects to a multicast"},
		{item: "[fe80::1%eth0]:80", err: "zone"},
	} {
		t.Run(c.item, func(t *testing.T) {
			l, err := ParseList([]string{c.item})
			if c.err != "" {
				if err == nil || !strings.Contains(err.Error(), c.err) {
					t.Fatalf("ParseList(%q) = %+v, %v; want an error with %q", c.item, l, err, c.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseList(%q): %v", c.item, err)
			}
			if len(l.entries) != 1 || l.entries[0] != c.want {
				t.Fatalf("ParseList(%q) = %+v, want %+v", c.item, l.entries, c.want)
			}
		})
	}
}

func TestListMatch(t *testing.T) {
	l, err := ParseList([]string{"*.amazonaws.com", "registry.opentofu.org", "nomad.example.internal:80", "*.example.com:8443", "api.example.com:8443", "203.0.113.7:9000", "[2001:db8::7]"})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		host          string
		port          uint16
		listed, exact bool
	}{
		{"sts.amazonaws.com", 443, true, false},
		{"s3.eu-west-1.amazonaws.com", 443, true, false},
		// A wildcard is for subdomains only.
		{"amazonaws.com", 443, false, false},
		{"evilamazonaws.com", 443, false, false},
		{"sts.amazonaws.com.evil.example.org", 443, false, false},
		// An item with no port allows 443 and nothing else.
		{"sts.amazonaws.com", 80, false, false},
		{"registry.opentofu.org", 443, true, true},
		{"registry.opentofu.org", 80, false, false},
		{"nomad.example.internal", 80, true, true},
		{"nomad.example.internal", 443, false, false},
		{"x.example.com", 8443, true, false},
		{"api.example.com", 8443, true, true},
		{"api.example.com", 443, false, false},
		{"203.0.113.7", 9000, true, true},
		{"203.0.113.7", 443, false, false},
		{"2001:db8::7", 443, true, true},
		{"example.org", 443, false, false},
	} {
		var ip netip.Addr
		host := c.host
		if a, err := netip.ParseAddr(c.host); err == nil {
			ip, host = a, ""
		}
		listed, exact := l.match(host, ip, c.port)
		if listed != c.listed || exact != c.exact {
			t.Errorf("match(%s:%d) = listed %v, exact %v; want %v, %v", c.host, c.port, listed, exact, c.listed, c.exact)
		}
	}
	// A host name never matches an IP item, nor an IP a host item.
	if listed, _ := l.match("203.0.113.7", netip.Addr{}, 9000); listed {
		t.Error("a host name spelled as an IP item matched it")
	}
}

func TestParseMode(t *testing.T) {
	for _, s := range []string{"off", "report", "enforce"} {
		if m, err := ParseMode(s); err != nil || string(m) != s {
			t.Errorf("ParseMode(%q) = %q, %v", s, m, err)
		}
	}
	if _, err := ParseMode("on"); err == nil || !strings.Contains(err.Error(), `"off", "report", or "enforce"`) {
		t.Errorf("ParseMode(on): %v", err)
	}
}
