package linkpreview

// CANT-326 — the build-tag test. This file has NO build tag on purpose: it runs
// in the ordinary build and asserts what that build cannot reach.

import (
	"go/build"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	allowanceFile   = "testnet.go"
	allowanceTag    = "linkpreviewtest"
	allowanceSymbol = "NewForTest"
)

func contains(files []string, name string) bool {
	for _, f := range files {
		if f == name {
			return true
		}
	}
	return false
}

func TestTheTestAllowanceIsUnreachableFromANonTestBuild(t *testing.T) {
	// 1. The package as an ordinary build sees it does not include the file; the
	// tag is what brings it in, and nothing else does.
	plain, err := build.Default.ImportDir(".", 0)
	if err != nil {
		t.Fatal(err)
	}
	if contains(plain.GoFiles, allowanceFile) {
		t.Fatalf("%s is compiled into an untagged build: %v", allowanceFile, plain.GoFiles)
	}
	if !contains(plain.IgnoredGoFiles, allowanceFile) {
		t.Fatalf("%s is not excluded by a build constraint (ignored: %v)", allowanceFile, plain.IgnoredGoFiles)
	}
	ctx := build.Default
	ctx.BuildTags = append(append([]string{}, ctx.BuildTags...), allowanceTag)
	tagged, err := ctx.ImportDir(".", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(tagged.GoFiles, allowanceFile) {
		t.Fatalf("the %s tag does not select %s: the guard would assert nothing", allowanceTag, allowanceFile)
	}

	// 2. No untagged source file in the module names the symbol, so nothing in
	// the production build can reference it.
	root := filepath.Join("..", "..")
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "build", ".dart_tool":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		src := string(b)
		if strings.Contains(src, "//go:build "+allowanceTag) {
			return nil
		}
		if strings.Contains(src, allowanceSymbol) {
			t.Errorf("%s names %s without the %s build constraint", p, allowanceSymbol, allowanceTag)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// 3. The built service binary does not contain the symbol.
	if testing.Short() {
		t.Skip("short: skipping the binary symbol check")
	}
	bin := filepath.Join(t.TempDir(), "catenary")
	build := exec.Command("go", "build", "-o", bin, "./cmd/catenary")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	nm, err := exec.Command("go", "tool", "nm", bin).Output()
	if err != nil {
		t.Fatalf("go tool nm: %v", err)
	}
	if !strings.Contains(string(nm), "linkpreview.(*Guarded).Preview") {
		t.Fatal("the symbol table does not even hold the production fetcher: the check reads nothing")
	}
	for _, sym := range []string{allowanceSymbol, "TestNet", "TestLookups"} {
		if strings.Contains(string(nm), "linkpreview."+sym) {
			t.Errorf("the built binary holds linkpreview.%s", sym)
		}
	}
}
