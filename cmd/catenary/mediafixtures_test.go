package main

// CANT-312 — the server's side of the shared image-fixture verdict.
//
// testdata/media/manifest.json is read by this test, the TypeScript pre-check
// test and the Dart pre-check test. Each posts or checks the same files and
// asserts the same accept or refuse verdict, so a client's supported-type list
// or size cap that drifts from the server's fails a build instead of surfacing
// as a 415 on someone's phone. This is the authority: it posts every fixture to
// a real POST /uploads?kind=image through setup()'s router over Postgres.

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/magos/catenary/internal/config"
)

type mediaManifest struct {
	ImageCapBytes int64 `json:"imageCapBytes"`
	Files         []struct {
		Name    string `json:"name"`
		Verdict string `json:"verdict"`
		Status  int    `json:"status"`
	} `json:"files"`
	Oversize struct {
		Verdict   string `json:"verdict"`
		Status    int    `json:"status"`
		HeaderHex string `json:"headerHex"`
		PadByte   int    `json:"padByte"`
	} `json:"oversize"`
}

const mediaFixtureDir = "../../testdata/media"

func loadMediaManifest(t *testing.T) mediaManifest {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(mediaFixtureDir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m mediaManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("manifest: %v", err)
	}
	return m
}

// oversizeImage is the generated oversize case: the manifest's JPEG header,
// padded to one byte past the cap. Never a committed file.
func oversizeImage(t *testing.T, m mediaManifest) []byte {
	t.Helper()
	header, err := hex.DecodeString(m.Oversize.HeaderHex)
	if err != nil {
		t.Fatal(err)
	}
	out := bytes.Repeat([]byte{byte(m.Oversize.PadByte)}, int(m.ImageCapBytes)+1)
	copy(out, header)
	return out
}

func TestTheManifestCapIsTheServersImageCap(t *testing.T) {
	m := loadMediaManifest(t)
	if m.ImageCapBytes != config.DefaultMaxImageBytes {
		t.Fatalf("manifest imageCapBytes = %d; DefaultMaxImageBytes = %d. The clients' tests read the manifest, so the two must be the same number", m.ImageCapBytes, config.DefaultMaxImageBytes)
	}
}

func TestEveryFixtureGetsTheVerdictTheClientsPreChecksMustMatch(t *testing.T) {
	m := loadMediaManifest(t)
	if len(m.Files) != 7 {
		t.Fatalf("manifest lists %d files; the plan's set is seven", len(m.Files))
	}
	// The plan's split, written here and not only in the manifest the clients
	// read: a manifest row cannot be flipped to make a red test green without
	// this test saying the settled set changed.
	want := map[string]int{
		"photo-exif-gps.jpg": http.StatusCreated,
		"image.png":          http.StatusCreated,
		"image.gif":          http.StatusCreated,
		"image.webp":         http.StatusCreated,
		"image.heic":         http.StatusUnsupportedMediaType,
		"image.svg":          http.StatusUnsupportedMediaType,
		"not-an-image.jpg":   http.StatusUnsupportedMediaType,
	}
	r := newUploadRig(t)

	for _, f := range m.Files {
		if w, ok := want[f.Name]; !ok || w != f.Status {
			t.Errorf("%s: manifest status %d; the settled split says %d (listed: %v)", f.Name, f.Status, w, ok)
		}
		body, err := os.ReadFile(filepath.Join(mediaFixtureDir, f.Name))
		if err != nil {
			t.Fatal(err)
		}
		if len(body) > 1<<20 {
			t.Errorf("%s is %d bytes; fixtures stay small", f.Name, len(body))
		}
		code, out := r.upload(r.token, "?kind=image&filename="+f.Name, body)
		if code != f.Status {
			t.Errorf("%s: POST /uploads?kind=image = %d (%v); manifest says %d (%s)", f.Name, code, out, f.Status, f.Verdict)
		}
		if (f.Verdict == "accept") != (code == http.StatusCreated) {
			t.Errorf("%s: verdict %q does not agree with status %d", f.Name, f.Verdict, code)
		}
	}

	over := oversizeImage(t, m)
	if int64(len(over)) != config.DefaultMaxImageBytes+1 {
		t.Fatalf("generated oversize file is %d bytes; want the cap plus one", len(over))
	}
	if code, out := r.upload(r.token, "?kind=image", over); code != m.Oversize.Status || code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversize: POST /uploads?kind=image = %d (%v); want 413", code, out)
	}
	// One byte under the same cap is not refused for size: the header is a
	// valid JPEG prefix but the body is not a decodable image, so it is a 415.
	// This pins that 413 above is the size check and not the type check.
	if code, _ := r.upload(r.token, "?kind=image", over[:len(over)-1]); code != http.StatusUnsupportedMediaType {
		t.Errorf("at the cap exactly: = %d; want 415 (past the size check, refused as a type)", code)
	}
}
