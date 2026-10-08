// The store: the journal and the outbox, projected into what the rail, the
// thread and the banner read. The twin of the session half of web/src/store.ts
// and of `connectionInfo` in web/src/transport/status.ts.
//
// EVERY RECORD IN HERE CAME FROM A SERVER, OR FROM THIS DEVICE'S OWN OUTBOX,
// and the two never mix. The journal is `catenary_client`'s, projected by its
// `project` and `projectApplied`: `project` over what the file already holds,
// then `projectApplied` folded over every `Applied` after it. What you have
// written and the server has not stored is the outbox's, and is laid after the
// log as a tail, not placed in it — it has no `seq` of its own to be placed by.
//
// TWO VOCABULARIES, ONE ENUM (store/status.dart). `queued`, `sending` and
// `failed` come only from the outbox: they are a message's relationship to its
// outbox, which the server has no opinion on (Invariant 3). `delivered` and
// `read` come only from the wire's `DeliveryState`. `sent` comes from the
// wire, or from the outbox's ack until the record arrives: an ack says the
// server stored the message, which is all `sent` claims (CANT-220 ruling 4,
// which supersedes CANT-200's narrower wording on this point). [deliveryStatus]
// and [outboxStatus] are the only two places a `MessageStatus` is made here.
//
// RULING 2 → THE UI ISOLATE. This is a `ChangeNotifier` the widgets listen to,
// on the isolate they run on. Nothing outside lib/store/ imports
// `package:catenary_client`, and a store method that sends, reads, types,
// retries or discards returns a `Future`, so a later move to a background
// isolate changes this file and no widget. test/store_boundary_test.dart holds
// both. The controls' commands are the four below `start`: [send], [retry],
// [discard] and [retryNow]. Each is a `Future`, and none returns before the
// outbox or the transport has taken the call. `read` and `typing` are not here:
// no widget sends either yet.

import 'dart:async';
import 'dart:math' as math;

import 'package:catenary_client/catenary_client.dart';
import 'package:catenary_wire/catenary_wire.dart' as wire;
import 'package:flutter/foundation.dart';

import 'address.dart';
import 'connection.dart';
import 'conversation.dart';
import 'enrollment.dart';
import 'session.dart';
import 'start.dart';
import 'status.dart';

/// What the banner reads, from what the transport knows. Pure: [now]
/// (wall-clock ms) and [online] are passed in. Follows `connectionInfo` in
/// web/src/transport/status.ts branch for branch.
///
/// TERMINAL FIRST, because neither terminal ends on a network change (CANT-31
/// §6): an offline device that is also terminal is told it is terminal. Then
/// `offline` when the device says so — [online] is false only once the network
/// has reported it, and null, which is not offline, until it has reported
/// anything; then `live` or `resyncing` for a ready session; and
/// `reconnecting` for everything between dials.
///
/// THE JOURNAL'S WRITE FAILURE RIDES EVERY BRANCH, by the error's name: a
/// journal that could not write is a fact about this device's storage, not
/// about the connection, and it holds until a write lands again. The
/// reference's other two extras, `refreshHold` and `tokenRefused`, are not
/// carried: no banner in either client draws them.
ConnectionView connectionInfo(TransportStatus status, {required num now, bool? online}) {
  final journalError = status.journalError?.name;
  if (status.terminal.kind != TerminalKind.none) {
    return ConnectionView(
      kind: ConnectionKind.terminal,
      terminal: status.terminal.kind == TerminalKind.protocol ? TerminalCause.protocol : TerminalCause.credential,
      journalError: journalError,
    );
  }
  if (online == false) return ConnectionView(kind: ConnectionKind.offline, journalError: journalError);
  if (status.ready) {
    if (status.caughtUp) return ConnectionView(kind: ConnectionKind.live, journalError: journalError);
    // A count toward `head_seq`, never a spinner (CANT-37), but only once
    // there is a real number to show: `headSeqTotal` is 0 until the first page
    // has named a conversation, and a 0 / 0 would itself be a claim. `synced`
    // is clamped to `total` because a live frame for a held conversation can
    // land between pages without that conversation's `head_seq` catching up
    // with it.
    if (status.headSeqTotal <= 0) return ConnectionView(kind: ConnectionKind.resyncing, journalError: journalError);
    return ConnectionView(
      kind: ConnectionKind.resyncing,
      synced: math.min(status.messages, status.headSeqTotal),
      total: status.headSeqTotal,
      journalError: journalError,
    );
  }
  final nextDialAt = status.nextDialAt;
  return ConnectionView(
    kind: ConnectionKind.reconnecting,
    attempt: math.max(1, status.attempt),
    retryIn: Duration(seconds: nextDialAt == null ? 0 : math.max(0, ((nextDialAt - now) / 1000).ceil())),
    journalError: journalError,
  );
}

/// A journal message's status: the wire's `DeliveryState`, and never one of
/// the outbox's three. A state this build does not know (the wire's `unknown`
/// sentinel) is shown as `sent`, the least a record the server holds can be.
MessageStatus deliveryStatus(wire.DeliveryState state) => switch (state) {
      wire.DeliveryState.delivered => MessageStatus.delivered,
      wire.DeliveryState.read => MessageStatus.read,
      wire.DeliveryState.sent || wire.DeliveryState.unknown => MessageStatus.sent,
    };

/// An outbox entry's status, from the outbox's own derived state. Its `sent`
/// is an entry the server has acked and whose record has not arrived yet: the
/// ack says the message is stored, which is all `sent` says.
MessageStatus outboxStatus(OutboxState state) => switch (state) {
      OutboxState.queued => MessageStatus.queued,
      OutboxState.sending => MessageStatus.sending,
      OutboxState.failed => MessageStatus.failed,
      OutboxState.sent => MessageStatus.sent,
    };

/// The rooms still behind their `head_seq`: a conversation holds fewer
/// messages than its head says exist. Derived, never stored, from the same two
/// facts `synced / total` is (`roomsPendingIn` in web/src/store.ts).
int roomsPending(Projection projection) {
  final held = <String, int>{};
  for (final m in projection.messages) {
    held[m.conversationId] = (held[m.conversationId] ?? 0) + 1;
  }
  return projection.conversations.where((c) => (held[c.id] ?? 0) < c.headSeq).length;
}

DateTime _at(String timestamp) => DateTime.parse(timestamp).toLocal();

/// A transcript's state as the thread draws it. One arm per wire value, and
/// the wire's `unknown` sentinel stays unknown: it is not folded into
/// `pending`, which would say a job is running.
TranscriptStatus transcriptStatus(wire.TranscriptState state) => switch (state) {
      wire.TranscriptState.pending => TranscriptStatus.pending,
      wire.TranscriptState.ready => TranscriptStatus.ready,
      wire.TranscriptState.failed => TranscriptStatus.failed,
      wire.TranscriptState.unknown => TranscriptStatus.unknown,
    };

/// A zero is no estimate, as in the web client, which hides a falsy one.
Duration? _eta(int? seconds) => seconds == null || seconds == 0 ? null : Duration(seconds: seconds);

/// One journal message, as the thread shows it. [me] is the enrolled account.
ThreadMessage journalMessage(wire.Message m, {required String me, required Map<String, wire.User> users}) {
  final attachments = m.attachments ?? const <wire.Attachment>[];
  final voice = attachments.whereType<wire.VoiceAttachment>().firstOrNull;
  final image = attachments.whereType<wire.ImageAttachment>().firstOrNull;
  return ThreadMessage(
    id: m.id,
    seq: m.seq,
    authorId: m.authorId,
    authorName: users[m.authorId]?.name ?? '',
    at: _at(m.at),
    mine: m.authorId == me,
    text: m.text,
    // The peaks are the server's (Invariant 3): no client derives a waveform.
    voice: voice == null
        ? null
        : VoiceNote(
            duration: Duration(milliseconds: voice.durationMs),
            peaks: voice.peaks,
            transcript: voice.transcript.state == wire.TranscriptState.ready ? voice.transcript.text : null,
            eta: voice.transcript.state == wire.TranscriptState.pending ? _eta(voice.transcript.etaSec) : null,
            status: transcriptStatus(voice.transcript.state),
          ),
    image: image == null ? null : ImageAttachment(filename: image.filename, width: image.width, height: image.height),
    status: deliveryStatus(m.state),
    readBy: m.readBy,
  );
}

/// One unsettled outbox entry, as the thread shows it. [seq] is where the row
/// sits: the ack's once there is one, and otherwise a place after the log that
/// the caller picks — an unacked entry has no seq, and this is not one the
/// server gave.
ThreadMessage outboxMessage(OutboxItem item, {required int seq, required Map<String, wire.User> users}) {
  final o = projectOutbox(item);
  final voice = item.entry.attachments.where((a) => a.kind == 'voice' && a.durationMs != null).firstOrNull;
  return ThreadMessage(
    id: o.clientId,
    seq: seq,
    authorId: o.authorId,
    authorName: users[o.authorId]?.name ?? '',
    at: _at(o.at),
    mine: true,
    text: o.text,
    // A note that has not left this device has no peaks: those are the
    // server's to compute, so the row shows its length and no bars.
    voice: voice == null ? null : VoiceNote(duration: Duration(milliseconds: voice.durationMs!), peaks: const []),
    status: outboxStatus(o.state),
    failure: o.error,
    retrying: o.retrying,
  );
}

/// The rail and every thread, from the journal's projection and the outbox's
/// view. Rooms and directs come back in one list, most recent first; the rail
/// splits them by kind and keeps this order.
///
/// A thread is the journal's records and the outbox's acked entries by `seq`,
/// then the unacked entries as a tail, in the outbox's own order. An entry
/// whose record the journal already holds is not shown twice: a record settles
/// its entry a moment after it lands, and the record is the one that stays.
///
/// [typing] is the server's `typing` frames by conversation: user ids in the
/// order each started, which the naming rule depends on. [secure] is whether
/// the stored address is `https` (`addressIsSecure`).
List<ConversationView> conversationViews({
  required Projection projection,
  required List<OutboxItem> outbox,
  required String me,
  required bool secure,
  Map<String, List<String>> typing = const {},
}) {
  final users = projection.users;
  final records = <String, List<wire.Message>>{};
  final heldClientIds = <String>{};
  final heldIds = <String>{};
  for (final m in projection.messages) {
    (records[m.conversationId] ??= []).add(m);
    heldIds.add(m.id);
    final clientId = m.clientId;
    if (clientId != null) heldClientIds.add(clientId);
  }
  final acked = <String, List<OutboxItem>>{};
  final tails = <String, List<OutboxItem>>{};
  for (final item in outbox) {
    final ack = item.ack;
    if (heldClientIds.contains(item.entry.clientId) || (ack != null && heldIds.contains(ack.messageId))) continue;
    // Placed by the SERVER's ack once there is one, never by the entry's own
    // conversation (outbox/projection.dart).
    if (ack != null) {
      (acked[ack.conversationId] ??= []).add(item);
    } else {
      (tails[item.entry.conversationId] ??= []).add(item);
    }
  }

  final views = <ConversationView>[];
  for (final c in projection.conversations) {
    final log = <ThreadMessage>[
      for (final m in records[c.id] ?? const <wire.Message>[]) journalMessage(m, me: me, users: users),
      for (final item in acked[c.id] ?? const <OutboxItem>[]) outboxMessage(item, seq: item.ack!.seq, users: users),
    ]..sort((a, b) => a.seq.compareTo(b.seq));
    var next = math.max(c.headSeq, log.isEmpty ? 0 : log.last.seq);
    final other = c.kind == wire.ConversationKind.direct ? users[c.otherMemberId] : null;
    views.add(ConversationView(
      id: c.id,
      // A kind this build does not know is listed with the rooms: `group` is
      // any number of members, which is the claim that cannot be wrong.
      kind: c.kind == wire.ConversationKind.direct ? ConversationKind.direct : ConversationKind.group,
      // A direct is titled by the other member's live name, and by the
      // record's own only until that `User` is held (CANT-141).
      name: other?.name ?? c.name,
      memberCount: c.memberCount,
      messages: [
        ...log,
        for (final item in tails[c.id] ?? const <OutboxItem>[]) outboxMessage(item, seq: ++next, users: users),
      ],
      firstUnreadSeq: c.firstUnreadSeq,
      muted: c.muted ?? false,
      typing: [
        for (final id in typing[c.id] ?? const <String>[])
          if (users[id] != null) users[id]!.name,
      ],
      secure: secure,
      unacked: tails[c.id]?.length ?? 0,
    ));
  }
  // Most recent first, by what the rail says each last held (`last`: the newer
  // of the log's end and the tail's); a conversation with nothing in it sorts
  // last. Equal stamps keep the projection's order, which is by id.
  final epoch = DateTime.fromMillisecondsSinceEpoch(0);
  final ordered = [for (var i = 0; i < views.length; i++) (i, views[i])]..sort((a, b) {
      final byTime = (b.$2.last?.at ?? epoch).compareTo(a.$2.last?.at ?? epoch);
      return byTime != 0 ? byTime : a.$1.compareTo(b.$1);
    });
  return List.unmodifiable([for (final (_, v) in ordered) v]);
}

/// Why the last [AppStore.start] ended in neither a session nor "not
/// enrolled": something this device keeps would not open (CANT-222).
@immutable
final class StartFailure {
  const StartFailure(this.name, {this.wipeOwed = false});

  /// The error's type name, as the transport names a failed journal write.
  /// Never its message: a `SqliteException` prints its statement's parameters.
  final String name;

  /// A re-enrollment stored its credential and the journal the previous one
  /// left has not been wiped. No session starts until it has been.
  final bool wipeOwed;
}

/// Wipes the journal in a directory. The default is [wipeJournalFile]; a test
/// passes one that throws.
typedef JournalWipe = Future<void> Function(String directory);

/// Opens `catenary.db`'s journal, wipes it and closes it: the journal's
/// tables, and never the credential beside them or the outbox's file.
Future<void> wipeJournalFile(String directory) async {
  final journal = SqliteJournal.open('$directory/$journalFileName');
  try {
    await journal.wipe();
  } finally {
    journal.close();
  }
}

/// The store the widgets listen to. Built over the platform's seams
/// (store/platform.dart builds the shipped ones), started once with [start],
/// and ended with [dispose].
///
/// Before [start] has found an enrolled device, [enrolled] is false and
/// everything else is empty: there is no fixture corpus behind it.
final class AppStore extends ChangeNotifier {
  /// [now] is wall-clock ms, which the banner's countdown is measured against.
  AppStore(this._seams, [this._now = systemClock, this._wipe = wipeJournalFile]);

  final SessionSeams _seams;
  final Clock _now;
  final JournalWipe _wipe;

  Session? _session;
  Projection _projection = emptyProjection;
  List<OutboxItem> _outbox = const [];
  final _typing = <String, List<String>>{};
  bool? _online;
  List<ConversationView> _conversations = const [];
  ConnectionView _connection = const ConnectionView(kind: ConnectionKind.reconnecting);
  var _offs = <void Function()>[];
  Timer? _ticker;
  var _disposed = false;

  StartFailure? _startFailure;

  /// Whether this device holds an address and a credential, and a session is
  /// running on them. False is the caller's cue for the enrollment screen,
  /// but only once [startFailure] has been asked and is null.
  bool get enrolled => _session != null;

  /// Set when the last [start] could not open what this device keeps. ASK
  /// THIS BEFORE [enrolled]: a device whose stores will not open has no
  /// session either, and showing it the enrollment form would spend a token
  /// against files that still cannot be opened.
  StartFailure? get startFailure => _startFailure;

  /// Rooms and directs, most recent first.
  List<ConversationView> get conversations => _conversations;

  /// The conversation with this id as it stands now, or null once the journal
  /// no longer holds it.
  ConversationView? conversation(String id) => _conversations.where((c) => c.id == id).firstOrNull;

  ConnectionView get connection => _connection;

  /// The enrolled person's two letters for the rail's header: the server's
  /// when it sent them, derived from the name otherwise, and empty until
  /// their own `User` record is held.
  String get myInitials {
    final user = _projection.users[_session?.credential.userId];
    return user == null ? '' : user.initials ?? initials(user.name);
  }

  /// Reads the address and the credential and, with both, starts a session and
  /// projects it. Completes true when one is running and false otherwise:
  /// when this device is not enrolled, in which case nothing was constructed,
  /// or when something it keeps would not open, in which case [startFailure]
  /// says so. It does not throw for either, and deletes nothing. Calling it
  /// again ends the running session first: the restart a re-enrollment needs,
  /// and the second attempt TRY AGAIN makes.
  Future<bool> start() async {
    _detach();
    final replacing = _session;
    _session = null;
    final SessionStart started;
    try {
      // A wipe a re-enrollment could not make is paid first, and no session
      // starts until it is: the new credential never runs over the journal
      // the previous one left.
      if (_startFailure?.wipeOwed ?? false) {
        replacing?.end();
        try {
          await _wipe(_seams.directory);
        } on Object catch (e) {
          if (_disposed) return false;
          _startFailure = StartFailure('${e.runtimeType}', wipeOwed: true);
          notifyListeners();
          return false;
        }
      }
      started = await startSession(_seams, replacing: replacing, onOutbox: _onOutbox);
    } on Object catch (e) {
      // `startSession` closed whatever it had opened before it threw.
      if (_disposed) return false;
      _empty();
      _startFailure = StartFailure('${e.runtimeType}');
      notifyListeners();
      return false;
    }
    final session = switch (started) {
      NotEnrolled() => null,
      SessionRunning(:final session) => session,
    };
    if (_disposed) {
      session?.end();
      return false;
    }
    _startFailure = null;
    if (session == null) {
      _empty();
      notifyListeners();
      return false;
    }
    _typing.clear();
    _session = session;
    final transport = session.transport;
    _offs = [
      transport.onApply((applied) {
        _projection = projectApplied(_projection, applied);
        _show();
      }),
      transport.subscribe((_) => _showStatus()),
      // The server's list, verbatim: the order is the order they started.
      transport.onTyping((f) {
        _typing[f.conversationId] = List.of(f.userIds);
        _show();
      }),
      _seams.lifecycle.subscribe((e) {
        if (e != LifecycleEvent.online && e != LifecycleEvent.offline) return;
        _online = e == LifecycleEvent.online;
        _showStatus();
      }),
    ];
    // Subscribed first, read second, so nothing applied in between is missed.
    _projection = project(transport.snapshot());
    _outbox = session.outbox.view();
    // The countdown the banner reads moves between status changes, so the
    // status is read again once a second while a dial is being waited for.
    // The status only: no thread is rebuilt for a second going by.
    _ticker = Timer.periodic(const Duration(seconds: 1), (_) {
      if (_connection.kind == ConnectionKind.reconnecting) _showStatus();
    });
    _show();
    return true;
  }

  /// The composer's send: the text becomes an outbox entry in [conversationId],
  /// whether or not a session is ready (`Outbox.compose`). Completes once the
  /// entry is stored, not once the server has acked it, and the thread shows
  /// it as queued until the outbox says otherwise. Blank text is not a
  /// message. Throws when the device is not enrolled.
  Future<void> send(String conversationId, String text) async {
    final trimmed = text.trim();
    if (trimmed.isEmpty) return;
    await _running.outbox.compose(OutboxDraft(conversationId: conversationId, text: trimmed));
  }

  /// RETRY on a failed message: `Outbox.retry`, under the same `client_id`.
  /// The message's `ThreadMessage.id` is that id for an outbox row.
  Future<void> retry(String clientId) async => _running.outbox.retry(clientId);

  /// DELETE on a failed message: `Outbox.discard`, which removes this device's
  /// copy and nothing the server holds. True when an entry was removed.
  Future<bool> discard(String clientId) async => _running.outbox.discard(clientId);

  /// The banner's RETRY: `Transport.retryNow`, which dials now instead of
  /// waiting out the backoff. The banner follows the transport's own status.
  Future<void> retryNow() async => _running.transport.retryNow();

  /// The roster a person picks from (`GET /users`): active persons other than
  /// you. Null when it could not be had, which the picker says and offers to
  /// try again; an empty list is a roster with nobody else in it.
  Future<List<RosterPerson>?> roster() async {
    final r = await _running.conversations.roster();
    return switch (r) {
      Started(:final value) => [
          for (final e in value) RosterPerson(id: e.id, name: e.name, handle: e.handle, initials: e.initials),
        ],
      _ => null,
    };
  }

  /// A direct with [handle], found or made (`POST /conversations/direct`).
  Future<StartOutcome> startDirect(String handle) async => _started(await _running.conversations.startDirect(handle));

  /// A group of you and [handles] (`POST /conversations`). [requestId] is the
  /// form's own, so asking again after an unreachable answer is a replay.
  Future<StartOutcome> startGroup(String name, List<String> handles, {required String requestId}) async =>
      _started(await _running.conversations.createGroup(name.trim(), handles, requestId: requestId));

  /// THE CONVERSATION COMES BACK THROUGH THE JOURNAL, NOT AROUND IT. The
  /// response names the id; a catch-up is triggered so `/sync` serves the row
  /// (the server draws its metadata marker in the creating transaction), and
  /// the id is waited for there, bounded, so the caller can open what the
  /// journal holds rather than a record this device invented.
  Future<StartOutcome> _started(StartResult<dynamic> r) async {
    switch (r) {
      case Started(:final value):
        final id = (value as wire.Conversation).id;
        final session = _session;
        if (session == null) return const StartUnreachableNow();
        if (conversation(id) == null) {
          session.transport.catchUp();
          await _held(id);
        }
        return StartDone(id, held: conversation(id) != null);
      case StartRefused(:final code):
        return StartRefusedBy(code);
      case StartUnreachable():
        return const StartUnreachableNow();
    }
  }

  /// Completes when [id] is in [conversations], or after [startWait].
  Future<void> _held(String id) {
    final done = Completer<void>();
    late final VoidCallback listener;
    final timer = Timer(startWait, () {
      if (!done.isCompleted) done.complete();
    });
    listener = () {
      if (conversation(id) != null && !done.isCompleted) done.complete();
    };
    addListener(listener);
    return done.future.whenComplete(() {
      timer.cancel();
      removeListener(listener);
    });
  }

  /// How long a start waits for the journal to hold what the server made.
  @visibleForTesting
  Duration startWait = const Duration(seconds: 8);

  Session get _running => _session ?? (throw StateError('store: this device is not enrolled'));

  /// The origin this device's credential belongs to, or null before it is
  /// enrolled. The re-enrollment screen shows it and does not edit it.
  String? get address => _session?.address;

  /// A first enrollment: spends [token] at [typedAddress] (store/enrollment.dart)
  /// and on success starts a session on what came back. [release] is
  /// `kReleaseMode`, passed in so a test can name either.
  Future<EnrollOutcome> enroll({
    required String typedAddress,
    required String token,
    required String deviceName,
    required bool release,
  }) async {
    // Opened inside the catch: a file that will not open is found out here,
    // before any request, and is a text on the form and not a throw.
    final SqliteCredentialStore store;
    try {
      store = openCredentialStore(_seams.directory);
    } on Object {
      return const EnrollFailed(enrollStorageText);
    }
    final EnrollOutcome outcome;
    try {
      outcome = await enrollDeviceAt(
        directory: _seams.directory,
        store: store,
        typedAddress: typedAddress,
        token: token,
        deviceName: deviceName,
        release: release,
        fetch: _seams.fetch,
      );
    } finally {
      store.close();
    }
    // RULING 2 → THE SAME SCREEN AS AT LAUNCH. A session that cannot start
    // on the credential just stored is `startFailure`, which `start` sets
    // and does not throw: the outcome is still `Enrolled`, and the shell
    // draws the failed-start screen in place of the form.
    if (outcome is Enrolled) await start();
    return outcome;
  }

  /// RE-ENROLL on a credential terminal, as the web does it (ruling 3):
  /// `reenrollCredential` replaces the pair, the journal is wiped (a cursor is
  /// a position in the log as the PREVIOUS credential's account could see it,
  /// and the new pair may be another person's), and a new session starts. The
  /// outbox is not touched: its entries are keyed by account. The address is
  /// the one already stored.
  Future<EnrollOutcome> reenroll({required String token, required String deviceName, required bool release}) async {
    final session = _session;
    if (session == null) return const EnrollFailed('This device is not enrolled.');
    final outcome = await enrollDeviceAt(
      directory: _seams.directory,
      store: session.credentials,
      typedAddress: session.address,
      token: token,
      deviceName: deviceName,
      release: release,
      fetch: _seams.fetch,
    );
    if (outcome is Enrolled) {
      // The old session ends first, so no late frame under the old credential
      // can land after the wipe, and it is let go of: until a new one runs
      // this store has no session, and says so.
      _detach();
      session.end();
      _session = null;
      _empty();
      try {
        await _wipe(_seams.directory);
      } on Object catch (e) {
        // The pair is the new one and the journal is the old one. The wipe
        // is owed, and `start` pays it before it starts anything.
        if (!_disposed) {
          _startFailure = StartFailure('${e.runtimeType}', wipeOwed: true);
          notifyListeners();
        }
        return outcome;
      }
      await start();
    }
    return outcome;
  }

  /// What the widgets read when there is no session.
  void _empty() {
    _typing.clear();
    _projection = emptyProjection;
    _outbox = const [];
    _conversations = const [];
    _connection = const ConnectionView(kind: ConnectionKind.reconnecting);
  }

  void _onOutbox(List<OutboxItem> items) {
    _outbox = items;
    if (_session != null) _show();
  }

  /// Rebuilds everything the widgets read, and tells them: for a change to the
  /// journal, the outbox or who is typing.
  void _show() {
    final session = _session;
    if (session == null || _disposed) return;
    _conversations = conversationViews(
      projection: _projection,
      outbox: _outbox,
      me: session.credential.userId,
      secure: addressIsSecure(session.address),
      typing: _typing,
    );
    _showStatus();
  }

  /// Re-derives the banner, and tells the widgets: for a change to the
  /// transport's status or the network, and for the countdown. The threads
  /// are left as they are, unless a session ending has emptied a typing list.
  void _showStatus() {
    final session = _session;
    if (session == null || _disposed) return;
    final status = session.transport.status();
    // A typing list is a fact about a live session; one that has ended says
    // nothing about who is typing now.
    if (!status.ready && _typing.isNotEmpty) {
      _typing.clear();
      _show();
      return;
    }
    final info = connectionInfo(status, now: _now(), online: _online);
    _connection = ConnectionView(
      kind: info.kind,
      attempt: info.attempt,
      retryIn: info.retryIn,
      synced: info.synced,
      total: info.total,
      // Beside a real total only: "0 rooms pending" before a page has landed
      // would be the same claim a 0 / 0 is.
      roomsPending: info.kind == ConnectionKind.resyncing && info.total != null ? roomsPending(_projection) : null,
      // What is waiting to go: entries the outbox will still send. A failed
      // one is not waiting; it is asking to be retried or deleted.
      queued: info.kind == ConnectionKind.offline
          ? _outbox.where((i) => i.state == OutboxState.queued || i.state == OutboxState.sending).length
          : null,
      terminal: info.terminal,
      journalError: info.journalError,
    );
    notifyListeners();
  }

  void _detach() {
    _ticker?.cancel();
    _ticker = null;
    for (final off in _offs) {
      off();
    }
    _offs = [];
  }

  /// Stops listening and ends the session: the transport stops, the outbox
  /// detaches and every file closes.
  @override
  void dispose() {
    _disposed = true;
    _detach();
    _session?.end();
    _session = null;
    super.dispose();
  }
}
