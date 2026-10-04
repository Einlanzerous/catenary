/// CANT-31 §4's close table, *preceded by*, and the protocol terminal (§6).
/// The twin of web/src/transport/test/closes.test.ts, which mirrors
/// internal/client/terminal_test.go.
library;

import 'dart:convert';
import 'dart:typed_data';

import 'package:catenary_client/catenary_client.dart';
import 'package:catenary_wire/catenary_wire.dart';
import 'package:test/test.dart';

import 'harness.dart';

ServerError err(ErrorCode code, {bool retryable = true, String? clientId}) =>
    ServerError(code: code, message: 'a ${code.wire}', retryable: retryable, clientId: clientId);

/// Drives one session to a close: `before` runs after `ready`, then the server
/// closes with `code`.
Future<({Rig r, SessionEnd end, TransportStatus status})> closeAfter(int code, void Function(FakeSocket s) before) async {
  final r = Rig();
  r.sync.answer = (_) => bootstrapPage();
  final ends = <SessionEnd>[];
  r.t.onSessionEnd(ends.add);
  r.t.start();
  final s = await r.connect();
  before(s);
  await flush();
  s.serverClose(code);
  await flush();
  return (r: r, end: ends.first, status: r.t.status());
}

void main() {
  test('the close table, row by row', () {
    final cases = <(String, int?, ServerError?, CloseVerdict)>[
      ('4001', 4001, null, CloseVerdict.terminalProtocol),
      ('4001 after an error', 4001, err(ErrorCode.internal), CloseVerdict.terminalProtocol),
      ('bare 1008', 1008, null, CloseVerdict.terminalProtocol),
      ('1008 after error{unauthorized}', 1008, err(ErrorCode.unauthorized), CloseVerdict.terminalProtocol),
      ('1008 after error{wire_version_unsupported}', 1008, err(ErrorCode.wireVersionUnsupported), CloseVerdict.terminalProtocol),
      ('1008 after error{internal}, retryable', 1008, err(ErrorCode.internal), CloseVerdict.reconnectAtMaximum),
      ('1008 after error{internal}, not retryable', 1008, err(ErrorCode.internal, retryable: false), CloseVerdict.reconnectAtMaximum),
      ('1008 after error{rate_limited}', 1008, err(ErrorCode.rateLimited), CloseVerdict.reconnect),
      ('1008 after error{not_a_member}', 1008, err(ErrorCode.notAMember), CloseVerdict.reconnect),
      ('1008 after an error code this build does not know', 1008, err(ErrorCode.unknown), CloseVerdict.reconnect),
      ('1000', 1000, null, CloseVerdict.reconnect),
      ('1001', 1001, null, CloseVerdict.reconnect),
      ('1005 (no status)', 1005, null, CloseVerdict.reconnect),
      ('1006 (abnormal)', 1006, null, CloseVerdict.reconnect),
      ('1009', 1009, null, CloseVerdict.reconnect),
      ('1011', 1011, null, CloseVerdict.reconnect),
      ('1012', 1012, null, CloseVerdict.reconnect),
      ('4000', 4000, null, CloseVerdict.reconnect),
      ('4002', 4002, null, CloseVerdict.reconnect),
      ('no close frame', null, null, CloseVerdict.reconnect),
      ('a code a later server adds', 4999, null, CloseVerdict.reconnect),
    ];
    for (final (name, code, preceding, want) in cases) {
      expect(classifyClose(code, preceding).verdict, want, reason: name);
    }
    expect(closeStatusKey(1006), -1);
    expect(closeStatusKey(1005), -1);
    expect(closeStatusKey(null), -1);
    expect(closeStatusKey(1001), 1001);
  });

  test('1008 after error{internal} reconnects at the maximum, not terminal', () async {
    final c = await closeAfter(1008, (s) => s.frame(err(ErrorCode.internal)));
    expect(c.end.verdict, CloseVerdict.reconnectAtMaximum);
    expect(c.end.bare1008, isFalse);
    expect(c.status.terminal.kind, TerminalKind.none);
    // At the maximum: the drawn wait is at least 0.8 × 5 s.
    expect(c.status.nextDialAt! - epoch, greaterThanOrEqualTo(4000));
  });

  test('an error naming a send does not precede the close', () async {
    final c = await closeAfter(1008, (s) => s.frame(err(ErrorCode.rateLimited, clientId: uuid(77))));
    expect(c.end.bare1008, isTrue);
    expect(c.end.verdict, CloseVerdict.terminalProtocol);
  });

  test('every kind of message event clears preceded-by before decoding', () async {
    final events = <(String, Object?, bool)>[
      ('a frame the decoder returns null for', jsonEncode({'type': 'a_frame_from_the_future'}), false),
      ('a frame the generated decoder refuses', jsonEncode({'type': 'ready'}), true),
      ('text that is not JSON', '{"type": "error", "code": ', true),
      ('a binary message', Uint8List(8), true),
      ('a binary message as a list of bytes', <int>[123, 125], true),
    ];
    for (final (name, data, undecodable) in events) {
      final c = await closeAfter(1008, (s) {
        s.frame(err(ErrorCode.internal));
        s.raw(data);
      });
      expect(c.end.preceding, isNull, reason: '$name: preceded-by is cleared');
      expect(c.end.bare1008, isTrue, reason: '$name: the close reads as a BARE 1008');
      expect(c.end.verdict, CloseVerdict.terminalProtocol, reason: '$name: which is terminal');
      expect(c.status.stats.undecodable, undecodable ? 1 : 0, reason: '$name: undecodable');
    }
  });

  test('a protocol terminal ends on nothing but a relaunch, and deletes nothing', () async {
    final r = Rig();
    r.sync.answer = (req) => req.after == 0 ? bootstrapPage(3, [message(1), message(2), message(3)]) : bootstrapPage(req.after);
    r.t.start();
    final s = await r.connect();
    final held = wireShape(r.t.snapshot());
    expect(r.t.snapshot().messages, hasLength(3));
    s.serverClose(4001);
    await flush();
    expect(r.t.status().terminal.kind, TerminalKind.protocol);
    expect(r.t.status().terminal.reason, contains('4001'));
    final dials = r.t.status().stats.dials;

    // Not a timer, not a wake signal, not retryNow, not start().
    await r.clock.advance(10 * 60000);
    for (final e in [LifecycleEvent.online, LifecycleEvent.visible, LifecycleEvent.pageshow, LifecycleEvent.resume]) {
      r.lifecycle.emit(e);
    }
    r.clock.jump(10 * 60000);
    await r.clock.advance(0);
    r.t.retryNow();
    r.t.start();
    await flush();
    expect(r.t.status().stats.dials, dials, reason: 'no dial after terminal');
    expect(r.net.sockets, hasLength(1));
    expect(r.t.status().terminal.kind, TerminalKind.protocol, reason: 'still terminal');

    // Nothing deleted: the journal is whole, and the credential is what it was.
    expect(wireShape(r.t.snapshot()), held);

    // A relaunch — a new transport over the same stores — dials again, from
    // the cursor it left.
    final again = r.again();
    again.t.start();
    await flush();
    expect(again.net.sockets, hasLength(1));
    again.net.last.open();
    expect(again.net.last.written_<ClientHello>().single.resumeFromLogSeq, 3);
  });

  test('a maxed-out backoff with no terminal reports none', () async {
    final r = Rig();
    r.net.onDial = (s) => s.fail();
    r.sync.answer = (_) => const NetworkDown();
    r.t.start();
    await r.clock.advance(60000);
    final st = r.t.status();
    expect(st.terminal.kind, TerminalKind.none);
    expect(st.nextDialAt, isNotNull, reason: 'a dial is pending');
    expect(st.stats.dials, greaterThan(10));
  });

  test('1008 after error{unauthorized} and after error{wire_version_unsupported} are terminal', () async {
    for (final code in [ErrorCode.unauthorized, ErrorCode.wireVersionUnsupported]) {
      final c = await closeAfter(1008, (s) => s.frame(err(code, retryable: false)));
      expect(c.end.verdict, CloseVerdict.terminalProtocol, reason: code.wire);
      expect(c.end.bare1008, isFalse, reason: '${code.wire}: frame-preceded, not bare');
      expect(c.status.terminal.kind, TerminalKind.protocol, reason: code.wire);
    }
  });

  test('negative controls · neverTerminal and alwaysTerminal each break the table', () async {
    final never = Rig(faults: const Faults(neverTerminal: true));
    never.sync.answer = (_) => bootstrapPage();
    never.t.start();
    (await never.connect()).serverClose(4001);
    await flush();
    expect(never.t.status().terminal.kind, TerminalKind.none, reason: 'neverTerminal: a revoked session reconnects');

    final always = Rig(faults: const Faults(alwaysTerminal: true));
    always.sync.answer = (_) => bootstrapPage();
    always.t.start();
    (await always.connect()).serverClose(1001);
    await flush();
    expect(always.t.status().terminal.kind, TerminalKind.protocol, reason: 'alwaysTerminal: a drain stops the client');
  });
}
