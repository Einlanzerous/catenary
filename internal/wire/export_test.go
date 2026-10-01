package wire

// ResetUnknownWireValues forgets every (enum, raw) pair already reported, so a
// test asserting "reported once" holds under `go test -count=N` too. Test-only:
// a _test.go file in package wire exports it to wire_test and to nothing else.
func ResetUnknownWireValues() {
	unknownMu.Lock()
	clear(unknownSeen)
	unknownMu.Unlock()
}
