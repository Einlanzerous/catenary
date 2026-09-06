module github.com/magos/catenary/server

go 1.26

// CANT-82 / CANT-18 Ruling 3. The generated wire package lives at
// `github.com/magos/catenary/internal/wire`, in the service module, because
// Go's internal rule is applied to the IMPORT PATH: only a package under
// `github.com/magos/catenary/...` may import it, and no module wiring changes
// that. So the dependency runs this way round — the spike binaries and the
// conformance runner point at the artefact, not the service at a module
// carrying `coder/websocket` and `oauth2` for spikes it does not use.
//
// `replace` by directory, so nothing here needs a checksum or a published
// version: this module is never fetched, only built in place.
require github.com/magos/catenary v0.0.0

replace github.com/magos/catenary => ../

require (
	github.com/coder/websocket v1.8.15
	golang.org/x/oauth2 v0.36.0
)

require cloud.google.com/go/compute/metadata v0.3.0 // indirect
