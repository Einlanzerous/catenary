package main

import "testing"

func TestSafeFilename(t *testing.T) {
	long := ""
	for i := 0; i < 300; i++ {
		long += "é"
	}
	for _, c := range []struct{ in, want string }{
		{"", "image.jpg"},
		{"   ", "image.jpg"},
		{"..", "image.jpg"},
		{"beach.jpg", "beach.jpg"},
		{"../../etc/passwd", "passwd"},
		{`C:\Users\a\pic.png`, "pic.png"},
		{"a\x00b\nc.jpg", "abc.jpg"},
		{"x\u202eevil.jpg", "xevil.jpg"},
		{"dir/", "image.jpg"},
		{long, long[:240]},
	} {
		if got := safeFilename(c.in, ".jpg"); got != c.want {
			t.Errorf("safeFilename(%q) = %q; want %q", c.in, got, c.want)
		}
	}
}
