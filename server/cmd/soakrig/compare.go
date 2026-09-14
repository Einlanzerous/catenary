package main

import (
	"context"

	"github.com/magos/catenary/internal/client"
)

// compareAll runs client.Compare for every client that got as far as a
// socket, against that viewer's own slice of the server's committed log, and
// writes the outcome into rep.Clients — indexed so a client that was skipped
// or errored stays visible rather than missing from a shorter list.
//
// ComparisonsRun is incremented ONLY on success: a comparison that errored,
// or one CANT-27's counter-proof tests deliberately skip, is exactly the "a
// comparison that never ran" case CLAUDE.md and the ticket both name as a
// harness failure — classify() reads this count against rep.N, not against
// len(rep.Clients).
func (h *harness) compareAll(ctx context.Context, clients []*soakClient, rep *Report) {
	for _, sc := range clients {
		cr := ClientReport{Index: sc.index, DeviceName: sc.name, Provisioned: true}

		if h.cfg.debugSkipCompareFor[sc.index] {
			cr.CompareError = "planted: the comparison was skipped for this client (test)"
			h.harnessError("comparison skipped for client %d (%s): planted (test)", sc.index, sc.name)
			rep.Clients[sc.index] = cr
			continue
		}

		rows, err := h.serverLogFor(ctx, sc.userID)
		if err != nil {
			cr.CompareError = err.Error()
			h.harnessError("fetch the server log for client %d (%s): %v", sc.index, sc.name, err)
			rep.Clients[sc.index] = cr
			continue
		}

		cr.Compare = client.Compare(rows, sc.c.Snapshot())
		cr.Compared = true
		rep.Clients[sc.index] = cr
		rep.ComparisonsRun++
	}
}
