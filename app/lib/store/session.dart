// The live session: the twin of `startSession` in web/src/store.ts. It reads
// the stored address and the stored credential, and either says this device is
// not enrolled or builds what a signed-in device runs on — a refreshing
// credential, a transport over the SQLite journal, and the outbox — and starts
// it.
//
// RULING 2 → THE UI ISOLATE. All of it lives where the widgets do. Nothing
// outside lib/store/ imports `package:catenary_client`
// (test/store_boundary_test.dart), so a later move to a background isolate
// changes the store and no widget.
//
// NOT ENROLLED STARTS NOTHING. With no `server` file, or no stored credential,
// no transport is constructed and no socket opens (CANT-152's rule in the web
// client): the caller's cue to show the enrollment screen. The address is read
// first, so a device that never enrolled opens no database at all.
//
// THE PLATFORM SEAMS are [SessionSeams]: the directory, the lifecycle signals,
// and optionally the HTTP and socket implementations and the logger. The clock
// and the random source are the package's defaults. store/platform.dart builds
// the shipped ones (path_provider, connectivity_plus, the widget binding), and
// a test passes a temporary directory and a `ManualLifecycle`.

import 'package:catenary_client/catenary_client.dart';

import 'address.dart';

/// `catenary.db` holds the journal and, in its own table, the credential;
/// `catenary-outbox.db` is the outbox's alone, beside it and not in it
/// (docs/decisions/cant-42-dart-client.md).
const journalFileName = 'catenary.db';
const outboxFileName = 'catenary-outbox.db';

/// Builds the transport. The default is the package's; a test passes one that
/// records the call, which is how "constructs no transport" is asserted.
typedef TransportFactory = Transport Function(TransportConfig cfg);

final class SessionSeams {
  const SessionSeams({
    required this.directory,
    required this.lifecycle,
    this.fetch,
    this.connect,
    this.logger,
    this.transportFactory = createTransport,
  });

  /// Where `catenary.db`, `catenary-outbox.db`, the lock files and `server`
  /// live: the application-support directory on a device.
  final String directory;

  /// `online`/`offline` from the network, `visible`/`hidden` from the app's
  /// own lifecycle.
  final Lifecycle lifecycle;

  /// Default: `ioHttpFetch` and `ioWebSocket`, which run under Flutter.
  final HttpFetch? fetch;
  final WebSocketConnect? connect;
  final Logger? logger;
  final TransportFactory transportFactory;
}

/// What [startSession] found.
sealed class SessionStart {
  const SessionStart();
}

/// No address or no credential. Nothing was constructed.
final class NotEnrolled extends SessionStart {
  const NotEnrolled();
}

final class SessionRunning extends SessionStart {
  const SessionRunning(this.session);

  final Session session;
}

/// A started transport with the stores under it. [end] is idempotent.
final class Session {
  Session._({
    required this.address,
    required this.credential,
    required this.transport,
    required this.outbox,
    required this.journal,
    required this.credentials,
    required this._close,
  });

  /// The origin the credential belongs to.
  final String address;
  final StoredCredential credential;
  final Transport transport;
  final Outbox outbox;
  final SqliteJournal journal;

  /// The credential's store, which `enrollCredential` and `reenrollCredential`
  /// write through: the same file the transport's refreshing credential reads.
  final SqliteCredentialStore credentials;
  final void Function() _close;
  var _ended = false;

  /// Stops the transport, detaches the outbox and closes every file. A
  /// terminal transport refuses `start()`, so a new credential gets a new
  /// session and not a restart of this one.
  void end() {
    if (_ended) return;
    _ended = true;
    _close();
  }
}

/// Reads the address and the credential from [seams.directory] and, with both,
/// starts a session. [replacing] is ended first, so the same call is the
/// restart a login or a re-enrollment needs.
Future<SessionStart> startSession(SessionSeams seams, {Session? replacing}) async {
  replacing?.end();
  final dir = seams.directory;
  final address = readAddress(dir);
  if (address == null) return const NotEnrolled();

  final credentials = SqliteCredentialStore.open('$dir/$journalFileName');
  final StoredCredential? held;
  try {
    held = await credentials.read();
  } on Object {
    credentials.close();
    rethrow;
  }
  if (held == null) {
    credentials.close();
    return const NotEnrolled();
  }

  final logger = seams.logger ?? const SilentLogger();
  final locks = SqliteLocks(dir);
  final journal = SqliteJournal.open('$dir/$journalFileName');
  final outboxStore = SqliteOutboxStore.open('$dir/$outboxFileName');
  final transport = seams.transportFactory(TransportConfig(
    baseUrl: address,
    credential: RefreshingCredential(
      baseUrl: address,
      store: credentials,
      lock: locks.call,
      logger: logger,
      fetch: seams.fetch,
    ),
    journal: journal,
    clientVersion: 'catenary-app',
    connect: seams.connect,
    fetch: seams.fetch,
    lifecycle: seams.lifecycle,
    logger: logger,
  ));
  // Attached before the first dial, so the outbox sees the first `ready`
  // rather than reading it late.
  final adapter = TransportOutbox(transport, logger);
  final Outbox outbox;
  try {
    outbox = await Outbox.open(
      store: outboxStore,
      transport: adapter,
      accountId: held.userId,
      lock: SqliteDrainLock(locks),
    );
  } on Object {
    adapter.close();
    outboxStore.close();
    journal.close();
    locks.close();
    credentials.close();
    rethrow;
  }
  transport.start();
  return SessionRunning(Session._(
    address: address,
    credential: held,
    transport: transport,
    outbox: outbox,
    journal: journal,
    credentials: credentials,
    close: () {
      transport.stop();
      outbox.close();
      adapter.close();
      outboxStore.close();
      journal.close();
      locks.close();
      credentials.close();
    },
  ));
}

/// Opens the credential store beside the journal, for enrollment before there
/// is a session to take it from.
SqliteCredentialStore openCredentialStore(String directory) => SqliteCredentialStore.open('$directory/$journalFileName');
