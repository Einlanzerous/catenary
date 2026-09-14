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
// number — see startServer's retry.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
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
// once at the top of a run and again after killServer for the restart — both
// times against the SAME port, because every client already has this
// process's base URL baked into its Config and cannot be told to redial
// somewhere else.
func (h *harness) startServer(ctx context.Context) error {
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
		if err := h.startServerOnce(ctx); err != nil {
			lastErr = err
			continue
		}
		return nil
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

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	var stdoutLog, stderrLog io.Writer = io.Discard, io.Discard
	var stderrBuf bytes.Buffer
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
	cmd.Stderr = io.MultiWriter(&stderrBuf, stderrLog)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start: %w", err)
	}
	h.proc = cmd
	h.procStderr = &stderrBuf
	procDone := make(chan struct{})
	go func() {
		h.waitErr = cmd.Wait()
		close(procDone)
	}()
	h.procDone = procDone

	tailDone := make(chan struct{})
	go func() {
		defer close(tailDone)
		h.hello.tail(io.TeeReader(stdout, stdoutLog))
	}()
	h.tailDone = tailDone

	if err := h.waitReady(ctx); err != nil {
		h.killServer()
		return err
	}
	h.logf("server up", "instance", h.instance, "port", h.port)
	return nil
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
