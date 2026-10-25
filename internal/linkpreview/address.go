package linkpreview

import "net/netip"

// The address policy: the SSRF defense, in one table.
//
// An address is allowed only when it is a GLOBAL UNICAST address that is in none
// of the ranges below. IPv4-mapped IPv6, NAT64 (64:ff9b::/96) and 6to4
// (2002::/16) carry an IPv4 address inside an IPv6 one, so they are unwrapped
// and the embedded address is judged by the IPv4 list: http://[::ffff:127.0.0.1]/
// is loopback, not "some IPv6 address".
//
// IPv6 is allowed by exception rather than refused by exception: only
// 2000::/3 can be a global unicast address, so ::1, ::, fc00::/7, fe80::/10,
// ff00::/8 and every unassigned block are refused by not being in it, and the
// reserved holes inside it (documentation, Teredo, benchmarking, ORCHID) are
// listed. A new special-purpose block therefore fails closed.

type rangeClass struct {
	prefix netip.Prefix
	class  string
}

func mustPrefix(s, class string) rangeClass {
	return rangeClass{netip.MustParsePrefix(s), class}
}

var blockedV4 = []rangeClass{
	mustPrefix("0.0.0.0/8", "unspecified"), // "this network", includes 0.0.0.0
	mustPrefix("10.0.0.0/8", "private"),
	mustPrefix("100.64.0.0/10", "cgnat"),
	mustPrefix("127.0.0.0/8", "loopback"),
	mustPrefix("169.254.0.0/16", "link_local"), // includes 169.254.169.254, the metadata address
	mustPrefix("172.16.0.0/12", "private"),
	mustPrefix("192.0.0.0/24", "reserved"),
	mustPrefix("192.0.2.0/24", "documentation"),
	mustPrefix("192.88.99.0/24", "reserved"), // deprecated 6to4 relay anycast
	mustPrefix("192.168.0.0/16", "private"),
	mustPrefix("198.18.0.0/15", "benchmarking"),
	mustPrefix("198.51.100.0/24", "documentation"),
	mustPrefix("203.0.113.0/24", "documentation"),
	mustPrefix("224.0.0.0/4", "multicast"),
	mustPrefix("240.0.0.0/4", "reserved"), // includes 255.255.255.255, broadcast
}

var (
	globalV6 = netip.MustParsePrefix("2000::/3")

	blockedV6 = []rangeClass{
		mustPrefix("2001::/32", "reserved"), // Teredo: the embedded address is obfuscated, so refuse the block
		mustPrefix("2001:2::/48", "benchmarking"),
		mustPrefix("2001:10::/28", "reserved"), // ORCHID
		mustPrefix("2001:20::/28", "reserved"), // ORCHIDv2
		mustPrefix("2001:db8::/32", "documentation"),
		mustPrefix("3fff::/20", "documentation"),
	}

	nat64 = netip.MustParsePrefix("64:ff9b::/96")
	sixTo = netip.MustParsePrefix("2002::/16")
)

// judge returns ("", true) for an address a socket may connect to, and
// (class, false) with the reason class otherwise. The class is for the warn log
// line and the Error detail; it is never shown to a member.
func judge(a netip.Addr) (class string, ok bool) {
	a = a.WithZone("") // a zone is a local-link concept; the address is what matters
	if !a.IsValid() {
		return "invalid", false
	}
	if a.Is4In6() {
		return judge4(a.Unmap())
	}
	if a.Is4() {
		return judge4(a)
	}
	// IPv6 carrying an IPv4 address.
	b := a.As16()
	switch {
	case nat64.Contains(a):
		return judge4(netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}))
	case sixTo.Contains(a):
		return judge4(netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]}))
	}
	if a.IsLoopback() {
		return "loopback", false
	}
	if a.IsUnspecified() {
		return "unspecified", false
	}
	if a.IsMulticast() {
		return "multicast", false
	}
	if a.IsLinkLocalUnicast() {
		return "link_local", false
	}
	if a.IsPrivate() {
		return "private", false // fc00::/7
	}
	if !globalV6.Contains(a) {
		return "reserved", false
	}
	for _, r := range blockedV6 {
		if r.prefix.Contains(a) {
			return r.class, false
		}
	}
	return "", true
}

func judge4(a netip.Addr) (string, bool) {
	for _, r := range blockedV4 {
		if r.prefix.Contains(a) {
			return r.class, false
		}
	}
	return "", true
}
