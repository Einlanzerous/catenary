package linkpreview

import (
	"net/netip"
	"testing"
)

// Every address class the plan lists, plus the addresses on either side of each
// boundary, because an off-by-one in a prefix is how a private range leaks.
func TestJudge(t *testing.T) {
	refused := []struct{ addr, class string }{
		{"0.0.0.0", "unspecified"},
		{"0.1.2.3", "unspecified"},
		{"127.0.0.1", "loopback"},
		{"127.255.255.254", "loopback"},
		{"10.0.0.1", "private"},
		{"10.255.255.255", "private"},
		{"172.16.0.1", "private"},
		{"172.31.255.255", "private"},
		{"192.168.0.1", "private"},
		{"192.168.255.255", "private"},
		{"169.254.0.1", "link_local"},
		{"169.254.169.254", "link_local"}, // the cloud metadata address
		{"100.64.0.1", "cgnat"},
		{"100.127.255.254", "cgnat"},
		{"224.0.0.1", "multicast"},
		{"239.255.255.250", "multicast"},
		{"255.255.255.255", "reserved"}, // broadcast
		{"240.0.0.1", "reserved"},
		{"192.0.0.8", "reserved"},
		{"192.0.2.1", "documentation"},
		{"198.51.100.7", "documentation"},
		{"203.0.113.9", "documentation"},
		{"198.18.0.1", "benchmarking"},
		{"198.19.255.255", "benchmarking"},

		{"::", "unspecified"},
		{"::1", "loopback"},
		{"fe80::1", "link_local"},
		{"fc00::1", "private"},
		{"fd12:3456::1", "private"},
		{"ff02::1", "multicast"},
		{"2001:db8::1", "documentation"},
		{"2001:0:4136:e378:8000:63bf:3fff:fdd2", "reserved"}, // Teredo
		{"100::1", "reserved"},                               // discard-only block, not in 2000::/3

		// IPv4-mapped IPv6 is judged as the IPv4 address inside it.
		{"::ffff:127.0.0.1", "loopback"},
		{"::ffff:10.1.2.3", "private"},
		{"::ffff:169.254.169.254", "link_local"},
		{"::ffff:100.64.0.1", "cgnat"},
		{"::ffff:0.0.0.0", "unspecified"},
		// NAT64 and 6to4 likewise.
		{"64:ff9b::7f00:1", "loopback"},
		{"64:ff9b::a9fe:a9fe", "link_local"},
		{"64:ff9b::c0a8:1", "private"},
		{"2002:7f00:1::", "loopback"},
		{"2002:a9fe:a9fe::1", "link_local"},
		{"2002:0a00:0001::", "private"},
	}
	for _, c := range refused {
		class, ok := judge(netip.MustParseAddr(c.addr))
		if ok {
			t.Errorf("%s was allowed, want refused as %s", c.addr, c.class)
			continue
		}
		if class != c.class {
			t.Errorf("%s refused as %q, want %q", c.addr, class, c.class)
		}
	}

	allowed := []string{
		"8.8.8.8", "1.1.1.1", "93.184.216.34",
		"172.15.255.255", "172.32.0.1", // either side of 172.16/12
		"100.63.255.255", "100.128.0.1", // either side of 100.64/10
		"169.253.255.255", "169.255.0.1",
		"192.167.255.255", "192.169.0.1",
		"198.17.255.255", "198.20.0.1",
		"223.255.255.255",
		"2606:4700:4700::1111",
		"2a00:1450:4001:81b::200e",
		// NAT64 and 6to4 wrapping a PUBLIC address are the public address.
		"64:ff9b::808:808",
		"2002:808:808::1",
		"::ffff:8.8.8.8",
	}
	for _, a := range allowed {
		if class, ok := judge(netip.MustParseAddr(a)); !ok {
			t.Errorf("%s was refused as %s, want allowed", a, class)
		}
	}
}

func TestJudgeIgnoresZone(t *testing.T) {
	a := netip.MustParseAddr("fe80::1%eth0")
	if _, ok := judge(a); ok {
		t.Fatal("a zoned link-local address was allowed")
	}
	if _, ok := judge(netip.Addr{}); ok {
		t.Fatal("the zero Addr was allowed")
	}
}
