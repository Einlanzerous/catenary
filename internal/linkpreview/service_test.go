package linkpreview

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

func TestLimiterBurstThenRefill(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	l := NewLimiter(10, 5, clk.now) // one token every 6 s
	a, b := uuid.New(), uuid.New()

	for i := 0; i < 5; i++ {
		if ok, _ := l.Allow(a); !ok {
			t.Fatalf("call %d inside the burst refused", i)
		}
	}
	ok, wait := l.Allow(a)
	if ok {
		t.Fatal("the sixth call in the same instant was allowed")
	}
	if wait < 5900*time.Millisecond || wait > 6100*time.Millisecond {
		t.Errorf("wait = %v; want about 6s", wait)
	}
	if ok, _ := l.Allow(b); !ok {
		t.Error("another account shares the bucket")
	}
	clk.t = clk.t.Add(6 * time.Second)
	if ok, _ := l.Allow(a); !ok {
		t.Error("a token did not refill after 6s")
	}
	if ok, _ := l.Allow(a); ok {
		t.Error("two tokens refilled after 6s")
	}
	// A long idle refills to the burst and no further.
	clk.t = clk.t.Add(time.Hour)
	n := 0
	for ok, _ := l.Allow(a); ok; ok, _ = l.Allow(a) {
		n++
	}
	if n != 5 {
		t.Errorf("after an hour idle, %d calls allowed; want the burst of 5", n)
	}
}

func TestLimiterPrunesFullBucketsOnly(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	l := NewLimiter(10, 5, clk.now)
	drained := uuid.New()
	for i := 0; i < 5; i++ {
		l.Allow(drained)
	}
	for i := 0; i < maxIdleAccounts; i++ {
		l.Allow(uuid.New())
	}
	clk.t = clk.t.Add(time.Second) // nothing has refilled to full yet
	l.Allow(uuid.New())            // triggers a prune
	if ok, _ := l.Allow(drained); ok {
		t.Error("pruning forgot a drained bucket, handing back a full burst")
	}
}

func TestCacheExpiryAndBound(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	c := NewCache[int](2, clk.now)
	c.Put("a", 1, time.Minute)
	c.Put("b", 2, 2*time.Minute)
	if v, ok := c.Get("a"); !ok || v != 1 {
		t.Fatalf("a = %v %v", v, ok)
	}
	// Full: a third entry evicts the one expiring first.
	c.Put("c", 3, time.Minute)
	if _, ok := c.Get("a"); ok {
		t.Error("a survived a full cache")
	}
	if _, ok := c.Get("b"); !ok {
		t.Error("b was evicted though it outlives a")
	}
	clk.t = clk.t.Add(90 * time.Second)
	if _, ok := c.Get("c"); ok {
		t.Error("c outlived its ttl")
	}
	if _, ok := c.Get("b"); !ok {
		t.Error("b expired early")
	}
	// Replacing a key does not evict another.
	c.Put("b", 20, time.Minute)
	c.Put("d", 4, time.Minute)
	if v, ok := c.Get("b"); !ok || v != 20 {
		t.Errorf("b = %v %v", v, ok)
	}
}

func TestCanonicalIsSyntaxOnlyAndAPortIsABadURL(t *testing.T) {
	for in, want := range map[string]string{
		"HTTPS://Example.COM/a?b=1#frag": "https://example.com/a?b=1",
		"http://bücher.example/":         "http://xn--bcher-kva.example/",
		"https://example.com:443/x":      "https://example.com:443/x",
	} {
		got, err := Canonical(in)
		if err != nil || got != want {
			t.Errorf("Canonical(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{
		"", "ftp://example.com/", "example.com", "https://u:p@example.com/", "https:///x",
		"https://example.com:8080/", "http://127.0.0.1:22/",
	} {
		_, err := Canonical(in)
		if ClassOf(err) != ClassBadURL {
			t.Errorf("Canonical(%q) = %v; want class bad_url", in, err)
		}
	}
	// A loopback literal on a good port is NOT a syntax matter: the dialer judges
	// it, and Canonical must not become a second place that does.
	if _, err := Canonical("http://127.0.0.1/"); err != nil {
		t.Errorf("Canonical judged an address: %v", err)
	}
}
