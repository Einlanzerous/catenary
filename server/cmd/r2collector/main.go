// Command r2collector is the R2 (IDEA-25) push probe harness: it registers the
// device, sends high-priority data messages through FCM HTTP v1, and records
// how long each one took to come back.
//
// # THE MEASUREMENT
//
// T0 is stamped immediately before the FCM HTTP call returns; T1 is stamped
// when the device's callback arrives. `T1 - T0` is the headline number. It is
// measured entirely on this machine's clock, so device clock skew cannot
// contaminate it, and it errs in the safe direction — it includes the
// callback's own network hop, so true delivery latency is somewhat lower.
//
// The device also reports its own wall clock, used only to cross-check and to
// separate "the push was slow" from "the callback was slow". Never as the
// headline.
//
// IDEA-25 is explicit that this must record LATENCIES, not pass/fail:
// "within seconds that is sometimes 4 minutes is a different product." So
// every send is a row, misses included, and the report is a distribution with
// its tail rather than an average.
package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"

	"golang.org/x/oauth2/google"
)

type send struct {
	ProbeID   string
	Phase     string
	SentAt    time.Time
	RecvAt    time.Time
	Path      string // foreground | background
	DeviceGap int64  // device_now - device_entry, ms: how slow the callback itself was
	Delivered bool
}

type collector struct {
	mu      sync.Mutex
	token   string // FCM registration token of the device
	model   string
	sends   map[string]*send
	order   []string
	outPath string
}

func (c *collector) record(s *send) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sends[s.ProbeID] = s
	c.order = append(c.order, s.ProbeID)
}

func (c *collector) writeCSV() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	f, err := os.Create(c.outPath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	w := csv.NewWriter(f)
	defer w.Flush()
	_ = w.Write([]string{"probe_id", "phase", "sent_at", "delivered", "latency_ms", "path", "callback_ms"})
	for _, id := range c.order {
		s := c.sends[id]
		lat := ""
		if s.Delivered {
			lat = strconv.FormatInt(s.RecvAt.Sub(s.SentAt).Milliseconds(), 10)
		}
		_ = w.Write([]string{
			s.ProbeID, s.Phase, s.SentAt.UTC().Format(time.RFC3339Nano),
			strconv.FormatBool(s.Delivered), lat, s.Path,
			strconv.FormatInt(s.DeviceGap, 10),
		})
	}
	return nil
}

// report prints the distribution. Percentiles, not a mean: the question is
// whether the SLOWEST deliveries are acceptable, and a mean hides exactly that.
func (c *collector) report(phase string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var lat []float64
	sent, got := 0, 0
	for _, id := range c.order {
		s := c.sends[id]
		if phase != "" && s.Phase != phase {
			continue
		}
		sent++
		if s.Delivered {
			got++
			lat = append(lat, s.RecvAt.Sub(s.SentAt).Seconds())
		}
	}
	if sent == 0 {
		return fmt.Sprintf("%-22s no sends", phase)
	}
	if len(lat) == 0 {
		return fmt.Sprintf("%-22s %d sent, %d delivered  *** ZERO DELIVERY ***", phase, sent, got)
	}
	sort.Float64s(lat)
	p := func(q float64) float64 {
		i := int(q * float64(len(lat)-1))
		return lat[i]
	}
	return fmt.Sprintf("%-22s %2d/%2d delivered  min %5.2fs  p50 %5.2fs  p90 %5.2fs  max %6.2fs",
		phase, got, sent, lat[0], p(.5), p(.9), lat[len(lat)-1])
}

func main() {
	keyPath := flag.String("key", "", "path to the Firebase service-account JSON")
	addr := flag.String("addr", "127.0.0.1:8110", "listen address (loopback; reach it through a tunnel)")
	auth := flag.String("token", "", "shared bearer token for /register and /receipt")
	out := flag.String("out", "r2-results.csv", "where to write the per-send CSV")
	flag.Parse()

	if *keyPath == "" || *auth == "" {
		log.Fatal("r2collector: -key and -token are required")
	}

	keyBytes, err := os.ReadFile(*keyPath)
	if err != nil {
		log.Fatalf("read key: %v", err)
	}
	var sa struct {
		ProjectID string `json:"project_id"`
	}
	if err := json.Unmarshal(keyBytes, &sa); err != nil {
		log.Fatalf("parse key: %v", err)
	}
	cfg, err := google.JWTConfigFromJSON(keyBytes, "https://www.googleapis.com/auth/firebase.messaging")
	if err != nil {
		log.Fatalf("jwt config: %v", err)
	}
	ts := cfg.TokenSource(context.Background())
	fcmURL := fmt.Sprintf("https://fcm.googleapis.com/v1/projects/%s/messages:send", sa.ProjectID)

	c := &collector{sends: map[string]*send{}, outPath: *out}

	guard := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+*auth && r.URL.Query().Get("t") != *auth {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next(w, r)
		}
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "device_registered": c.token != "", "model": c.model,
			"sends": len(c.order),
		})
	})

	// The probe posts here on launch and on every token rotation. A rotated
	// token that we kept sending to would look exactly like a device that
	// stopped receiving, which is the wrong conclusion to draw at 3 a.m.
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Token, Model string }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Token == "" {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		c.mu.Lock()
		changed := c.token != body.Token
		c.token = body.Token
		c.model = body.Model
		c.mu.Unlock()
		if changed {
			log.Printf("device registered (%s) token …%s", body.Model, tail(body.Token, 12))
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// Guarded, because a registration token is a capability: anyone holding it
	// can push to that device. Exists so the token can be read exactly rather
	// than transcribed off a screenshot — which is how a stale token got sent
	// to FCM and came back UNREGISTERED, costing an hour of misdiagnosis.
	mux.HandleFunc("/token", guard(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]string{"token": c.token, "model": c.model})
	}))

	mux.HandleFunc("/receipt", func(w http.ResponseWriter, r *http.Request) {
		recv := time.Now()
		var body struct {
			ProbeID       string `json:"probe_id"`
			DeviceEntryMs int64  `json:"device_entry_ms"`
			DeviceNowMs   int64  `json:"device_now_ms"`
			Path          string `json:"path"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		c.mu.Lock()
		if s, ok := c.sends[body.ProbeID]; ok && !s.Delivered {
			s.Delivered = true
			s.RecvAt = recv
			s.Path = body.Path
			s.DeviceGap = body.DeviceNowMs - body.DeviceEntryMs
			log.Printf("  ← %s delivered in %6.2fs (%s, callback %dms)",
				s.ProbeID, recv.Sub(s.SentAt).Seconds(), body.Path, s.DeviceGap)
		}
		c.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})

	srv := &http.Server{Addr: *addr, Handler: withGuardedPaths(mux, guard)}
	go func() {
		log.Printf("r2collector listening on %s (project %s)", *addr, sa.ProjectID)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	// ---- sender ----
	sendOne := func(phase string) error {
		c.mu.Lock()
		tok := c.token
		c.mu.Unlock()
		if tok == "" {
			return fmt.Errorf("no device registered yet")
		}
		id := fmt.Sprintf("%s-%d", phase, time.Now().UnixNano()%1e9)

		// DATA-ONLY, HIGH priority. A `notification` message is displayed by the
		// system and may never invoke the app's handler; data-only is what makes
		// the client fetch through the sync path, and it is the only shape whose
		// wake behaviour this test can actually measure.
		payload := map[string]any{
			"message": map[string]any{
				"token": tok,
				"data": map[string]string{
					"probe_id":   id,
					"sent_at_ms": strconv.FormatInt(time.Now().UnixMilli(), 10),
				},
				"android": map[string]any{"priority": "HIGH"},
			},
		}
		b, _ := json.Marshal(payload)
		tk, err := ts.Token()
		if err != nil {
			return fmt.Errorf("oauth: %w", err)
		}
		req, _ := http.NewRequest(http.MethodPost, fcmURL, bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+tk.AccessToken)
		req.Header.Set("Content-Type", "application/json")

		s := &send{ProbeID: id, Phase: phase, SentAt: time.Now()}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return fmt.Errorf("fcm: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode >= 300 {
			buf := new(bytes.Buffer)
			_, _ = buf.ReadFrom(resp.Body)
			return fmt.Errorf("fcm %d: %s", resp.StatusCode, buf.String())
		}
		c.record(s)
		log.Printf("→ %s sent (%s)", id, phase)
		return nil
	}

	// Control loop over stdin-free flags: phases are driven by the runner script
	// via HTTP so the harness can be poked while it runs.
	mux.HandleFunc("/send", guard(func(w http.ResponseWriter, r *http.Request) {
		phase := r.URL.Query().Get("phase")
		if phase == "" {
			phase = "manual"
		}
		if err := sendOne(phase); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	mux.HandleFunc("/soak", guard(func(w http.ResponseWriter, r *http.Request) {
		hours, _ := strconv.ParseFloat(r.URL.Query().Get("hours"), 64)
		if hours <= 0 {
			hours = 6
		}
		minGap, _ := strconv.Atoi(r.URL.Query().Get("min_gap_sec"))
		maxGap, _ := strconv.Atoi(r.URL.Query().Get("max_gap_sec"))
		if minGap <= 0 {
			minGap = 900
		}
		if maxGap <= minGap {
			maxGap = 2400
		}
		go func() {
			deadline := time.Now().Add(time.Duration(hours * float64(time.Hour)))
			log.Printf("soak: until %s, gaps %d–%ds", deadline.Format(time.TimeOnly), minGap, maxGap)
			for time.Now().Before(deadline) {
				// Randomised gaps, deliberately long. Doze buckets tighten the
				// longer a device sits undisturbed; a fixed short interval keeps
				// waking it and measures the easy case forever.
				gap := time.Duration(minGap+rand.Intn(maxGap-minGap)) * time.Second
				time.Sleep(gap)
				if err := sendOne("soak"); err != nil {
					log.Printf("soak send failed: %v", err)
				}
				_ = c.writeCSV()
			}
			log.Printf("soak complete\n%s", c.report("soak"))
			_ = c.writeCSV()
		}()
		w.WriteHeader(http.StatusAccepted)
	}))

	mux.HandleFunc("/report", guard(func(w http.ResponseWriter, r *http.Request) {
		_ = c.writeCSV()
		fmt.Fprintln(w, c.report("cold"))
		fmt.Fprintln(w, c.report("doze"))
		fmt.Fprintln(w, c.report("soak"))
		fmt.Fprintln(w, c.report(""))
	}))

	select {}
}

func withGuardedPaths(mux *http.ServeMux, guard func(http.HandlerFunc) http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/register", "/receipt":
			guard(mux.ServeHTTP)(w, r)
		default:
			mux.ServeHTTP(w, r)
		}
	})
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
