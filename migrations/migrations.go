// Package migrations embeds Catenary's SQL migrations so the in-process
// migrator (internal/store) can apply them on boot. No external migration
// tool, on the construct-server house pattern.
package migrations

import "embed"

// FS holds every *.up.sql / *.down.sql migration in this directory.
//
//go:embed *.sql
var FS embed.FS
