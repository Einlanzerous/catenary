// Command fcmprobe sends a single FCM message and prints the FULL response.
//
// It exists because r2collector only checks the HTTP status and discards the
// body, which is enough to run the experiment and not enough to debug it. When
// a device stops receiving, the questions are: did FCM accept the token, what
// message id did it mint, and does a `notification` message behave differently
// from a data-only one? None of those are answerable from a 200.
//
//	go run ./cmd/fcmprobe -key sa.json -token <fcm-token> [-notification]
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"golang.org/x/oauth2/google"
)

func main() {
	keyPath := flag.String("key", "", "service-account JSON")
	token := flag.String("token", "", "FCM registration token")
	notification := flag.Bool("notification", false, "send a display notification instead of data-only")
	validateOnly := flag.Bool("validate-only", false, "ask FCM to validate the message without delivering it")
	flag.Parse()

	if *keyPath == "" || *token == "" {
		fmt.Fprintln(os.Stderr, "fcmprobe: -key and -token are required")
		os.Exit(2)
	}

	keyBytes, err := os.ReadFile(*keyPath)
	must(err)
	var sa struct {
		ProjectID string `json:"project_id"`
	}
	must(json.Unmarshal(keyBytes, &sa))

	cfg, err := google.JWTConfigFromJSON(keyBytes, "https://www.googleapis.com/auth/firebase.messaging")
	must(err)
	tk, err := cfg.TokenSource(context.Background()).Token()
	must(err)

	msg := map[string]any{
		"token":   *token,
		"android": map[string]any{"priority": "HIGH"},
	}
	if *notification {
		// A display notification is handled by the system, so it lands on the
		// screen even if the app's Dart handler never runs. That distinguishes
		// "FCM cannot reach this device" from "FCM reaches it but the data
		// handler is not firing" — two very different findings.
		msg["notification"] = map[string]any{
			"title": "Catenary probe",
			"body":  "notification-path test " + time.Now().Format(time.TimeOnly),
		}
	} else {
		msg["data"] = map[string]string{
			"probe_id":   "fcmprobe-" + strconv.FormatInt(time.Now().Unix(), 10),
			"sent_at_ms": strconv.FormatInt(time.Now().UnixMilli(), 10),
		}
	}

	body := map[string]any{"message": msg}
	if *validateOnly {
		body["validate_only"] = true
	}
	b, _ := json.MarshalIndent(body, "", "  ")
	fmt.Printf("── request ──\n%s\n\n", b)

	url := fmt.Sprintf("https://fcm.googleapis.com/v1/projects/%s/messages:send", sa.ProjectID)
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+tk.AccessToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	must(err)
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	fmt.Printf("── response ── HTTP %d\n%s\n", resp.StatusCode, string(raw))
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "fcmprobe:", err)
		os.Exit(1)
	}
}
