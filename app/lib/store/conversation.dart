// The rail and the thread as the views read them: what a conversation's row
// says, and what a thread shows between its header and its composer. The twin
// of the derivations in web/src/store.ts and web/src/components.
//
// DERIVED, NOT STORED, wherever the two could disagree (Invariant 3). There
// is no stored unread count: the rail's badge and the thread's "N NEW" rule
// both come from `firstUnreadSeq`, so they cannot drift apart, and neither
// counts your own messages — you cannot have an unread message you sent. A
// transcript's word count is derived from the text on screen. A stamp is
// derived from `now`.
//
// These are view models. An enrolled device's are built from the journal and
// the outbox by store/app_store.dart; the specimen screen's and the widget
// tests' are fixtures (fixtures.dart).

import 'package:flutter/foundation.dart';

import 'status.dart';
import 'when.dart';

enum ConversationKind { group, direct }

/// Where a voice note's transcript stands, as the server last said. The
/// wire's `TranscriptState`, kept apart from the text so that "no text" is not
/// read as "still coming" (CANT-220 rulings 5 and 6).
enum TranscriptStatus {
  /// The job has not finished. The only state drawn as `TRANSCRIBING`.
  pending,
  ready,

  /// The job failed, and there will be no transcript.
  failed,

  /// A state this build does not know. Nothing is claimed about it.
  unknown,
}

/// A voice note as a message carries it. [peaks] are the server's, 0–100: the
/// waveform is computed there, never on a client.
@immutable
class VoiceNote {
  // ignore: prefer_initializing_formals — the parameter is `status`, the field is private.
  const VoiceNote({required this.duration, required this.peaks, this.transcript, TranscriptStatus? status}) : _status = status;

  final Duration duration;
  final List<int> peaks;

  /// The transcript's text, held only once it is ready.
  final String? transcript;
  final TranscriptStatus? _status;

  /// A note built without a status has one by its text: a fixture's, or one
  /// of your own that has not left this device.
  TranscriptStatus get status => _status ?? (transcript == null ? TranscriptStatus.pending : TranscriptStatus.ready);
}

@immutable
class ImageAttachment {
  const ImageAttachment({required this.filename, required this.width, required this.height});

  final String filename;
  final int width;
  final int height;
}

@immutable
class ThreadMessage {
  const ThreadMessage({
    required this.id,
    required this.seq,
    required this.authorId,
    required this.authorName,
    required this.at,
    required this.mine,
    this.text,
    this.voice,
    this.image,
    this.status = MessageStatus.sent,
    this.readBy,
    this.failure,
    this.retrying = false,
  });

  final String id;
  final int seq;
  final String authorId;
  final String authorName;
  final DateTime at;
  final bool mine;
  final String? text;
  final VoiceNote? voice;
  final ImageAttachment? image;

  /// Rendered on your own messages only.
  final MessageStatus status;
  final int? readBy;

  /// Why a failed send failed, in the server's or the outbox's words.
  final String? failure;

  /// An unsent message held under backoff past its third retryable refusal:
  /// the outbox's, and what turns QUEUED into RETRYING (CANT-36 ruling 3 C).
  final bool retrying;
}

@immutable
class ConversationView {
  const ConversationView({
    required this.id,
    required this.kind,
    required this.name,
    required this.memberCount,
    required this.messages,
    this.firstUnreadSeq,
    this.muted = false,
    this.typing = const [],
    this.secure = true,
    this.unacked = 0,
  });

  final String id;
  final ConversationKind kind;
  final String name;
  final int memberCount;

  /// The log in seq order, then the tail: the last [unacked] of these.
  final List<ThreadMessage> messages;
  final int? firstUnreadSeq;
  final bool muted;

  /// The display names of the people typing, in the order each started.
  final List<String> typing;

  /// Whether the address these messages travel to is `https`. The store sets
  /// it from the stored address's scheme (store/address.dart); a fixture is
  /// the canvas, which is drawn over TLS.
  final bool secure;

  /// How many of [messages], at its end, are your own outbox entries the
  /// server has not acked. They are drawn after the log whatever their age: an
  /// entry has no seq to be placed by.
  final int unacked;

  /// What the rail says this conversation last held: the newer of the log's
  /// last message and the tail's, by time (`lastMessageOf` in
  /// web/src/store.ts). A send moves the preview, the marker and the room's
  /// place the moment it is composed, and a failed one from this morning does
  /// not sit over a reply from this afternoon.
  ThreadMessage? get last {
    if (messages.isEmpty) return null;
    final entry = unacked > 0 ? messages.last : null;
    final record = unacked < messages.length ? messages[messages.length - unacked - 1] : null;
    if (entry == null || record == null) return entry ?? record;
    return entry.at.isBefore(record.at) ? record : entry;
  }

  /// What is new here: messages at or past `firstUnreadSeq` that somebody
  /// else wrote. The badge and the "N NEW" rule are both this number.
  int get newCount {
    final from = firstUnreadSeq;
    if (from == null) return 0;
    return messages.where((m) => m.seq >= from && !m.mine).length;
  }

  /// The thread header's second line. TLS, not E2E: the server can read
  /// these messages, and a badge asserting otherwise is the one claim this
  /// surface must not make (D1). And TLS only where it is: over an `http`
  /// address the last word is CLEARTEXT, because that is what the link is.
  String get subtitle {
    final transport = secure ? 'TLS' : 'CLEARTEXT';
    return kind == ConversationKind.group ? '$memberCount MEMBERS · $transport' : 'DIRECT · $transport';
  }
}

/// Two letters for an avatar tile: first and last initial, or the first two
/// letters of a single name.
String initials(String name) {
  final parts = name.trim().split(RegExp(r'\s+')).where((p) => p.isNotEmpty).toList();
  if (parts.isEmpty) return '';
  if (parts.length == 1) return parts.first.substring(0, parts.first.length < 2 ? parts.first.length : 2).toUpperCase();
  return (parts.first[0] + parts.last[0]).toUpperCase();
}

/// The rail's preview line for a conversation: its last message, said the way
/// its kind says it. A room's preview carries a first name; a direct's needs
/// none — the row is already the person.
String preview(ConversationView c) {
  final m = c.last;
  if (m == null) return '';
  final voice = m.voice;
  final String body;
  if (voice != null) {
    // "Pending" is the server's word for a note it holds and has not
    // transcribed yet. One of your own that never left this device is not
    // pending anything, and does not say so.
    final unsent = m.mine &&
        (m.status == MessageStatus.failed || m.status == MessageStatus.queued || m.status == MessageStatus.sending);
    // And only a pending one says so: a failed transcript is not coming, and
    // a state this build does not know is not claimed to be.
    body = voice.status == TranscriptStatus.pending && !unsent
        ? 'transcript pending · ${clock(voice.duration)}'
        : 'voice note · ${clock(voice.duration)}';
  } else if (m.image != null && (m.text == null || m.text!.isEmpty)) {
    body = 'photo';
  } else {
    body = m.text ?? '';
  }
  if (m.mine) return 'You: $body';
  return c.kind == ConversationKind.group ? '${m.authorName.split(' ').first}: $body' : body;
}

/// What sits at the right of a rail row's second line.
enum RailMarkKind { none, count, muted, status }

@immutable
class RailMark {
  const RailMark(this.kind, {this.count = 0, this.status, this.retrying = false});

  final RailMarkKind kind;
  final int count;
  final MessageStatus? status;
  final bool retrying;
}

/// The trailing marker: the unread count, else MUTED, else your own last
/// message's delivery state. [open] is whether this conversation is the one
/// last opened.
RailMark railMark(ConversationView c, {bool open = false}) {
  // The open row carries no marker at all, as the canvas draws it: its accent
  // bar already says where you are.
  if (open) return const RailMark(RailMarkKind.none);
  final unread = c.newCount;
  if (unread > 0) return RailMark(RailMarkKind.count, count: unread);
  if (c.muted) return const RailMark(RailMarkKind.muted);
  final m = c.last;
  if (m != null && m.mine) return RailMark(RailMarkKind.status, status: m.status, retrying: m.retrying);
  return const RailMark(RailMarkKind.none);
}

/// Whether a row has gone quiet and dims its name. Deliberate call 05 rejects
/// bold-name-for-unread; this contrast replaces it. As the narrow canvas
/// draws it: a row with something unread stays bright, and so does the open
/// one; otherwise a row dims once its last message is from another day, or is
/// somebody else's that you have read.
///
/// The web client's rule is the desktop canvas's, "not today", which leaves a
/// read row from this morning bright. The narrow canvas dims that row (Nadia,
/// 09:41), and this follows the canvas it is built against.
bool isQuiet(ConversationView c, DateTime now, {bool open = false}) {
  final m = c.last;
  if (m == null || open || c.newCount > 0) return false;
  return !sameDay(m.at, now) || !m.mine;
}

/// One row of a thread, in the order it is drawn.
sealed class ThreadRow {
  const ThreadRow();
}

/// A date separator: `15 AUG`.
class DayRow extends ThreadRow {
  const DayRow(this.day);

  final DateTime day;
}

/// The "N NEW" rule, above the first unread message.
class NewRow extends ThreadRow {
  const NewRow(this.count);

  final int count;
}

/// One author's run of messages. The header — avatar, name, time — is the
/// group's, and continuation lines drop it entirely (narrow call M1).
class GroupRow extends ThreadRow {
  const GroupRow(this.messages);

  final List<ThreadMessage> messages;

  ThreadMessage get first => messages.first;
}

/// Lays a conversation out as rows. A group breaks on a new author, a new
/// day, and the unread rule; nothing else splits one.
List<ThreadRow> threadRows(ConversationView c) {
  final rows = <ThreadRow>[];
  final newCount = c.newCount;
  final from = c.firstUnreadSeq;
  var ruled = false;
  List<ThreadMessage>? run;
  ThreadMessage? previous;
  for (final m in c.messages) {
    var broke = false;
    if (previous == null || !sameDay(previous.at, m.at)) {
      rows.add(DayRow(m.at));
      broke = true;
    }
    // Above the first message that is new to you: your own are never new, so
    // the rule sits above the first of somebody else's at or past the marker.
    if (!ruled && newCount > 0 && from != null && m.seq >= from && !m.mine) {
      rows.add(NewRow(newCount));
      ruled = broke = true;
    }
    if (broke || run == null || previous!.authorId != m.authorId) {
      run = [m];
      rows.add(GroupRow(run));
    } else {
      run.add(m);
    }
    previous = m;
  }
  return rows;
}
