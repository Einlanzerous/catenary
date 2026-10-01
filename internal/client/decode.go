package client

import "github.com/magos/catenary/internal/wire"

// decodeFromServer is the ONE place this package decodes what Catenary sends
// it: a socket frame (name "ServerFrame") or a response body ("SyncResponse",
// "EnrollResponse", "RefreshResponse"). It decodes AS A CLIENT (CANT-177): a
// client-open enum value this wire version does not define — an error code, a
// resync reason, a conversation kind a later server adds — decodes to the
// sentinel "unknown" and is reported once, exactly as TypeScript and Dart
// decode it, and every other constraint is enforced as on the server. The
// strict decoder is the server's, and the server is the trust boundary; a
// client that refused what a newer server says would drop the frame, or wedge
// catch-up on the page, that carries it.
//
// Faults.StrictDecode puts the server's decoder back, and this is the one
// function that may name it: decode_guard_test.go holds every other non-test
// file in the package to the client side.
func (f Faults) decodeFromServer(name string, b []byte) (any, error) {
	if f.StrictDecode {
		if name == "ServerFrame" {
			return wire.DecodeServerFrame(b)
		}
		return wire.DecodeNamed(name, b)
	}
	if name == "ServerFrame" {
		return wire.DecodeServerFrameAsClient(b)
	}
	return wire.DecodeNamedAsClient(name, b)
}

// decodeResponse is decodeFromServer for a response body, typed. T is the
// VALUE type the generated decoder returns for name, e.g. wire.SyncResponse.
func decodeResponse[T any](f Faults, name string, b []byte) (T, error) {
	var zero T
	v, err := f.decodeFromServer(name, b)
	if err != nil {
		return zero, err
	}
	return v.(T), nil
}
