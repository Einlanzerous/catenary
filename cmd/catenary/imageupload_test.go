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

func TestWithExt(t *testing.T) {
	for _, c := range []struct{ name, ext, want string }{
		{"photo.jpg", ".jpg", "photo.jpg"},
		{"photo.JPEG", ".jpg", "photo.JPEG"},
		{"clip.gif", ".png", "clip.png"},
		{"photo.webp", ".png", "photo.png"},
		{"holiday", ".png", "holiday.png"},
		{"v1.2 final", ".png", "v1.2 final.png"},
		{"image.jpg", ".png", "image.png"},
	} {
		if got := withExt(c.name, c.ext); got != c.want {
			t.Errorf("withExt(%q, %q) = %q; want %q", c.name, c.ext, got, c.want)
		}
	}
}
