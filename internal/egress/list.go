// Package egress decides where an OpenTofu run of writ serve may connect,
// and is the proxy the run connects through. A run with egress on has a
// network namespace of its own with only loopback, so its one way out is
// the proxy that HTTP_PROXY and HTTPS_PROXY point to. The proxy reads the
// host and port of each request, CONNECT or plain HTTP, decides it
// against the environment's list before it connects, resolves the host on
// writ serve's own resolver, and connects only to an address it checked.
// It never connects to a loopback, link-local, multicast, or unspecified
// address, to the instance metadata service, or to a NAT64 address of
// any of them. It records each
// destination with its verdict and a count, and nothing of a request but
// its host and port. A provider install's proxy has no list: it allows
// any public destination and refuses a private one.
//
// It imports only the standard library, so its rules can be read here and
// nowhere else.
package egress

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
)

// Mode is writ serve's egress_mode.
type Mode string

const (
	// Off leaves every run on the host's network, as writ serve ran before
	// egress existed.
	Off Mode = "off"
	// Report gives every run its own network namespace and the proxy, which
	// lets a destination the list lacks through and records it.
	Report Mode = "report"
	// Enforce refuses a destination the list lacks.
	Enforce Mode = "enforce"
)

// On reports whether m gives a run its own network: report or enforce.
// Any other value, the zero value among them, is off.
func (m Mode) On() bool { return m == Report || m == Enforce }

// ParseMode reads an egress_mode.
func ParseMode(s string) (Mode, error) {
	switch m := Mode(s); m {
	case Off, Report, Enforce:
		return m, nil
	}
	return "", fmt.Errorf("egress_mode must be %q, %q, or %q, not %q", Off, Report, Enforce, s)
}

// DefaultPort is the port of an entry that names none.
const DefaultPort = 443

// List is an environment's egress list. Its zero value lists nothing.
type List struct {
	entries []entry
}

// entry is one item of a List: a host name, a wildcard's domain, or an IP
// address, and one port.
type entry struct {
	// host is a lowercase host name, or for a wildcard the domain after
	// "*.". It is "" for an IP address.
	host     string
	wildcard bool
	ip       netip.Addr
	port     uint16
}

// ParseList reads the items of an egress list: HOST, *.DOMAIN, or an IP
// address, each with an optional :PORT. An IPv6 address with a port is
// written [ADDR]:PORT. An item with no port allows 443 only. A wildcard
// matches the domain's subdomains, not the domain itself, and needs a
// domain of at least two labels, so *.com is refused.
func ParseList(items []string) (List, error) {
	var l List
	for _, s := range items {
		e, err := parseEntry(s)
		if err != nil {
			return List{}, fmt.Errorf("egress item %q: %w", s, err)
		}
		l.entries = append(l.entries, e)
	}
	return l, nil
}

const itemShape = "it is not HOST, *.DOMAIN, or an IP address, with an optional :PORT"

func parseEntry(s string) (entry, error) {
	e := entry{port: DefaultPort}
	if strings.ContainsAny(s, "/@?# ") {
		return entry{}, errors.New(itemShape)
	}
	hostPart := s
	bare := s
	if inner, ok := strings.CutPrefix(s, "["); ok && strings.HasSuffix(inner, "]") {
		bare = strings.TrimSuffix(inner, "]")
	}
	if a, err := netip.ParseAddr(bare); err == nil {
		// An IP address with no port, whose colons, for IPv6, are not one.
		return ipEntry(a, e.port)
	}
	if h, p, err := net.SplitHostPort(s); err == nil {
		port, err := parsePort(p)
		if err != nil {
			return entry{}, err
		}
		hostPart, e.port = h, port
	} else if strings.ContainsAny(s, ":[]") {
		return entry{}, errors.New(itemShape)
	}
	if a, err := netip.ParseAddr(hostPart); err == nil {
		return ipEntry(a, e.port)
	}
	if rest, ok := strings.CutPrefix(hostPart, "*."); ok {
		host, ok := hostName(rest)
		switch {
		case !ok:
			return entry{}, fmt.Errorf("%q after \"*.\" is not a host name", rest)
		case !strings.Contains(host, "."):
			return entry{}, fmt.Errorf("a wildcard needs a domain of two labels or more, such as *.example.com")
		}
		e.host, e.wildcard = host, true
		return e, nil
	}
	host, ok := hostName(hostPart)
	if !ok {
		return entry{}, errors.New(itemShape + "; a wildcard may only start an item")
	}
	e.host = host
	return e, nil
}

func ipEntry(a netip.Addr, port uint16) (entry, error) {
	if a.Zone() != "" {
		return entry{}, fmt.Errorf("an address with a zone is never connected to")
	}
	a = a.Unmap()
	if why := never(a); why != "" {
		return entry{}, fmt.Errorf("the egress proxy never connects to %s", why)
	}
	return entry{ip: a, port: port}, nil
}

func parsePort(p string) (uint16, error) {
	n, err := strconv.ParseUint(p, 10, 16)
	if err != nil || n == 0 {
		return 0, fmt.Errorf("port %q is not a number from 1 to 65535", p)
	}
	return uint16(n), nil
}

// hostName lowercases s, drops one trailing dot, and reports whether the
// rest is a host name: labels of letters, digits, '-', and '_', of 1 to
// 63 bytes, that neither start nor end with '-', 253 bytes in all.
func hostName(s string) (string, bool) {
	s = strings.TrimSuffix(strings.ToLower(s), ".")
	if s == "" || len(s) > 253 {
		return "", false
	}
	for label := range strings.SplitSeq(s, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !('a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-' || c == '_') {
				return "", false
			}
		}
	}
	return s, true
}

// match reports whether l lists the destination, a host name already
// passed through hostName or an unmapped IP address, and whether an item
// lists it by its exact name rather than by a wildcard.
func (l List) match(host string, ip netip.Addr, port uint16) (listed, exact bool) {
	for _, e := range l.entries {
		switch {
		case e.port != port:
		case ip.IsValid():
			if e.ip == ip {
				return true, true
			}
		case e.ip.IsValid():
		case !e.wildcard && e.host == host:
			return true, true
		case e.wildcard && strings.HasSuffix(host, "."+e.host):
			listed = true
		}
	}
	return listed, false
}

// metadata6 is the instance metadata service's IPv6 address on EC2. Its
// IPv4 address, 169.254.169.254, is link-local.
var metadata6 = netip.MustParseAddr("fd00:ec2::254")

// thisNetwork is 0.0.0.0/8, whose addresses no host may connect to and
// which Linux takes for its own.
var thisNetwork = netip.MustParsePrefix("0.0.0.0/8")

// never says why the proxy never connects to a, or "". a is unmapped. An
// address of a NAT64 prefix is also checked by each IPv4 address it may
// embed, since a translator on the network connects to that address.
func never(a netip.Addr) string {
	if why := reserved(a); why != "" {
		return why
	}
	for _, v4 := range embedded4(a) {
		if why := reserved(v4); why != "" {
			return "a NAT64 address of " + why
		}
	}
	return ""
}

// reserved says why no host connects to a, or "".
func reserved(a netip.Addr) string {
	switch {
	case a.IsLoopback():
		return "a loopback address"
	case a.IsLinkLocalUnicast():
		return "a link-local address, where the instance metadata service is"
	case a == metadata6:
		return "the instance metadata address"
	case a.IsUnspecified() || thisNetwork.Contains(a):
		return "an unspecified address"
	case a.IsMulticast() || a.IsLinkLocalMulticast() || a.IsInterfaceLocalMulticast() || a == netip.AddrFrom4([4]byte{255, 255, 255, 255}):
		return "a multicast or broadcast address"
	}
	return ""
}

// The NAT64 prefixes: the well-known prefix of RFC 6052, and the prefix
// RFC 8215 keeps for local use, which a network divides into prefixes of
// its own.
var (
	nat64Known = netip.MustParsePrefix("64:ff9b::/96")
	nat64Local = netip.MustParsePrefix("64:ff9b:1::/48")
)

// nat64Layouts are the bytes of an IPv6 address that hold the embedded
// IPv4 address, by RFC 6052, for each prefix length a translator under
// the local-use prefix may have: /48, /56, /64, and /96. Byte 8 is never
// one of them.
var nat64Layouts = [][4]int{{6, 7, 9, 10}, {7, 9, 10, 11}, {9, 10, 11, 12}, {12, 13, 14, 15}}

// embedded4 returns the IPv4 addresses that a, an address of a NAT64
// prefix, may embed: the last 32 bits under the well-known prefix, and
// under the local-use prefix each layout of nat64Layouts that a fits,
// whose byte 8 and the bytes past the IPv4 address are zero, as RFC 6052
// has them.
func embedded4(a netip.Addr) []netip.Addr {
	b := a.As16()
	if nat64Known.Contains(a) {
		return []netip.Addr{netip.AddrFrom4([4]byte(b[12:]))}
	}
	if !nat64Local.Contains(a) {
		return nil
	}
	var out []netip.Addr
	for _, at := range nat64Layouts {
		fits := at[3] == 15 || b[8] == 0 && !slices.ContainsFunc(b[at[3]+1:], func(x byte) bool { return x != 0 })
		if fits {
			out = append(out, netip.AddrFrom4([4]byte{b[at[0]], b[at[1]], b[at[2]], b[at[3]]}))
		}
	}
	return out
}

var private = []netip.Prefix{
	// Shared address space, which some networks use as private.
	netip.MustParsePrefix("100.64.0.0/10"),
	// IPv6 site-local, which RFC 3879 retired.
	netip.MustParsePrefix("fec0::/10"),
}

// isPrivate reports whether a is an address of a private network: RFC
// 1918, unique local IPv6, shared address space, or IPv6 site-local, or
// a NAT64 address of one. Every address of the local-use NAT64 prefix is
// private, since where it leads is the network's own choice. A host name
// that only a wildcard lists may not resolve to one, so a name under a
// public domain cannot lead into the network writ serve runs in.
func isPrivate(a netip.Addr) bool {
	switch {
	case nat64Local.Contains(a):
		return true
	case nat64Known.Contains(a):
		a = embedded4(a)[0]
	}
	if a.IsPrivate() {
		return true
	}
	for _, p := range private {
		if p.Contains(a) {
			return true
		}
	}
	return false
}
