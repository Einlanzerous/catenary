// CANT-263 — ensure the self conversation, once, after the first catch-up.
//
// CANT-254 ruling 3 → B: each client calls the idempotent
// `POST /conversations/self` once its first catch-up has completed and the
// journal holds no self conversation. That is how an account that existed
// before `self` did gets a Notes row without enrollment changing, and why
// nothing here is a button: a person did not ask for it, so it must never go
// wrong where they can see it. The web's twin is `web/src/ensure-self.ts`.
//
//   * AT MOST ONCE PER SESSION. The first completed catch-up is the only one
//     that acts; a failure is not retried inside the launch (the next launch
//     asks again).
//   * ZERO CALLS WHEN ONE IS HELD. The journal is read at the moment of the
//     decision.
//   * NEVER A BANNER. A 404, a 403 (a bot), an offline failure and a thrown
//     error all end here, quietly; nothing a person reads is written.
//
// Pure of the store and the transport so a test drives it with fakes.

/// The once-per-session decision. Feed it every transport status.
final class EnsureSelfOnce {
  EnsureSelfOnce({required this.holdsSelf, required this.request, required this.afterCreated});

  /// Whether the journal, as projected, already holds a self conversation.
  final bool Function() holdsSelf;

  /// `POST /conversations/self`: true when the server answered with the
  /// conversation. May return false or throw; neither is surfaced.
  final Future<bool> Function() request;

  /// Called after a created/found answer, to fetch the row through `/sync`.
  final void Function() afterCreated;

  var _decided = false;

  /// [ready]: a socket is open and the server's `ready` has arrived.
  /// [caughtUp]: no catch-up trigger is outstanding.
  void observe({required bool ready, required bool caughtUp}) {
    if (_decided || !ready || !caughtUp) return;
    // The first completed catch-up decides, whichever way it goes.
    _decided = true;
    if (holdsSelf()) return;
    () async {
      try {
        if (await request()) afterCreated();
      } catch (_) {
        // Quiet by design.
      }
    }();
  }
}
