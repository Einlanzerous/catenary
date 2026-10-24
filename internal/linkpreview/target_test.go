package linkpreview

import (
	"strings"
	"testing"
)

func TestParseTarget(t *testing.T) {
	good := []struct{ in, want string }{
		{"https://example.com/a?b=c#frag", "https://example.com/a?b=c"},
		{"HTTP://Example.COM/", "http://example.com/"},
		{"http://example.com:80/x", "http://example.com:80/x"},
		{"https://example.com:443", "https://example.com:443"},
		{"https://b\u00FCcher.example/", "https://xn--bcher-kva.example/"},
		{"https://[2606:4700:4700::1111]/", "https://[2606:4700:4700::1111]/"},
		{"https://example.com/" + strings.Repeat("a", 2000), ""}, // under the cap; checked for no error only
	}
	for _, c := range good {
		u, err := parseTarget(c.in, prodPort)
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		if c.want != "" && u.String() != c.want {
			t.Errorf("%q -> %q, want %q", c.in, u.String(), c.want)
		}
	}

	bad := []struct {
		in    string
		class Class
	}{
		{"", ClassBadURL},
		{"ftp://example.com/", ClassBadURL},
		{"file:///etc/passwd", ClassBadURL},
		{"javascript:alert(1)", ClassBadURL},
		{"//example.com/", ClassBadURL},
		{"example.com", ClassBadURL},
		{"https://", ClassBadURL},
		{"https://user:pw@example.com/", ClassBadURL},
		{"https://user@example.com/", ClassBadURL},
		{"https://example.com/" + strings.Repeat("a", 2100), ClassBadURL},
		{"https://exa mple.com/", ClassBadURL},
		{"http://example.com:8080/", ClassBlocked},
		{"https://example.com:22/", ClassBlocked},
		{"http://127.0.0.1:6379/", ClassBlocked},
	}
	for _, c := range bad {
		u, err := parseTarget(c.in, prodPort)
		if err == nil {
			t.Errorf("%q was accepted as %v", c.in, u)
			continue
		}
		if err.Class != c.class {
			t.Errorf("%q: class %s, want %s", c.in, err.Class, c.class)
		}
	}
}

func TestErrorTextNeverCarriesTheURL(t *testing.T) {
	_, err := parseTarget("ftp://secret.example/path?token=abc", prodPort)
	if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "token") {
		t.Fatalf("error text leaks the URL: %v", err)
	}
}
