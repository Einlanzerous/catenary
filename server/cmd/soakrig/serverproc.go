package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// resolveBinary returns a path to a catenary binary, building one from the
// repo root if the caller did not supply one. THE SAME BINARY IS USED FOR
// EVERY START ACROSS THE RUN, including the restart after kill -9 — this is
// the real `catenary serve`, not a stand-in, which is the whole point of
// running it out of process.
func resolveBinary(cfg Config) (string, error) {
	if cfg.CatenaryBin != "" {
		if _, err := os.Stat(cfg.CatenaryBin); err != nil {
			return "", fmt.Errorf("-catenary-bin %s: %w", cfg.CatenaryBin, err)
		}
		return cfg.CatenaryBin, nil
	}
	root, err := repoRoot(cfg.RepoRoot)
	if err != nil {
		return "", err
	}
	out := filepath.Join(os.TempDir(), fmt.Sprintf("soakrig-catenary-%d", os.Getpid()))
	build := exec.Command("go", "build", "-o", out, "./cmd/catenary")
	build.Dir = root
	if b, err := build.CombinedOutput(); err != nil {
		return "", fmt.Errorf("go build ./cmd/catenary (in %s): %w\n%s", root, err, b)
	}
	return out, nil
}

// resolveDriver returns the TypeScript driver bundle (CANT-153), building it
// from the repo root if the caller did not name one — the same bargain as
// resolveBinary, and for the same reason: a bundle left over from an earlier
// checkout is a stale client under test. A named bundle that is missing is an
// error, never a quiet rebuild somewhere else.
func resolveDriver(cfg Config) (string, error) {
	if cfg.TSDriver != "" {
		if _, err := os.Stat(cfg.TSDriver); err != nil {
			return "", fmt.Errorf("-ts-driver %s: %w", cfg.TSDriver, err)
		}
		return cfg.TSDriver, nil
	}
	root, err := repoRoot(cfg.RepoRoot)
	if err != nil {
		return "", err
	}
	web := filepath.Join(root, "web")
	build := exec.Command("npm", "run", "--silent", "build:driver")
	build.Dir = web
	if b, err := build.CombinedOutput(); err != nil {
		return "", fmt.Errorf("npm run build:driver (in %s): %w\n%s", web, err, b)
	}
	return filepath.Join(web, "dist-transport-driver", "driver.js"), nil
}

// repoRoot finds the catenary repo root from this file's own compiled-in
// source path — server/cmd/soakrig/serverproc.go, three directories under the
// root — so a run started from any working directory finds ./cmd/catenary
// without the operator naming it. -catenary-root overrides it outright, for a
// checkout laid out differently or a build where runtime.Caller's path does
// not survive (e.g. -trimpath).
func repoRoot(override string) (string, error) {
	if override != "" {
		return filepath.Abs(override)
	}
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", errors.New("soakrig: cannot locate its own source to find the repo root — pass -catenary-root or -catenary-bin")
	}
	root, err := filepath.Abs(filepath.Join(filepath.Dir(file), "..", "..", ".."))
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(root, "cmd", "catenary")); err != nil {
		return "", fmt.Errorf("soakrig: %s does not look like the catenary repo root (no cmd/catenary): %w — pass -catenary-root or -catenary-bin", root, err)
	}
	return root, nil
}

// freePort hands back a port nothing is listening on right now. There is a
// small window between closing this probe and the subprocess binding the same
// number — CHECK-THEN-USE, not atomic — and on a busy box something else can
// take it in that gap (CANT-174: 1 run in 40 on imperial-construct). See
// startServer's retry, which is where that race is actually handled.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// bindConflict reports whether err — startServerOnce's own failure — was the
// subprocess's net.Listen losing freePort's race, as opposed to any other
// reason /readyz never answered (a bad DSN, a real crash, a slow box blowing
// the 15s bound). There is no error TYPE to check across the process
// boundary, only what the child printed: main.go's top-level handler writes
// "catenary: serve: listen tcp :PORT: bind: address already in use" to
// stderr, and waitReady folds that stderr into err's own text.
func bindConflict(err error) bool {
	return err != nil && strings.Contains(err.Error(), "address already in use")
}

// helloHistogram is CANT-24's decision record made evidence: every structured
// "hello" line the server logs, across every subprocess instance this run
// starts — the restart after kill -9 gets its OWN tail goroutine but writes
// into the SAME histogram, so the report is one picture of the whole run.
type helloHistogram struct {
	mu       sync.Mutex
	outcomes map[string]int
	deltas   []int64
}

func newHelloHistogram() *helloHistogram {
	return &helloHistogram{outcomes: map[string]int{}}
}

// helloLogLine is the fields hello.go logs that this harness reads. Anything
// else on the line — device_id, session_id, head, cursor, time, level — is
// read by an operator with grep, not by this struct.
type helloLogLine struct {
	Msg     string `json:"msg"`
	Outcome string `json:"outcome"`
	Delta   *int64 `json:"delta"`
}

func (h *helloHistogram) observe(line []byte) {
	var l helloLogLine
	if err := json.Unmarshal(line, &l); err != nil || l.Msg != "hello" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.outcomes[l.Outcome]++
	if l.Delta != nil {
		h.deltas = append(h.deltas, *l.Delta)
	}
}

func (h *helloHistogram) tail(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		h.observe(sc.Bytes())
	}
}

// startServer starts a fresh `catenary serve` and waits for /readyz. Called
// once at the top of a run (isRestart false) and again after killServer for
// the restart (isRestart true) — the two calls settle freePort's race
// (CANT-174) in opposite directions.
//
// ON THE FIRST START, no client has been built yet, so h.baseURL can still
// change out from under nobody: a bind conflict there — something else on the
// box won freePort's check-then-use race — is answered by picking a fresh
// port and retrying on it, rather than hammering the same stolen number three
// times and giving up.
//
// THE RESTART NEVER DOES. Every already-running client has this run's
// baseURL baked into its Config and cannot be told to redial somewhere else,
// so a restart retries the SAME port exactly as before CANT-174 — losing
// that race here is reported as a harness error after three attempts, never
// papered over by moving the server out from under connected clients.
func (h *harness) startServer(ctx context.Context, isRestart bool) error {
	h.instance++
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			h.logf("retrying server start after %v (attempt %d)", lastErr, attempt+1)
			select {
			case <-time.After(500 * time.Millisecond):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		err := h.startServerOnce(ctx)
		if err == nil {
			return nil
		}
		lastErr = err
		// attempt < 2 because a reassignment here only matters if another
		// attempt follows to use it — on the last attempt it would just
		// leave h.port pointing at a port nothing ever actually tried.
		if !isRestart && attempt < 2 && bindConflict(err) {
			newPort, perr := freePort()
			if perr != nil {
				return fmt.Errorf("start catenary serve: %w — and pick a replacement port after that bind conflict: %v", err, perr)
			}
			h.logf("bind conflict on the first start; moving to a fresh port", "old_port", h.port, "new_port", newPort)
			h.port = newPort
			h.baseURL = fmt.Sprintf("http://127.0.0.1:%d", newPort)
		}
	}
	return fmt.Errorf("start catenary serve after 3 attempts: %w", lastErr)
}

func (h *harness) startServerOnce(ctx context.Context) error {
	cmd := exec.Command(h.bin, "serve")
	cmd.Env = append(os.Environ(),
		"CATENARY_DATABASE_URL="+h.cfg.DBURL,
		fmt.Sprintf("CATENARY_PORT=%d", h.port),
		"CATENARY_LOG_FORMAT=json",
		"CATENARY_LOG_LEVEL=info",
	)

	// OUR OWN PIPE, NOT cmd.StdoutPipe(). os/exec's own doc on that method:
	// "Wait will close the pipe after seeing the command exit... it is thus
	// incorrect to call Wait before all reads from the pipe have completed" —
	// and killServer's whole contract is to call Wait (via procDone)
	// concurrently with the tail goroutine reading it, which is exactly the
	// race that warns against. cmd.Stdout = pw instead makes os/exec run its
	// OWN copy-to-pw goroutine, and Wait() is documented to block until that
	// copy finishes — which, because io.Pipe's Write is synchronous, cannot
	// happen until our tail goroutine has actually consumed every byte. A
	// `kill -9` closes the child's OS pipe at once, but everything already
	// written before that reaches pr regardless: CANT-27's review (this file
	// used cmd.StdoutPipe() when it found this).
	pr, pw := io.Pipe()
	cmd.Stdout = pw

	var stdoutLog, stderrLog io.Writer = io.Discard, io.Discard
	stderrBuf := &syncBuffer{}
	if h.cfg.ServerLogDir != "" {
		if f, err := os.Create(filepath.Join(h.cfg.ServerLogDir, fmt.Sprintf("server-%d.stdout.log", h.instance))); err == nil {
			stdoutLog = f
			h.logFiles = append(h.logFiles, f)
		}
		if f, err := os.Create(filepath.Join(h.cfg.ServerLogDir, fmt.Sprintf("server-%d.stderr.log", h.instance))); err == nil {
			stderrLog = f
			h.logFiles = append(h.logFiles, f)
		}
	}
	cmd.Stderr = io.MultiWriter(stderrBuf, stderrLog)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start: %w", err)
	}
	h.proc = cmd
	h.procStderr = stderrBuf
	procDone := make(chan struct{})
	go func() {
		h.waitErr = cmd.Wait()
		_ = pw.Close() // safe only AFTER Wait: see the comment above pr/pw.
		close(procDone)
	}()
	h.procDone = procDone

	tailDone := make(chan struct{})
	go func() {
		defer close(tailDone)
		h.hello.tail(io.TeeReader(pr, stdoutLog))
	}()
	h.tailDone = tailDone

	if err := h.waitReady(ctx); err != nil {
		h.killServer()
		return err
	}
	h.logf("server up", "instance", h.instance, "port", h.port)
	return nil
}

// syncBuffer is bytes.Buffer with a lock: cmd.Stderr is written from
// os/exec's own copying goroutine, and waitReady's timeout path reads it
// concurrently from this one. A plain bytes.Buffer there is a data race
// `-race` catches the moment a start takes long enough to hit the 15s bound
// (CANT-27's review).
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// waitReady polls /readyz — which pings the database, not just the process —
// until it answers 200 or ctx/the process itself ends.
func (h *harness) waitReady(ctx context.Context) error {
	wctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cl := &http.Client{Timeout: 2 * time.Second}
	for {
		select {
		case <-h.procDone:
			return fmt.Errorf("the server process exited before /readyz answered: %w\nstderr:\n%s", h.waitErr, h.procStderr.String())
		case <-wctx.Done():
			return fmt.Errorf("waiting for /readyz: %w\nstderr so far:\n%s", wctx.Err(), h.procStderr.String())
		case <-time.After(50 * time.Millisecond):
		}
		req, err := http.NewRequestWithContext(wctx, http.MethodGet, h.baseURL+"/readyz", nil)
		if err != nil {
			return err
		}
		resp, err := cl.Do(req)
		if err != nil {
			continue
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return nil
		}
	}
}

// killServer is `kill -9`: SIGKILL, no drain, no close frames — the same
// event cmd/catenary's own kill rig simulates in process, done here to a real
// OS process. It blocks until the process has been reaped and its stdout
// tail goroutine has seen EOF, so a subsequent startServer never races it for
// the port.
func (h *harness) killServer() {
	if h.proc == nil {
		return
	}
	_ = h.proc.Process.Kill()
	<-h.procDone
	<-h.tailDone
	h.proc = nil
}

func (h *harness) logf(msg string, args ...any) {
	h.cfg.logger().Info(msg, args...)
}
