package wireview

// CANT-117 — one device row becomes one wire.Device.
//
// ITS OWN FILE rather than another entry in sync.go, because it belongs to
// neither of that file's two jobs: it is not part of a /sync page and it is not
// part of a message. The mapping is small enough that the reason it exists
// somewhere is worth more than the six lines it saves.

import (
	"github.com/magos/catenary/internal/store"
	"github.com/magos/catenary/internal/wire"
)

// Device maps one row of the caller's own device list.
//
// ABSENT RATHER THAN ZERO for revoked_at, on the same rule User.initials
// follows: the wire's own description says absent means live, so a live device
// must omit the field rather than carry a zero timestamp. A zero time formatted
// through TimeLayout would be a real-looking date in the year 1, which a client
// would render.
func Device(d store.DeviceRow) wire.Device {
	out := wire.Device{
		ID:        wire.Uuid(d.ID.String()),
		Name:      d.Name,
		CreatedAt: wire.Timestamp(d.CreatedAt.UTC().Format(TimeLayout)),
	}
	if d.RevokedAt != nil {
		at := wire.Timestamp(d.RevokedAt.UTC().Format(TimeLayout))
		out.RevokedAt = &at
	}
	return out
}

// DeviceList maps a whole page.
//
// NON-NIL EVEN WHEN EMPTY, and that is the one thing this function is really
// for. `devices` is required on the wire, and a nil slice encodes as `null` in
// Go while an empty one encodes as `[]` — so a person with no devices, or a bot
// which never enrols one, would otherwise be served a page every generated
// decoder refuses. The `device_list_response_empty` vector is the oracle.
func DeviceList(rows []store.DeviceRow) wire.DeviceListResponse {
	out := wire.DeviceListResponse{Devices: make([]wire.Device, 0, len(rows))}
	for _, d := range rows {
		out.Devices = append(out.Devices, Device(d))
	}
	return out
}
