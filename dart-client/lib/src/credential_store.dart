/// The durable credential — mirrors web/src/transport/credential-store.ts,
/// which mirrors the credential half of internal/client/journal.go
/// (`Credential`, `ChainLink`, `Journal.Enroll`, `Journal.Reenroll`,
/// `Journal.Rotate`, `Journal.propose`), with the store behind a seam: a row
/// in `catenary.db` on a device, memory in a test.
///
/// CANT-31 §2 and §3 (docs/decisions/cant-31-refresh-and-terminal-reconnect.md).
/// The record is the device's pair, the `Date` offset and issue time §1
/// persists with it, the proposal chain §3 persists before each request, and
/// CANT-127's `last_sent_at`. EVERY WRITE IS ONE ATOMIC READ-MODIFY-WRITE
/// (`update`), and a write completes only once it is durable — PERSIST BEFORE
/// PRESENT. The single-flight lock is the caller's (refresh.dart), taken
/// around the whole refresh, so a waiter that re-reads under it sees what the
/// holder wrote.
///
/// NOTHING HERE IS CLEARED BY A TERMINAL OR BY A JOURNAL WIPE (§6). The only
/// doors that replace a held pair with one that did not descend from it are
/// `enrollCredential` (refuses if one is held) and `reenrollCredential` (a
/// person re-enrolling a device the server stopped recognising).
///
/// AT REST (CANT-42 ruling 3 → in the SQLite file): the row is plain, in
/// `catenary.db`, which db.dart creates readable by its owner alone.
library;

import 'dart:convert';
import 'dart:io';

import 'package:catenary_wire/catenary_wire.dart';
import 'package:sqlite3/sqlite3.dart';

import 'db.dart';
import 'lock.dart';

/// One refresh this device sent and has not seen answered: the token it
/// presented and the successor it proposed. THE PROPOSAL IS THE ONE THE TOKEN
/// WAS FIRST PRESENTED WITH, and a token presented again carries it again.
final class ChainLink {
  const ChainLink(this.token, this.proposal);

  final String token;
  final String proposal;

  Map<String, Object?> toJson() => {'token': token, 'proposal': proposal};
}

/// What a device holds to get back in. Times are wall-clock ms since the epoch.
/// CLIENT-LOCAL: no field of this is on the wire as such, and the JSON it is
/// stored as is this file's own.
final class StoredCredential {
  const StoredCredential({
    required this.userId,
    required this.deviceId,
    required this.accessToken,
    required this.accessExpiresAt,
    required this.refreshToken,
    required this.refreshExpiresAt,
    this.accessIssuedAt,
    this.clockOffsetMs = 0,
    this.chain = const [],
    this.lastSentAt,
  });

  factory StoredCredential.fromJson(Map<String, dynamic> o) => StoredCredential(
        userId: o['userId'] as String,
        deviceId: o['deviceId'] as String,
        accessToken: o['accessToken'] as String,
        accessExpiresAt: o['accessExpiresAt'] as num?,
        refreshToken: o['refreshToken'] as String,
        refreshExpiresAt: o['refreshExpiresAt'] as num,
        accessIssuedAt: o['accessIssuedAt'] as num?,
        clockOffsetMs: o['clockOffsetMs'] as num,
        chain: [for (final l in o['chain'] as List) ChainLink((l as Map)['token'] as String, l['proposal'] as String)],
        lastSentAt: o['lastSentAt'] as num?,
      );

  /// `EnrollResponse.user_id`: who this device speaks as.
  final Uuid userId;

  /// `EnrollResponse.device_id`. A rotation never changes it.
  final Uuid deviceId;
  final String accessToken;

  /// Server clock. Null is NO EXPIRY LEARNED, which is never due: the reactive
  /// path is the net.
  final num? accessExpiresAt;

  /// Single-use: presenting it rotates the pair (CANT-29).
  final String refreshToken;
  final num refreshExpiresAt;

  /// The `Date` of the response that served this pair; null when it was never
  /// learned, which gets the 60 s floor (CANT-31 §1).
  final num? accessIssuedAt;

  /// Server minus device, from that same response; 0 when never learned.
  final num clockOffsetMs;

  /// THE INVARIANT: `chain[0].token` is `refreshToken`, and each later link's
  /// token is the one before's proposal. Empty whenever the last refresh was
  /// answered.
  final List<ChainLink> chain;

  /// CANT-127's `last_sent_at`: when the newest link was written, before its
  /// request left. Null with an empty chain.
  final num? lastSentAt;

  /// This credential with its chain and stamp replaced.
  StoredCredential withChain(List<ChainLink> chain, num? lastSentAt) => StoredCredential(
        userId: userId,
        deviceId: deviceId,
        accessToken: accessToken,
        accessExpiresAt: accessExpiresAt,
        refreshToken: refreshToken,
        refreshExpiresAt: refreshExpiresAt,
        accessIssuedAt: accessIssuedAt,
        clockOffsetMs: clockOffsetMs,
        chain: List.unmodifiable(chain),
        lastSentAt: lastSentAt,
      );

  Map<String, Object?> toJson() => {
        'userId': userId,
        'deviceId': deviceId,
        'accessToken': accessToken,
        'accessExpiresAt': accessExpiresAt,
        'refreshToken': refreshToken,
        'refreshExpiresAt': refreshExpiresAt,
        'accessIssuedAt': accessIssuedAt,
        'clockOffsetMs': clockOffsetMs,
        'chain': [for (final l in chain) l.toJson()],
        'lastSentAt': lastSentAt,
      };
}

/// What `update`'s function decides: what to store, if anything, and what to
/// hand back.
typedef CredentialWrite<T> = ({StoredCredential? write, T result});

/// The persisted credential. `update` is ONE atomic read-modify-write: `fn`
/// runs synchronously against what is stored now and says what to store, and
/// the future completes once that write is durable. Two contexts over one store
/// are two isolates, or two processes, over one file.
abstract interface class CredentialStore {
  Future<StoredCredential?> read();
  Future<T> update<T>(CredentialWrite<T> Function(StoredCredential? held) fn);
}

/// CANT-31 §2's lock, named per credential.
String credentialLockName(Uuid deviceId) => 'catenary.credential.$deviceId';

/// A store in memory: tests, and a host with nowhere durable to put it.
final class MemoryCredentialStore implements CredentialStore {
  MemoryCredentialStore([this._held]);

  StoredCredential? _held;

  @override
  Future<StoredCredential?> read() async => _held;

  @override
  Future<T> update<T>(CredentialWrite<T> Function(StoredCredential? held) fn) async {
    final (:write, :result) = fn(_held);
    if (write != null) _held = write;
    return result;
  }
}

/// The device's store: `catenary.db`, table `credential`, one row per device.
/// `update` is one `BEGIN IMMEDIATE … COMMIT` on a connection that runs
/// `synchronous = FULL`, so a rotated pair is on disk before anything presents
/// it. Its connection is its own: no journal statement reaches this table, and
/// a wipe leaves the row as it was.
final class SqliteCredentialStore implements CredentialStore {
  SqliteCredentialStore._(this._db);

  /// Opens `catenary.db` at [path], running any missing migration.
  static SqliteCredentialStore open(String path) => SqliteCredentialStore._(openCatenaryDb(path));

  final Database _db;

  void close() => _db.close();

  @override
  Future<StoredCredential?> read() async => _only();

  @override
  Future<T> update<T>(CredentialWrite<T> Function(StoredCredential? held) fn) async {
    _db.execute('BEGIN IMMEDIATE');
    try {
      final held = _only();
      final (:write, :result) = fn(held);
      if (write != null) {
        // ONE ROW PER DEVICE, and one device: a re-enrollment's new id
        // replaces the old device's row in the same transaction.
        _db.execute('DELETE FROM credential');
        _db.execute('INSERT INTO credential (device_id, record) VALUES (?, ?)', [write.deviceId, jsonEncode(write.toJson())]);
      }
      _db.execute('COMMIT');
      return result;
    } catch (_) {
      if (!_db.autocommit) _db.execute('ROLLBACK');
      rethrow;
    }
  }

  StoredCredential? _only() {
    final rows = _db.select('SELECT record FROM credential LIMIT 1');
    return rows.isEmpty ? null : StoredCredential.fromJson(jsonDecode(rows.single['record'] as String) as Map<String, dynamic>);
  }
}

/// The pair `POST /enroll` minted, as durable state (Go's
/// `CredentialFromEnroll` plus `WithServerDate`). `date` is the response's HTTP
/// `Date` header and `deviceNow` the wall clock when it arrived: the one moment
/// the two clocks are known to describe the same instant. With no usable `Date`
/// the pair has no offset and no issue time, and the proactive check reads it
/// against the 60 s floor until its first rotation.
StoredCredential credentialFromEnroll(EnrollResponse e, String? date, num deviceNow) {
  final served = parseHttpDate(date);
  return StoredCredential(
    userId: e.userId,
    deviceId: e.deviceId,
    accessToken: e.accessToken,
    accessExpiresAt: parseWireTime(e.accessExpiresAt, 'access_expires_at'),
    refreshToken: e.refreshToken,
    refreshExpiresAt: parseWireTime(e.refreshExpiresAt, 'refresh_expires_at'),
    accessIssuedAt: served,
    clockOffsetMs: served == null ? 0 : served - deviceNow,
  );
}

int parseWireTime(String s, String field) {
  final t = DateTime.tryParse(s);
  if (t == null) throw FormatException('credential: $field is not a timestamp');
  return t.millisecondsSinceEpoch;
}

/// An HTTP `Date` header as ms since the epoch; null for absent or unusable.
int? parseHttpDate(String? date) {
  if (date == null) return null;
  try {
    return HttpDate.parse(date).millisecondsSinceEpoch;
  } on Exception {
    return null;
  }
}

/// `enrollCredential` over a store that already holds a pair.
final class CredentialHeld implements Exception {
  const CredentialHeld();

  @override
  String toString() => 'CredentialHeld: this store already holds a credential; re-enroll to replace it';
}

/// A store nobody enrolled.
final class NoCredential implements Exception {
  const NoCredential();

  @override
  String toString() => 'NoCredential: this store holds no credential';
}

void _complete(StoredCredential c) {
  if (c.deviceId.isEmpty || c.accessToken.isEmpty || c.refreshToken.isEmpty) {
    throw ArgumentError('credential: a pair needs a device id, an access token and a refresh token');
  }
}

/// Seeds the store with a first enrollment's pair, ONCE (Go's
/// `Journal.Enroll`). A store that already holds one refuses: the held pair may
/// be a rotation ahead of the one the caller has, and overwriting it is the
/// spent-token restart CANT-121 exists to prevent. Under the credential's lock,
/// as every read-modify-write is.
Future<void> enrollCredential(CredentialStore store, Lock lock, StoredCredential cred) async {
  _complete(cred);
  final fresh = cred.withChain(const [], null);
  await lock(
    credentialLockName(cred.deviceId),
    () => store.update<void>((held) => held != null ? throw const CredentialHeld() : (write: fresh, result: null)),
  );
}

/// Replaces the held pair with a NEW enrollment's — a different device, as far
/// as the server is concerned (Go's `Journal.Reenroll`). The only thing that
/// ends a credential terminal (§6). The old device's chain and stamp mean
/// nothing now and go with it.
Future<void> reenrollCredential(CredentialStore store, Lock lock, StoredCredential cred) async {
  _complete(cred);
  final held = await store.read();
  if (held == null) throw const NoCredential();
  final fresh = cred.withChain(const [], null);
  await lock(
    credentialLockName(held.deviceId),
    () => store.update<void>((now) => now == null ? throw const NoCredential() : (write: fresh, result: null)),
  );
}
