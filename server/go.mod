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
	github.com/getkin/kin-openapi v0.135.0
	golang.org/x/oauth2 v0.36.0
)

require (
	cloud.google.com/go/compute/metadata v0.3.0 // indirect
	github.com/go-openapi/jsonpointer v0.21.0 // indirect
	github.com/go-openapi/swag v0.23.0 // indirect
	github.com/josharian/intern v1.0.0 // indirect
	github.com/mailru/easyjson v0.7.7 // indirect
	github.com/mohae/deepcopy v0.0.0-20170929034955-c48cc78d4826 // indirect
	github.com/oasdiff/yaml v0.0.9 // indirect
	github.com/oasdiff/yaml3 v0.0.9 // indirect
	github.com/perimeterx/marshmallow v1.1.5 // indirect
	github.com/woodsbury/decimal128 v1.3.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)
