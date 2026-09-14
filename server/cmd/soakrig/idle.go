package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/magos/catenary/internal/client"
	"github.com/magos/catenary/internal/wire"
)

// runIdle is CANT-23's remaining clause: an hour of idle through the real
// Cloudflare tunnel needs a pinging client pointed at a deployed URL with a
// real device token, and no local server. This function is that client — the
// choice of WHERE to point it, and whether to run it at all against anything
// deployed, belongs to whoever calls soakrig idle, never to this code.
//
// It runs until ctx ends (SIGINT/SIGTERM, or -run-for) or Close returns, and
// exits 1 if the client never once reached `ready` — the harness/server
// distinction does not apply here: there is no local server for this process
// to have started, so a failure to connect is unambiguous.
func runIdle(ctx context.Context, baseURL, token, deviceID, clientInfo string, runFor time.Duration, logger *slog.Logger) int {
	if runFor > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, runFor)
		defer cancel()
	}

	c, err := client.New(client.Config{
		BaseURL: baseURL, AccessToken: token, DeviceID: wire.Uuid(deviceID),
		ClientInfo: clientInfo, Logger: logger,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "soakrig idle: %v\n", err)
		return 2
	}

	done := make(chan struct{})
	go func() { defer close(done); _ = c.Run(ctx) }()

	started := time.Now()
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-t.C:
			s := c.Status()
			fmt.Fprintf(os.Stderr, "[idle] alive %s · ready=%v · dials=%d · readys=%d · pings=%d · pongs=%d · severs=%d · last_rtt=%s\n",
				time.Since(started).Round(time.Second), s.Ready, s.Dials, s.Readys,
				s.PingsSent, s.PongsReceived, s.HeartbeatSevers, s.LastRTT.Round(time.Millisecond))
		}
	}

	c.Close()
	<-done

	s := c.Status()
	fmt.Fprintf(os.Stderr, "[idle] DONE total=%s dials=%d readys=%d pings=%d pongs=%d severs=%d close_statuses=%v\n",
		time.Since(started).Round(time.Second), s.Dials, s.Readys, s.PingsSent, s.PongsReceived, s.HeartbeatSevers, s.CloseStatuses)
	if s.Readys == 0 {
		return 1
	}
	return 0
}
