//go:build linkpreviewtest

package linkpreview

// CANT-326 — the test-only network allowance.
//
// The end-to-end proof drives the REAL guarded fetcher against a fake origin,
// and a fake origin has to bind a non-public address, which the production
// judge refuses by design. Nothing in the production environment can be
// weakened by configuration (see environment), so the proof needs a way in, and
// this file is it: compiled ONLY under the build tag linkpreviewtest, which no
// build of the binary, the image or an ordinary `go test ./...` sets. The tag
// is set by verify.sh's CANT-326 step and nowhere else.
// TestTheTestAllowanceIsUnreachableFromANonTestBuild asserts, from an untagged
// build, that this file is outside the package, that the symbol is not in a
// built binary, and that no untagged source names it.
//
// What it widens, and all it widens: the listed addresses are judged public, any
// port is allowed, names resolve from a table (in sequence, so a name can answer
// publicly once and privately next: a DNS rebind), and the dialer redirects a
// port to the fake origin's. Every other address still goes through the
// production judge, at connect time, in the production dialer.

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"sync"
)

// TestNet describes the fake network a test fetcher runs in.
type TestNet struct {
	// Public are the addresses the judge treats as global unicast (the fake
	// origin's). Anything else is judged by the production table.
	Public []string
	// Hosts maps a name to its answers, one list per lookup, the last repeating.
	Hosts map[string][][]string
	// Ports maps the port a URL names (80) to the port the fake origin listens on.
	Ports map[int]int
}

// TestLookups counts the resolutions made for each name.
type TestLookups struct {
	mu    sync.Mutex
	count map[string]int
}

// Count is how many times host was resolved.
func (l *TestLookups) Count(host string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.count[host]
}

// NewForTest returns the production Guarded over the fake network n.
func NewForTest(cfg Config, n TestNet) (*Guarded, *TestLookups) {
	pub := map[netip.Addr]bool{}
	for _, p := range n.Public {
		pub[netip.MustParseAddr(p)] = true
	}
	lk := &TestLookups{count: map[string]int{}}
	env := environment{
		lookup: func(_ context.Context, host string) ([]netip.Addr, error) {
			lk.mu.Lock()
			defer lk.mu.Unlock()
			seq, ok := n.Hosts[host]
			if !ok {
				return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
			}
			i := lk.count[host]
			lk.count[host]++
			if i >= len(seq) {
				i = len(seq) - 1
			}
			var out []netip.Addr
			for _, s := range seq[i] {
				out = append(out, netip.MustParseAddr(s))
			}
			return out, nil
		},
		judge: func(a netip.Addr) (string, bool) {
			if pub[a] {
				return "", true
			}
			return judge(a)
		},
		allowPort: func(int) bool { return true },
	}
	g := newGuarded(cfg, env)
	tr := g.client.Transport.(*http.Transport)
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		if p, err := strconv.Atoi(port); err == nil {
			if to, ok := n.Ports[p]; ok {
				addr = net.JoinHostPort(host, strconv.Itoa(to))
			}
		}
		return g.dial(ctx, network, addr)
	}
	return g, lk
}
