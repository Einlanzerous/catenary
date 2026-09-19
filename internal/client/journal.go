package client

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/magos/catenary/internal/wire"
)

// Credential is what a device holds to get back in: the pair POST /enroll
// minted, or the pair the latest POST /refresh rotated it into. It is durable
// state and lives in the Journal, never in Config (CANT-121) — see
// Journal.credential for why that distinction is the whole ticket.
type Credential struct {
	// DeviceID is the device the pair was minted for; the hello must name it.
	// A rotation never changes it.
	DeviceID wire.Uuid
	// AccessToken rides on the upgrade as `catenary.token.<token>` and on
	// /sync as a bearer.
	AccessToken     string
	AccessExpiresAt time.Time
	// RefreshToken is single-use: presenting it rotates the pair, and
	// presenting it a second time outside the grace window invalidates the
	// family and revokes the device (CANT-29).
	RefreshToken     string
	RefreshExpiresAt time.Time

	// AccessIssuedAt and ClockOffset are what CANT-31's record §1 has
	// persisted WITH the credential (CANT-124): when the server says it
	// issued this pair — the response's HTTP `Date` — and how far the device's
	// wall clock stood from the server's at that moment (server minus device).
	// Together they let a cold start read AccessExpiresAt correctly on a
	// device whose clock is wrong, without a round trip. Both are zero for a
	// pair that did not arrive over HTTP; see refreshThreshold and refreshDue
	// for what zero means to each. Set by Credential.WithServerDate.
	AccessIssuedAt time.Time
	ClockOffset    time.Duration
}

// wireTimestampLayout is the ONE shape internal/wire's TimestampPattern
// accepts: RFC 3339 with exactly three fractional digits and a literal Z. It
// is deliberately not time.RFC3339, which is a superset — an offset timestamp
// the generated decoder refuses would otherwise become durable state here.
// Restated rather than imported from internal/wireview, as soakrig's
// credentials.go does, so the client does not take on that package's
// dependency surface; TestTheTimestampLayoutIsTheWiresOwn pins it to the
// pattern itself.
const wireTimestampLayout = "2006-01-02T15:04:05.000Z"

// CredentialFromEnroll is the pair POST /enroll minted, as durable state. A
// parse failure here is a server that broke the contract, not an input to
// tolerate.
//
// THE BODY ALONE CARRIES NO CLOCK. A caller holding the HTTP response should
// use Enroll, or follow this with Credential.WithServerDate, so the pair
// knows its offset and its issue time; this form is for a caller that never
// had a response to read — a rig enrolling through the store.
func CredentialFromEnroll(e wire.EnrollResponse) (Credential, error) {
	accessExp, err := time.Parse(wireTimestampLayout, string(e.AccessExpiresAt))
	if err != nil {
		return Credential{}, fmt.Errorf("client: access_expires_at %q: %w", e.AccessExpiresAt, err)
	}
	refreshExp, err := time.Parse(wireTimestampLayout, string(e.RefreshExpiresAt))
	if err != nil {
		return Credential{}, fmt.Errorf("client: refresh_expires_at %q: %w", e.RefreshExpiresAt, err)
	}
	return Credential{
		DeviceID:    e.DeviceID,
		AccessToken: string(e.AccessToken), AccessExpiresAt: accessExp,
		RefreshToken: string(e.RefreshToken), RefreshExpiresAt: refreshExp,
	}, nil
}

var (
	// ErrNoCredential is New over a journal nobody enrolled.
	ErrNoCredential = errors.New("client: the journal holds no credential")
	// ErrCredentialHeld is Enroll over a journal that already holds one.
	ErrCredentialHeld = errors.New("client: the journal already holds a credential")
	// ErrCredentialDevice is a rotation naming a different device.
	ErrCredentialDevice = errors.New("client: a rotation cannot change the device")

	errCredentialIncomplete = errors.New("client: a credential needs a DeviceID, an AccessToken and a RefreshToken")
)

// Journal is the client's DURABLE state: the credential, the messages,
// conversations and users it holds, the cursor, and the evidence log of which
// messages it counted.
//
// It outlives a Client. A kill abandons the Client and keeps the Journal, and a
// restart is a new Client over the same Journal — which is what a process that
// died and came back finds on disk. That is the whole of what "durable" means
// in-process, and it is enough for the property under test: nothing a killed
// client did after its death reaches the Journal (Client.Kill takes this lock
// to die), and everything it did before is here.
//
// OBLIGATION 1 — PERSIST BEFORE RENDER — HOLDS BY CONSTRUCTION. A page's
// messages, conversations, users and cursor are written under one lock, and
// nothing is observable (Status, Holds, Snapshot, the Await wake-up) until the
// lock is released. There is no window in which a message is shown or counted
// and the cursor that covers it is not written. On real storage that is a
// transaction or an fsync, and proving it there is CANT-35's and CANT-42's.
type Journal struct {
	mu sync.Mutex

	// credential is the pair this device currently holds.
	//
	// HERE AND NOT IN Config, BECAUSE A REFRESH TOKEN IS SINGLE-USE (CANT-121).
	// A restart is a new Client over this Journal, built by a caller that
	// still has the enrollment response in hand. While the credential was a
	// Config field that caller passed the enrollment-time pair again, which
	// after one rotation is the SPENT pair — and presenting a spent refresh
	// token outside the grace window is what reuse detection exists to catch,
	// so the reference client would revoke its own device by restarting.
	// Config has no credential field at all now, so a restart cannot present
	// anything but what is written here.
	//
	// NOT CLEARED BY A WIPE. resetLocked is obligation 4's discard of the
	// message store; the credential is not part of what a stale cursor
	// invalidates, and CANT-31's record (§6) has nothing delete it at all —
	// not a wipe, and not a terminal state.
	//
	// UNDER ITS OWN LOCK, credMu, and not mu. The credential is read on the
	// way INTO a dial and a /sync, and mu is held across a whole page apply;
	// it is also not part of obligation 1's transaction — no message is
	// rendered against it. Lock order is mu then credMu, and only
	// Client.rotate takes both, for the Kill guard.
	credMu        sync.Mutex
	credential    Credential
	hasCredential bool

	// refreshing is the single-flight lock of CANT-31's record §2: at most one
	// refresh in flight PER CREDENTIAL, which means per store and not per
	// Client — two Clients over one Journal are two tabs over one origin's
	// storage. On real storage this is a Web Lock or a SQLite transaction. A
	// one-slot channel rather than a mutex, because it is held across an HTTP
	// round trip and a waiter must be able to give up when its context ends.
	refreshing chan struct{}

	cursor    int64
	hasCursor bool

	messages      map[wire.Uuid]wire.Message
	conversations map[wire.Uuid]wire.Conversation
	users         map[wire.Uuid]wire.User

	// counted is R1's journal: every message id in the order the client first
	// counted it. A message counted twice is a duplicate delivery the client
	// failed to recognise, and Compare reports it. Cleared by a wipe, because
	// a wipe discards the store the counts were counts of.
	counted []wire.Uuid

	// wipes is how many discard-and-bootstraps this journal has been through.
	// It doubles as the epoch a page request is issued in: a page requested
	// before a wipe is dropped rather than applied over it.
	wipes int
}

// NewJournal returns an empty journal: no cursor, nothing held.
func NewJournal() *Journal {
	j := &Journal{refreshing: make(chan struct{}, 1)}
	j.resetLocked()
	return j
}

// Enroll seeds a journal with the pair POST /enroll minted, once. A journal
// that already holds a credential refuses, and that refusal is the point: the
// held pair may be a rotation ahead of the one the caller has, and overwriting
// it with an older one is the spent-token restart this type exists to prevent.
func (j *Journal) Enroll(cred Credential) error {
	if cred.DeviceID == "" || cred.AccessToken == "" || cred.RefreshToken == "" {
		return errCredentialIncomplete
	}
	j.credMu.Lock()
	defer j.credMu.Unlock()
	if j.hasCredential {
		return ErrCredentialHeld
	}
	j.credential, j.hasCredential = cred, true
	return nil
}

// Rotate replaces the held pair with the one a refresh returned. The device
// stays the same; everything else is whatever the server said.
//
// It is on the Journal, not only the Client, because a credential is shared by
// every context that holds this store and not by one process — CANT-31's
// record §2. A Client's own rotations go through Client.rotate, which adds the
// Kill guard every other journal write has.
func (j *Journal) Rotate(next Credential) error {
	j.credMu.Lock()
	defer j.credMu.Unlock()
	switch {
	case !j.hasCredential:
		return ErrNoCredential
	case next.DeviceID != j.credential.DeviceID:
		return ErrCredentialDevice
	case next.AccessToken == "" || next.RefreshToken == "":
		// BOTH HALVES. A pair persisted without its refresh token connects
		// until the access token expires and then cannot refresh at all —
		// which is a re-enrollment, not a retry.
		return errCredentialIncomplete
	}
	j.credential = next
	return nil
}

// lockRefresh takes the single-flight lock, or gives up with ctx. The caller
// re-reads the credential AFTER this returns and before deciding anything:
// whoever held the lock may have rotated it.
func (j *Journal) lockRefresh(ctx context.Context) (release func(), err error) {
	select {
	case j.refreshing <- struct{}{}:
		return func() { <-j.refreshing }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Credential is the pair currently held, and whether there is one.
func (j *Journal) Credential() (Credential, bool) {
	j.credMu.Lock()
	defer j.credMu.Unlock()
	return j.credential, j.hasCredential
}

// resetLocked is obligation 4's wipe: messages, conversations, users and the
// cursor, all of them — and NOT the credential (see Journal.credential). The
// caller holds mu and bumps wipes.
func (j *Journal) resetLocked() {
	j.cursor, j.hasCursor = 0, false
	j.messages = map[wire.Uuid]wire.Message{}
	j.conversations = map[wire.Uuid]wire.Conversation{}
	j.users = map[wire.Uuid]wire.User{}
	j.counted = nil
}

// Snapshot is a consistent copy of a Journal.
type Snapshot struct {
	Cursor    int64
	HasCursor bool
	Wipes     int

	// Messages sorted by conversation, then seq — the thread as it renders.
	Messages      []wire.Message
	Conversations []wire.Conversation // by id
	Users         []wire.User         // by id

	// Counted is every message id in the order it was first counted since
	// the last wipe. A correct client counts each id exactly once.
	Counted []wire.Uuid
}

// Snapshot copies the journal under its lock.
func (j *Journal) Snapshot() Snapshot {
	j.mu.Lock()
	defer j.mu.Unlock()
	s := Snapshot{
		Cursor: j.cursor, HasCursor: j.hasCursor, Wipes: j.wipes,
		Counted: append([]wire.Uuid(nil), j.counted...),
	}
	for _, m := range j.messages {
		s.Messages = append(s.Messages, m)
	}
	sort.Slice(s.Messages, func(a, b int) bool {
		if s.Messages[a].ConversationID != s.Messages[b].ConversationID {
			return s.Messages[a].ConversationID < s.Messages[b].ConversationID
		}
		if s.Messages[a].Seq != s.Messages[b].Seq {
			return s.Messages[a].Seq < s.Messages[b].Seq
		}
		return s.Messages[a].ID < s.Messages[b].ID
	})
	for _, c := range j.conversations {
		s.Conversations = append(s.Conversations, c)
	}
	sort.Slice(s.Conversations, func(a, b int) bool { return s.Conversations[a].ID < s.Conversations[b].ID })
	for _, u := range j.users {
		s.Users = append(s.Users, u)
	}
	sort.Slice(s.Users, func(a, b int) bool { return s.Users[a].ID < s.Users[b].ID })
	return s
}
