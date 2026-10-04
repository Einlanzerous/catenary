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
// TWO VOCABULARIES, ONE ENUM (store/status.dart). A journal message's status
// comes from the wire's `DeliveryState` and is `sent`, `delivered` or `read`.
// `queued`, `sending` and `failed` come from the outbox's own state and from
// nowhere else: they are a message's relationship to its outbox, which the
// server has no opinion on (Invariant 3). [deliveryStatus] and [outboxStatus]
// are the only two places a `MessageStatus` is made here.
//
// RULING 2 → THE UI ISOLATE. This is a `ChangeNotifier` the widgets listen to,
// on the isolate they run on. Nothing outside lib/store/ imports
// `package:catenary_client`, and a store method that sends, reads, types,
// retries or discards returns a `Future`, so a later move to a background
// isolate changes this file and no widget. test/store_boundary_test.dart holds
// both. This row has none of those methods yet: the controls are CANT-209's.

import 'dart:async';
import 'dart:math' as math;

import 'package:catenary_client/catenary_client.dart';
import 'package:catenary_wire/catenary_wire.dart' as wire;
import 'package:flutter/foundation.dart';

import 'address.dart';
import 'connection.dart';
import 'conversation.dart';
import 'session.dart';
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
        : VoiceNote(duration: Duration(milliseconds: voice.durationMs), peaks: voice.peaks, transcript: voice.transcript.text),
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

/// The store the widgets listen to. Built over the platform's seams
/// (store/platform.dart builds the shipped ones), started once with [start],
/// and ended with [dispose].
///
/// Before [start] has found an enrolled device, [enrolled] is false and
/// everything else is empty: there is no fixture corpus behind it.
final class AppStore extends ChangeNotifier {
  /// [now] is wall-clock ms, which the banner's countdown is measured against.
  AppStore(this._seams, [this._now = systemClock]);

  final SessionSeams _seams;
  final Clock _now;

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

  /// Whether this device holds an address and a credential, and a session is
  /// running on them. False is the caller's cue for the enrollment screen.
  bool get enrolled => _session != null;

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
  /// projects it. Completes true when one is running and false when this
  /// device is not enrolled, in which case nothing was constructed. Calling it
  /// again ends the running session first: the restart a re-enrollment needs.
  Future<bool> start() async {
    _detach();
    final replacing = _session;
    _session = null;
    final started = await startSession(_seams, replacing: replacing, onOutbox: _onOutbox);
    final session = switch (started) {
      NotEnrolled() => null,
      SessionRunning(:final session) => session,
    };
    if (_disposed) {
      session?.end();
      return false;
    }
    _typing.clear();
    if (session == null) {
      _projection = emptyProjection;
      _outbox = const [];
      _conversations = const [];
      _connection = const ConnectionView(kind: ConnectionKind.reconnecting);
      notifyListeners();
      return false;
    }
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
