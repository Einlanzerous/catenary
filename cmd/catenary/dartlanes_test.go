package main

// CANT-42 row d — the Dart client as the client under test in CANT-102's
// kill-test and restore-test rigs: the Dart twins of the five lanes in
// tslanes_test.go. THERE IS NO SECOND COPY OF A LANE HERE. Each test below is
// one call into the same driverLane body the TypeScript test runs, with the
// Dart driver named in place of the Node one, so the two clients are held to
// one set of assertions by one piece of code.
//
// The client is dart-client/bin/driver.dart, built by `dart build cli` and run
// through the same tsDriver adapter (tsdriver_test.go, a symlink to soakrig's
// tsdriver.go) with tsDriverConfig.Native set. Its durable journal is the real
// SqliteJournal over a file, so `clientDies` relaunches a new process over
// the SQLite file the dead one committed to.
//
// Gated like the TypeScript lanes: CATENARY_DART_DRIVER unset skips, set and
// missing — or not executable — fails. verify.sh's Dart cohort step builds
// the driver and sets it.

import (
	"os"
	"os/exec"
	"testing"
)

// dartDriverExe is the built Dart driver, or a skip.
func dartDriverExe(t *testing.T) string {
	t.Helper()
	p := os.Getenv("CATENARY_DART_DRIVER")
	if p == "" {
		t.Skip("CATENARY_DART_DRIVER not set; skipping the Dart lane (verify.sh builds the driver and sets it)")
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("CATENARY_DART_DRIVER=%s: %v — the lane was asked to run the Dart driver and there is none; build it with `dart build cli -t bin/driver.dart -o build/driver` in dart-client/", p, err)
	}
	// `--probe` opens a SQLite database in memory through the journal's own
	// open and exits 0. A file that is there and will not execute, or a
	// bundle that cannot load its SQLite library, fails here — not later, as
	// N clients that never became ready.
	if out, err := exec.Command(p, "--probe").CombinedOutput(); err != nil {
		t.Fatalf("CATENARY_DART_DRIVER=%s cannot be executed, or cannot open SQLite: %v\n%s", p, err, out)
	}
	return p
}

var dartLane = driverLane{name: "Dart", script: dartDriverExe, native: true}

func TestTheDartClientResumesThroughTheKillTest(t *testing.T) { dartLane.resumesThroughTheKillTest(t) }

func TestTheDartClientSurvivesItsOwnDeathOverADurableJournal(t *testing.T) {
	dartLane.survivesItsOwnDeathOverADurableJournal(t)
}

func TestTheKillTestCatchesABrokenDartClient(t *testing.T) {
	dartLane.theKillTestCatchesABrokenClient(t)
}

func TestTheDartClientDiscardsALogTruncatedBelowItsCursor(t *testing.T) {
	dartLane.discardsALogTruncatedBelowItsCursor(t)
}

func TestTheDartClientSeversAHalfDeadSocketOnTheHeartbeat(t *testing.T) {
	dartLane.seversAHalfDeadSocketOnTheHeartbeat(t)
}
