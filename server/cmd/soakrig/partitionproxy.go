package main

// partitionProxy is a loopback TCP pass-through a rig puts between ONE client
// and the server, so the rig — not the client — owns that client's network.
// A client is handed base() as its base URL; both clients build the socket
// URL and the /sync URL from that one address, so what the proxy does to a
// connection it does to the socket and to the catch-up together, for either
// implementation, with no change to any driver (CANT-46).
//
// It is CANT-109's severingProxy, promoted out of idle_test.go and given
// hold() and heal(): there is one pass-through here and not two.
//
// HELD REFUSES; IT DOES NOT SILENCE. While held, every connection is accepted
// and closed at once, so a client keeps running and keeps redialing on its
// backoff and every dial fails immediately. A network that accepts and never
// answers is the heartbeat's territory, which the idle and soak rigs cover
// per client, and it would make every schedule wait out a heartbeat interval.

import (
	"io"
	"net"
	"sync"
)

type partitionProxy struct {
	ln     net.Listener
	target string

	mu      sync.Mutex
	conns   map[net.Conn]struct{}
	held    bool
	refused int64 // connections closed unforwarded since the last hold() or resetRefused()

	// debugHoldIsNoOp plants a partition that is not one, for the control
	// that proves the rig's partition-bit check can fail. Test only.
	debugHoldIsNoOp bool
}

// newPartitionProxy listens on a free loopback port and forwards to target, a
// host:port. The caller closes it.
func newPartitionProxy(target string) (*partitionProxy, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p := &partitionProxy{ln: ln, target: target, conns: map[net.Conn]struct{}{}}
	go p.accept()
	return p, nil
}

func (p *partitionProxy) base() string { return "http://" + p.ln.Addr().String() }

func (p *partitionProxy) accept() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		if p.refuse(c) {
			continue
		}
		up, err := net.Dial("tcp", p.target)
		if err != nil {
			_ = c.Close()
			continue
		}
		// AGAIN, UNDER THE LOCK THAT ADMITS IT: a hold() that landed while the
		// upstream dial was in flight must not leave this connection carried.
		p.mu.Lock()
		if p.held {
			p.refused++
			p.mu.Unlock()
			_ = c.Close()
			_ = up.Close()
			continue
		}
		p.conns[c], p.conns[up] = struct{}{}, struct{}{}
		p.mu.Unlock()
		go p.pipe(up, c)
		go p.pipe(c, up)
	}
}

func (p *partitionProxy) refuse(c net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.held {
		return false
	}
	p.refused++
	_ = c.Close()
	return true
}

func (p *partitionProxy) pipe(dst, src net.Conn) {
	_, _ = io.Copy(dst, src)
	_ = dst.Close()
	_ = src.Close()
	p.mu.Lock()
	delete(p.conns, dst)
	delete(p.conns, src)
	p.mu.Unlock()
}

// severAll closes every connection currently carried — no close frame, an
// abrupt cut — without stopping the listener: a NEW connection afterward is
// still accepted and forwarded, exactly as a reconnect through a tunnel is.
func (p *partitionProxy) severAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.severLocked()
}

func (p *partitionProxy) severLocked() {
	for c := range p.conns {
		_ = c.Close()
	}
	p.conns = map[net.Conn]struct{}{}
}

// hold takes the client's network away: every connection being carried is
// closed abruptly, and every connection made from now on is accepted and
// closed at once, until heal().
func (p *partitionProxy) hold() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.debugHoldIsNoOp {
		return
	}
	p.held, p.refused = true, 0
	p.severLocked()
}

// heal gives it back, for connections made afterward. Nothing is resumed: a
// connection hold() closed is gone, as it would be on a real network.
func (p *partitionProxy) heal() {
	p.mu.Lock()
	p.held = false
	p.mu.Unlock()
}

// refusedDials is how many connections were closed unforwarded since the last
// hold(): the proof that a held client was redialing into a dead network.
func (p *partitionProxy) refusedDials() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.refused
}

// resetRefused returns that count and starts it again from zero, without
// touching the hold: for a rig that relaunches a client behind a held proxy
// and wants the new client's refused dial, not the old one's.
func (p *partitionProxy) resetRefused() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := p.refused
	p.refused = 0
	return n
}

// carrying is how many connections are being forwarded right now, both ends
// counted.
func (p *partitionProxy) carrying() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.conns)
}

// close stops listening and cuts whatever is carried.
func (p *partitionProxy) close() {
	_ = p.ln.Close()
	p.severAll()
}
