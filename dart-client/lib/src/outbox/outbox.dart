/// The outbox's state machine — mirrors web/src/outbox/outbox.ts (CANT-36).
///
/// Pure logic over two seams — `OutboxStore` for persistence, `OutboxTransport`
/// for a session — plus §8's lock. It holds the FEWEST facts only it can know:
/// the entry, and whether it has been refused. Everything else is derived or
/// held in memory for this session only:
///
///   sending   = pending and in this session's in-flight set
///   queued    = pending and not in flight
///   sent      = an ack is held in memory for its clientId
///   RETRYING  = pending with three or more retryable refusals received
///
/// so a relaunch empties the in-flight set and the acks and every stored
/// `pending` entry reads QUEUED until the drain sends it again. The moment a
/// record carrying the entry's clientId is held — in ANY status — the entry is
/// deleted, and the server is the truth about that message from then on.
///
/// The rules are `docs/decisions/cant-36-outbox.md`; section numbers below are
/// that file's.
///
/// AN UPLOAD WAITS FOR A READY SESSION, AND IS THE LOCK HOLDER'S (CANT-201
/// ruling 1). A recording may be composed with no session at all: it is held
/// with its media, `pending`, and no uploader is called — one called with no
/// connection would only throw, and a throw fails the entry for good. It is
/// offered when this context is `ready` AND holds the drain lock, which is
/// also how an entry reloaded after a relaunch is offered, and why two
/// contexts over one store do not both upload it.
///
/// A SECOND CONTEXT SEES ANOTHER'S WRITES AT ITS NEXT READ. The reference
/// re-reads on a `BroadcastChannel` post; there is no channel here. Every
/// context re-reads on `refresh()`, and the lock's holder re-reads on a timer
/// while it holds, so an entry composed in a context that is not draining is
/// sent by the one that is.
library;

import 'dart:async';
import 'dart:math';
import 'dart:typed_data';

import 'package:catenary_wire/catenary_wire.dart';

import '../seams.dart';
import 'coordination.dart';
import 'types.dart';

/// §6: after this many retryable refusals a held entry reads RETRYING.
const retryingAfter = 3;

/// §6's backoff: 2 s, doubling, capped at 5 min, full jitter.
const outboxBackoffBaseMs = 2000;
const outboxBackoffCapMs = 300000;

/// How often the lock's holder re-reads the store for what another context
/// composed.
const holderRereadMs = 1000;

/// The inline error for a bare `1008` (CANT-31 §7): fixed, because the server
/// said nothing — the close is all there is.
const bare1008Message = 'This message could not be read by the server — update the app, then retry';

/// The retry delay for the nth retryable refusal: full jitter under the capped
/// exponential, and never shorter than the server's `retryAfterSec`.
int backoffMs(int n, double random, [int? retryAfterSec]) {
  final ceiling = min(outboxBackoffCapMs, outboxBackoffBaseMs * pow(2, max(0, n - 1)));
  final floor = (retryAfterSec ?? 0) * 1000;
  return max((random * ceiling).floor(), floor);
}

/// §6: the only two refusals that hold rather than fail. Everything else —
/// `internal` with `retryable: false`, `not_a_member`,
/// `conversation_not_found`, `message_too_large`, the door codes and the
/// `unknown` sentinel — goes to `failed` at once.
bool isHeldRefusal(ServerError e) => e.code == ErrorCode.rateLimited || (e.code == ErrorCode.internal && e.retryable);

final _secure = Random.secure();

/// A version 4 UUID from the platform's CSPRNG.
String mintUuid() {
  final b = [for (var i = 0; i < 16; i++) _secure.nextInt(256)];
  b[6] = (b[6] & 0x0f) | 0x40;
  b[8] = (b[8] & 0x3f) | 0x80;
  final hex = [for (final x in b) x.toRadixString(16).padLeft(2, '0')].join();
  return '${hex.substring(0, 8)}-${hex.substring(8, 12)}-${hex.substring(12, 16)}-${hex.substring(16, 20)}-${hex.substring(20)}';
}

String _iso(num ms) => DateTime.fromMillisecondsSinceEpoch(ms.floor(), isUtc: true).toIso8601String();

final class Outbox {
  Outbox._({
    required OutboxStore store,
    required OutboxTransport transport,
    required Uuid? accountId,
    DrainLock? lock,
    Clock? now,
    Timers? timers,
    double Function()? random,
    Uuid Function()? mintId,
    Uploader uploader = const RefusingUploader(),
    OutboxFaults faults = OutboxFaults.none,
    void Function(List<OutboxItem> items)? onChange,
    num rereadMs = holderRereadMs,
  })  : _store = store,
        _rereadMs = rereadMs,
        _transport = transport,
        _accountId = accountId,
        _lock = lock ?? InProcessLockHub().lock(),
        _now = now ?? systemClock,
        _timers = timers ?? const SystemTimers(),
        _random = random ?? _secure.nextDouble,
        _mintId = mintId ?? mintUuid,
        _uploader = uploader,
        _faults = faults,
        _onChange = onChange;

  /// Load every entry from the store, then attach to the transport.
  ///
  /// `accountId` is the enrolled account: entries composed under any other are
  /// neither rendered nor sent (§1). `lock` is §8's; on a device it is a
  /// `SqliteDrainLock`, which every context over the same directory respects.
  /// `rereadMs` is how often the lock's holder re-reads the store for what
  /// another context composed.
  static Future<Outbox> open({
    required OutboxStore store,
    required OutboxTransport transport,
    required Uuid? accountId,
    DrainLock? lock,
    Clock? now,
    Timers? timers,
    double Function()? random,
    Uuid Function()? mintId,
    Uploader uploader = const RefusingUploader(),
    OutboxFaults faults = OutboxFaults.none,
    void Function(List<OutboxItem> items)? onChange,
    num rereadMs = holderRereadMs,
  }) async {
    final outbox = Outbox._(
      store: store,
      transport: transport,
      accountId: accountId,
      lock: lock,
      now: now,
      timers: timers,
      random: random,
      mintId: mintId,
      uploader: uploader,
      faults: faults,
      onChange: onChange,
      rereadMs: rereadMs,
    );
    await outbox._load();
    outbox._attach();
    return outbox;
  }

  final OutboxStore _store;
  final OutboxTransport _transport;
  final DrainLock _lock;
  final Clock _now;
  final Timers _timers;
  final double Function() _random;
  final Uuid Function() _mintId;
  final Uploader _uploader;
  final OutboxFaults _faults;
  final void Function(List<OutboxItem> items)? _onChange;
  final num _rereadMs;

  Uuid? _accountId;
  final _entries = <Uuid, OutboxEntry>{};

  /// Settled or discarded this session. A re-read racing the delete must not
  /// bring one back.
  final _gone = <Uuid>{};

  /// Entries between a refusal and its persisted disposition. The drain must
  /// not resend one in that window.
  final _busy = <Uuid>{};
  final _inFlight = <Uuid>{};
  final _acks = <Uuid, ServerAck>{};

  /// Entries this context has offered to the uploader and not heard back on.
  /// One is never offered twice at once, whatever asks.
  final _uploading = <Uuid>{};

  var _ready = false;

  /// The last close this context saw was terminal (CANT-31 §6). Known only
  /// from that event: the seam has no way to ask a transport that was already
  /// terminal when the outbox attached.
  var _terminal = false;
  var _holding = false;
  void Function()? _releaseLock;
  Object? _wake;
  num _wakeAt = double.infinity;
  Object? _reread;
  void Function()? _unsubscribe;
  var _closed = false;

  // ── reading ────────────────────────────────────────────────────────────────

  /// The enrolled account's unsettled entries, in drain order, each with its
  /// derived state.
  List<OutboxItem> view() => [
        for (final entry in _sorted())
          if (_mine(entry))
            OutboxItem(
              entry: entry,
              state: _stateOf(entry, _acks[entry.clientId]),
              ack: _acks[entry.clientId],
              retrying: entry.status == OutboxStatus.pending && entry.internalRetries >= retryingAfter,
            ),
      ];

  /// Whether this context holds the drain lock.
  bool get isHolder => _holding;

  OutboxState _stateOf(OutboxEntry e, ServerAck? ack) {
    if (e.status == OutboxStatus.failed) return OutboxState.failed;
    if (_faults.persistSending) {
      final stored = e.storedStatusOverride;
      if (stored == 'sending') return OutboxState.sending;
      if (stored == 'queued') return OutboxState.queued;
    }
    if (ack != null) return OutboxState.sent;
    if (_inFlight.contains(e.clientId)) return OutboxState.sending;
    return OutboxState.queued;
  }

  List<OutboxEntry> _sorted() => _entries.values.toList()..sort((a, b) => a.order.compareTo(b.order));

  bool _mine(OutboxEntry e) => _faults.crossAccountLeak || (_accountId != null && e.accountId == _accountId);

  // ── actions ────────────────────────────────────────────────────────────────

  /// §2: mint the clientId ONCE, allocate `order` and write the entry in one
  /// transaction — and only when that transaction has committed does the entry
  /// render, or become eligible for a frame. Completes with the stored entry,
  /// whose `clientId` is the wire's idempotency key, WITHOUT WAITING FOR AN
  /// ACK: with no ready session the entry is queued and drains when one is.
  ///
  /// AN ATTACHMENT, WITH NO READY SESSION (CANT-201 ruling 1): a `voice` one is
  /// taken, held with its media, and uploaded when a session is ready; any
  /// other kind is a picked file and is refused with `ComposeRefused`, as is
  /// every attachment on a terminal client. A refusal writes nothing. An
  /// attachment not yet uploaded must name its `source`, which is read here,
  /// once, and stored in the entry's own transaction (ruling 0).
  Future<OutboxEntry> compose(OutboxDraft draft) async {
    final account = _accountId;
    if (account == null) throw StateError('outbox: no enrolled account to compose as');
    if (draft.attachments.isNotEmpty && !_ready) {
      if (_terminal) throw const ComposeRefused(ComposeRefused.terminal);
      if (draft.attachments.any((a) => a.kind != 'voice')) throw const ComposeRefused(ComposeRefused.pickedFileOffline);
    }
    final media = <Uint8List?>[];
    for (final a in draft.attachments) {
      final source = a.source;
      if (source != null) {
        media.add(await source.read());
      } else if (a.upload == UploadState.uploaded && a.uploadId != null) {
        media.add(null);
      } else {
        throw const ComposeRefused(ComposeRefused.noMedia);
      }
    }
    final base = OutboxEntry(
      clientId: _mintId(),
      accountId: account,
      conversationId: draft.conversationId,
      order: 0,
      composedAt: _iso(_now()),
      text: draft.text,
      replyToMessageId: draft.replyToMessageId,
      replyPreview: draft.replyPreview,
      attachments: [for (final a in draft.attachments) a.held()],
    );

    final OutboxEntry entry;
    if (_faults.sendBeforePersist) {
      entry = base..order = _localOrder(account);
      _entries[entry.clientId] = entry;
      _drain();
      _emit();
      await _store.put(entry, media);
    } else if (_faults.orderOutsideTxn) {
      entry = base..order = _localOrder(account);
      await _store.put(entry, media);
    } else {
      entry = await _store.add(base, media);
    }

    if (_closed) return entry;
    _entries[entry.clientId] = entry;
    _emit();
    if (_awaitsUpload(entry)) {
      // Not this context's to upload, or not yet: the entry is held `pending`
      // and whoever holds the lock on a ready session is offered it.
      if (!_mayUpload) return entry;
      await _upload(entry.clientId);
      return _entries[entry.clientId] ?? entry;
    }
    _drain();
    return entry;
  }

  bool _awaitsUpload(OutboxEntry e) => e.attachments.any((a) => a.upload != UploadState.uploaded || a.uploadId == null);

  /// An upload is attempted only on a ready session and only by the drain
  /// lock's holder — §8's one writer, for uploads as for frames.
  bool get _mayUpload => _ready && _holding;

  /// §10, with no upload queue: each attachment not yet uploaded is offered to
  /// the `Uploader` — on compose, on every RETRY, and whenever the holder
  /// reads the store on a ready session, which is what offers an entry that
  /// was composed offline, in another context, or before a relaunch. A handle
  /// it returns is persisted on the entry before the next is asked for, and
  /// the entry joins the drain once its last one has; a refusal fails the
  /// entry with the uploader's own message — so an attachment entry is never
  /// left `pending` with nothing that will ever send it.
  Future<void> _upload(Uuid clientId) async {
    if (!_uploading.add(clientId)) return;
    try {
      final count = _entries[clientId]?.attachments.length ?? 0;
      for (var i = 0; i < count; i++) {
        final entry = _entries[clientId];
        if (entry == null || _closed) return;
        final a = entry.attachments[i];
        if (a.upload == UploadState.uploaded && a.uploadId != null) continue;
        final Uuid handle;
        try {
          handle = await _uploader.upload(entry, a);
        } catch (e) {
          await _fail(clientId, UploadFailure('$e'));
          return;
        }
        await _mutate(clientId, (x) {
          x.attachments = [
            for (final (j, b) in x.attachments.indexed)
              if (j == i) b.uploadedAs(handle) else b,
          ];
        });
      }
    } finally {
      _uploading.remove(clientId);
    }
    _emit();
    _drain();
  }

  /// Every `pending` entry of this account still awaiting an upload is offered,
  /// once: `_upload` passes over one already offered and not yet answered.
  void _offerUploads() {
    if (_closed || !_mayUpload) return;
    for (final e in _sorted()) {
      if (!_mine(e) || e.status != OutboxStatus.pending || !_awaitsUpload(e)) continue;
      unawaited(_upload(e.clientId));
    }
  }

  /// §7: `failed` back to `pending` under the SAME clientId, at its original
  /// `order`.
  Future<void> retry(Uuid clientId) async {
    final e = _entries[clientId];
    if (e == null || e.status != OutboxStatus.failed) return;
    if (_faults.remintOnRetry) {
      _forget(clientId);
      await _store.delete(clientId);
      final fresh = await _store.add(e.copy()
        ..clientId = _mintId()
        ..status = OutboxStatus.pending
        ..lastError = null);
      _entries[fresh.clientId] = fresh;
    } else {
      final pending = await _mutate(clientId, (x) {
        x.status = OutboxStatus.pending;
        x.lastError = null;
        x.notBefore = null;
      });
      // An attachment that never uploaded is offered again, so the entry
      // either joins the drain or fails again with the uploader's message —
      // or, with no ready session to upload on, waits for one as it would
      // have at compose.
      if (pending != null && _awaitsUpload(pending)) {
        _emit();
        if (_mayUpload) await _upload(clientId);
        return;
      }
    }
    _emit();
    _drain();
  }

  /// §7: DELETE, offered only on `failed`, removes the local copy alone. If the
  /// server did store the message, its record still arrives and renders.
  Future<bool> discard(Uuid clientId) async {
    final e = _entries[clientId];
    if (e == null || e.status != OutboxStatus.failed) return false;
    _forget(clientId);
    _emit();
    await _store.delete(clientId);
    return true;
  }

  /// §4: a held record carrying this clientId settles the entry, whatever its
  /// status. Called for every `RecordHeld` event, and by whoever applies
  /// records from elsewhere.
  Future<void> settle(Uuid? clientId) async {
    if (clientId == null) return;
    final e = _entries[clientId];
    if (e == null) return;
    if (_faults.settlePendingOnly && e.status != OutboxStatus.pending) return;
    _forget(clientId);
    _emit();
    await _store.delete(clientId);
  }

  /// Re-reads the store: what another context composed, settled or discarded
  /// since this one last read. Then drains, if this context is the drainer.
  Future<void> refresh() async {
    if (_closed) return;
    final rows = await _store.list();
    if (_closed) return;
    _entries.clear();
    for (final e in rows) {
      if (!_gone.contains(e.clientId)) _entries[e.clientId] = e;
    }
    // What this session knows of an entry that is no longer stored is moot.
    _inFlight.removeWhere((id) => !_entries.containsKey(id));
    _acks.removeWhere((id, _) => !_entries.containsKey(id));
    // Always: another context may have failed an entry, or retried one, and
    // the count says nothing of that.
    _emit();
    _drain();
    _offerUploads();
  }

  void setAccount(Uuid? accountId) {
    _accountId = accountId;
    _emit();
    _drain();
  }

  /// Detach from everything. Does not close the store, which the caller owns.
  void close() {
    _closed = true;
    _unsubscribe?.call();
    _unsubscribe = null;
    _dropLock();
    if (_wake != null) _timers.clearTimeout(_wake);
    _wake = null;
  }

  // ── lifecycle ──────────────────────────────────────────────────────────────

  Future<void> _load() async {
    var rows = await _store.list();
    if (_faults.remintOnRetry) {
      final reminted = <OutboxEntry>[];
      for (final e in rows) {
        await _store.delete(e.clientId);
        reminted.add(await _store.add(e.copy()..clientId = _mintId()));
      }
      rows = reminted;
    }
    _entries.clear();
    for (final e in rows) {
      if (!_gone.contains(e.clientId)) _entries[e.clientId] = e;
    }
  }

  void _attach() {
    _unsubscribe = _transport.subscribe(_onEvent);
    if (_faults.lockWithoutReady) _requestLock();
    if (_transport.isReady) _onReady();
    _emit();
  }

  void _onEvent(OutboxTransportEvent e) {
    if (_closed) return;
    switch (e) {
      case SessionReady():
        _onReady();
      case SessionClosed(:final bare1008, :final terminal):
        unawaited(_onClosed(bare1008, terminal));
      case SendAcked(:final ack):
        _onAck(ack);
      case SendErrored(:final error):
        unawaited(_onError(error));
      case RecordHeld(:final clientId):
        unawaited(settle(clientId));
      case Bootstrapped():
        // CANT-24 obligation 4 wipes server-derived state. None of this is.
        if (_faults.deleteOnTerminal) unawaited(_wipe());
    }
  }

  void _onReady() {
    _ready = true;
    _terminal = false;
    _requestLock();
    _drain();
    // Nothing unless this context already holds the lock; the grant's own
    // re-read is what offers a held recording on the first `ready`.
    _offerUploads();
  }

  /// §5 and §7: a close before the ack leaves an in-flight entry `pending`, to
  /// resend on the next `ready` — unless the transport classified the close as
  /// a bare `1008`, which fails it and does not requeue it (CANT-31 §7).
  Future<void> _onClosed(bool bare1008, bool terminal) async {
    _ready = false;
    _terminal = terminal;
    final flying = _inFlight.toList();
    _inFlight.clear();
    if (!_faults.lockWithoutReady) _dropLock();
    _emit();
    if (bare1008 && !_faults.resendAfterBare1008) {
      for (final id in flying) {
        await _fail(id, const Bare1008());
      }
    }
    // CANT-31 §6: a terminal state never deletes the local store.
    if (terminal && _faults.deleteOnTerminal) await _wipe();
  }

  void _onAck(ServerAck ack) {
    if (!_entries.containsKey(ack.clientId)) return;
    _inFlight.remove(ack.clientId);
    _acks[ack.clientId] = ack;
    _emit();
  }

  /// §6: a retryable refusal holds under capped backoff and never reaches
  /// `failed`; every other refusal fails at once.
  Future<void> _onError(ServerError err) async {
    final id = err.clientId;
    if (id == null || !_entries.containsKey(id)) return;
    _inFlight.remove(id);
    _busy.add(id);
    try {
      var hold = isHeldRefusal(err);
      if (hold && _faults.misclassifyRetryable) hold = false;
      if (!hold && _faults.retryNonRetryable) hold = true;

      final refusal = ServerRefusal(err.code.wire, err.message, err.retryable);
      if (!hold) {
        await _fail(id, refusal);
        return;
      }
      final now = _now();
      final updated = await _mutate(id, (x) {
        x.internalRetries++;
        x.notBefore = _iso(now + backoffMs(x.internalRetries, _random(), err.retryAfterSec));
      });
      if (updated != null && _faults.misjudgeRetryBudget && updated.internalRetries >= retryingAfter) {
        await _fail(id, refusal);
      }
    } finally {
      _busy.remove(id);
    }
    _emit();
    _drain();
  }

  Future<void> _fail(Uuid clientId, OutboxError error) async {
    await _mutate(clientId, (x) {
      x.status = OutboxStatus.failed;
      x.lastError = error;
      x.notBefore = null;
    });
    _emit();
  }

  // ── the drain ──────────────────────────────────────────────────────────────

  /// §8: only the lock holder writes, and it holds only while its own session
  /// is ready. §11: every eligible entry is written at once, in `order`,
  /// without waiting for each ack; one held back is overtaken.
  void _drain() {
    if (_closed || !_ready) return;
    if (!_holding && !_faults.everyTabDrains) return;

    final now = _now();
    num wakeAt = double.infinity;
    var wrote = false;
    for (final e in _sorted()) {
      if (!_mine(e)) continue;
      if (e.status != OutboxStatus.pending && !_faults.resendFailedWithoutRetry) continue;
      final id = e.clientId;
      if (_inFlight.contains(id) || _acks.containsKey(id) || _busy.contains(id)) continue;
      // An attachment not yet uploaded is skipped, never waited on.
      if (_awaitsUpload(e)) continue;
      final notBefore = e.notBefore;
      if (notBefore != null) {
        final t = DateTime.parse(notBefore).millisecondsSinceEpoch;
        if (t > now) {
          if (t < wakeAt) wakeAt = t;
          continue;
        }
      }

      _transport.sendFrame(frameOf(e));
      _inFlight.add(id);
      wrote = true;
      unawaited(_mutate(id, (x) {
        x.attempts++;
        if (_faults.persistSending) x.storedStatusOverride = 'sending';
      }));
    }
    _schedule(wakeAt);
    if (wrote) _emit();
  }

  void _schedule(num at) {
    if (at == _wakeAt) return;
    if (_wake != null) _timers.clearTimeout(_wake);
    _wake = null;
    _wakeAt = at;
    if (at == double.infinity) return;
    _wake = _timers.setTimeout(() {
      _wake = null;
      _wakeAt = double.infinity;
      _drain();
    }, max(0, at - _now()));
  }

  // ── §8 ─────────────────────────────────────────────────────────────────────

  void _requestLock() {
    if (_releaseLock != null || _closed) return;
    _releaseLock = _lock.request(() {
      if (_closed) return;
      if (!_ready && !_faults.lockWithoutReady) {
        _dropLock();
        return;
      }
      _holding = true;
      _rereadWhileHolding();
      // Whatever a previous holder had in flight is unacked-and-pending now,
      // and what it was handed since this context last read is in the store.
      unawaited(refresh());
      _emit();
    });
  }

  /// The holder's read of what other contexts composed: on a timer, for as
  /// long as it holds.
  void _rereadWhileHolding() {
    if (_reread != null) _timers.clearTimeout(_reread);
    _reread = _timers.setTimeout(() {
      _reread = null;
      if (!_holding || _closed) return;
      unawaited(refresh());
      _rereadWhileHolding();
    }, _rereadMs);
  }

  void _dropLock() {
    final release = _releaseLock;
    _releaseLock = null;
    _holding = false;
    if (_reread != null) _timers.clearTimeout(_reread);
    _reread = null;
    release?.call();
  }

  // ── plumbing ───────────────────────────────────────────────────────────────

  /// Every change to a stored entry goes through here, and the in-memory copy
  /// is only ever replaced by what the store committed.
  Future<OutboxEntry?> _mutate(Uuid clientId, void Function(OutboxEntry e) fn) async {
    final next = await _store.update(clientId, fn);
    if (next != null && !_gone.contains(clientId) && !_closed) _entries[clientId] = next;
    return next;
  }

  void _forget(Uuid clientId) {
    _gone.add(clientId);
    _entries.remove(clientId);
    _inFlight.remove(clientId);
    _acks.remove(clientId);
  }

  Future<void> _wipe() async {
    for (final id in _entries.keys.toList()) {
      _forget(id);
      await _store.delete(id);
    }
    _emit();
  }

  int _localOrder(Uuid accountId) {
    var highest = 0;
    for (final e in _entries.values) {
      if (e.accountId == accountId && e.order > highest) highest = e.order;
    }
    return highest + 1;
  }

  void _emit() {
    if (!_closed) _onChange?.call(view());
  }
}

/// The only path from an entry to the wire. `replyPreview`, `composedAt` and
/// every counter stay behind.
ClientSend frameOf(OutboxEntry e) => ClientSend(
      clientId: e.clientId,
      conversationId: e.conversationId,
      text: e.text,
      attachments: e.attachments.isEmpty ? null : [for (final a in e.attachments) OutboundAttachment(kind: a.kind, uploadId: a.uploadId!)],
      replyToMessageId: e.replyToMessageId,
    );
