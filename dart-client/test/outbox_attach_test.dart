/// `Outbox.canAttachFile` (CANT-247 ruling 4 → A): the one predicate the
/// composer's ADD follows. It must say yes exactly when `compose` would take a
/// picked file.
library;

import 'dart:typed_data';

import 'package:catenary_client/catenary_client.dart';
import 'package:test/test.dart';

import 'harness.dart';
import 'outbox_harness.dart';

void main() {
  test('is false before a session, true while one is ready, false when it drops, and false once terminal', () async {
    final ctx = await context();
    final t = ctx.transport;
    expect(ctx.outbox.canAttachFile, isFalse, reason: 'no session yet');
    t.open();
    await flush();
    expect(ctx.outbox.canAttachFile, isTrue);
    t.close();
    expect(ctx.outbox.canAttachFile, isFalse, reason: 'reconnecting or offline');
    t.open();
    expect(ctx.outbox.canAttachFile, isTrue, reason: 'a ready session that is still catching up (resyncing) takes a file');
    t.terminal = true;
    expect(ctx.outbox.canAttachFile, isFalse, reason: 'a terminal transport, even with a session still reading ready');
    t.terminal = false;
    t.close(terminal: true);
    t.open();
    expect(ctx.outbox.canAttachFile, isTrue, reason: 'a new session clears the closed one\'s terminal flag, as compose does');
  });

  test('agrees with compose over those states: a picked file is taken exactly when it is true', () async {
    final ctx = await context();
    final t = ctx.transport;
    OutboxDraft file() => OutboxDraft(conversationId: conv, attachments: [OutboundAttachmentDraft(kind: 'image', source: MediaBytes(Uint8List(4)), filename: 'a.jpg')]);

    for (final step in <(String, void Function(), bool)>[
      ('offline', () {}, false),
      ('ready', t.open, true),
      ('dropped', t.close, false),
      ('terminal', () => t.close(terminal: true), false),
    ]) {
      step.$2();
      await flush();
      expect(ctx.outbox.canAttachFile, step.$3, reason: step.$1);
      if (step.$3) {
        await ctx.outbox.compose(file());
      } else {
        await expectLater(ctx.outbox.compose(file()), throwsA(isA<ComposeRefused>()), reason: step.$1);
      }
    }
  });
}
