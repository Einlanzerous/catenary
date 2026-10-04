// The canvas's cast and its one moment, as data — `Catenary Mobile.dc.html`,
// frames 01 and 02. The app shows this until it is fed by a journal
// (CANT-200), and the tests and renders are built from it.
//
// NOTHING HERE IS PINNED TO A DATE. The canvas is a mock of 16 August at
// 14:16 and its stamps are fixed strings. These are built from `now`: "14:12"
// is four minutes ago, "MON" is the most recent Monday before today. So the
// rail reads the same on any day it is opened, and a test can move `now`
// forward a week and watch every stamp change — which a list of fixed dates
// would instead show by going dim (CANT-43).
//
// THE WAVEFORM PEAKS ARE FIXTURE DATA, standing in for the server's. A real
// voice note's peaks are computed server-side and arrive on the wire; no
// client derives them.

import 'store/conversation.dart';
import 'store/status.dart';

const me = 'u-hollis';

/// Deterministic bars for a fixture voice note: a hump of [n] heights, 9–100.
List<int> fixturePeaks(int seed, int n) {
  var s = seed;
  final out = <int>[];
  for (var i = 0; i < n; i++) {
    s = (s * 1103515245 + 12345) % 2147483648;
    final r = s / 2147483648;
    // A half-sine envelope, by its parabola: fixtures need no trigonometry.
    final x = i / (n - 1);
    final env = 0.35 + 0.65 * (4 * x * (1 - x));
    final h = ((0.22 + 0.78 * r) * env * 100).round();
    out.add(h < 9 ? 9 : h);
  }
  return out;
}

/// Ninety-six words, as the canvas's `EXPAND · 96 W` says — a count the app
/// derives from this text and does not store.
const _transcript =
    'Okay so I talked to Ted about the delivery and the short version is Thursday still works but they want '
    'everything staged by two, which means somebody has to be at the shed in the morning to sign for the pallet. If '
    'nobody can do that I will ask them to hold it until Friday. The depot said the window is firm this time, so if '
    'we miss it the whole order goes back on the truck and we start again next week, which nobody wants. I can '
    'cover the afternoon but not the morning, sorry.';

/// The conversations of canvas frame 01, as they stand at [now].
List<ConversationView> fixtureConversations(DateTime now) {
  // Today, at a clock time that has already happened.
  DateTime today(int minutesAgo) => now.subtract(Duration(minutes: minutesAgo));
  // The most recent given weekday strictly before today, at noon.
  DateTime last(int weekday) {
    var d = DateTime(now.year, now.month, now.day, 12).subtract(const Duration(days: 1));
    while (d.weekday != weekday) {
      d = d.subtract(const Duration(days: 1));
    }
    return d;
  }

  var n = 0;
  ThreadMessage msg(
    int seq,
    String authorId,
    String authorName,
    DateTime at, {
    String? text,
    VoiceNote? voice,
    ImageAttachment? image,
    MessageStatus status = MessageStatus.sent,
    int? readBy,
    String? failure,
  }) =>
      ThreadMessage(
        id: 'm-${n++}',
        seq: seq,
        authorId: authorId,
        authorName: authorName,
        at: at,
        mine: authorId == me,
        text: text,
        voice: voice,
        image: image,
        status: status,
        readBy: readBy,
        failure: failure,
      );

  return [
    ConversationView(
      id: 'kitchen',
      kind: ConversationKind.group,
      name: 'Kitchen Table',
      memberCount: 7,
      firstUnreadSeq: 3,
      typing: const ['Nadia Okonkwo'],
      messages: [
        msg(1, 'u-nadia', 'Nadia Okonkwo', today(35),
            text: 'Anyone know whether the co-op still does the Thursday pickup, or did that move for the summer?'),
        msg(2, me, 'Hollis Brandt', today(24),
            text: 'Still Thursday. They moved the window, not the day — 2 to 6 now instead of noon.',
            status: MessageStatus.read,
            readBy: 5),
        msg(3, 'u-ilse', 'Ilse Marchetti', today(4),
            voice: VoiceNote(duration: const Duration(seconds: 38), peaks: fixturePeaks(9931, 48), transcript: _transcript)),
        msg(4, 'u-ilse', 'Ilse Marchetti', today(4), text: 'photo from this morning, the whole run is re-tensioned'),
        msg(5, 'u-ilse', 'Ilse Marchetti', today(4),
            image: const ImageAttachment(filename: 'IMG_4471.HEIC', width: 3024, height: 2016)),
        msg(6, me, 'Hollis Brandt', today(1), text: 'I can be there at eight to sign for it.', status: MessageStatus.delivered),
      ],
    ),
    ConversationView(
      id: 'coop',
      kind: ConversationKind.group,
      name: 'Bergen Hill Co-op',
      memberCount: 31,
      firstUnreadSeq: 1,
      messages: [
        for (var i = 1; i <= 11; i++) msg(i, 'u-rosa', 'Rosa Whitfield', today(78 + 12 - i), text: 'crate $i is on the dock'),
        msg(12, 'u-ted', 'Ted Almasy', today(18), text: 'the delivery window moved to Thursday afternoon, I will confirm with the depot'),
      ],
    ),
    ConversationView(
      id: 'dinner',
      kind: ConversationKind.group,
      name: 'Sunday Dinner',
      memberCount: 5,
      messages: [msg(1, me, 'Hollis Brandt', today(176), text: 'bringing the big pot', status: MessageStatus.read, readBy: 4)],
    ),
    ConversationView(
      id: 'shed',
      kind: ConversationKind.group,
      name: 'Shed Projects',
      memberCount: 4,
      muted: true,
      messages: [
        msg(1, 'u-marek', 'Marek Dubois', last(DateTime.monday),
            image: const ImageAttachment(filename: 'IMG_4402.HEIC', width: 3024, height: 2016)),
      ],
    ),
    ConversationView(
      id: 'ilse',
      kind: ConversationKind.direct,
      name: 'Ilse Marchetti',
      memberCount: 2,
      firstUnreadSeq: 1,
      messages: [
        msg(1, 'u-ilse', 'Ilse Marchetti', today(12),
            voice: VoiceNote(duration: const Duration(seconds: 72), peaks: fixturePeaks(4477, 48))),
      ],
    ),
    ConversationView(
      id: 'marek',
      kind: ConversationKind.direct,
      name: 'Marek Dubois',
      memberCount: 2,
      messages: [msg(1, me, 'Hollis Brandt', today(89), text: 'sent it to your inbox instead')],
    ),
    ConversationView(
      id: 'ted',
      kind: ConversationKind.direct,
      name: 'Ted Almasy',
      memberCount: 2,
      messages: [
        msg(1, me, 'Hollis Brandt', last(DateTime.tuesday),
            voice: VoiceNote(duration: const Duration(seconds: 22), peaks: fixturePeaks(1777, 48)),
            status: MessageStatus.failed,
            failure: 'server rejected upload (413)'),
      ],
    ),
    ConversationView(
      id: 'nadia',
      kind: ConversationKind.direct,
      name: 'Nadia Okonkwo',
      memberCount: 2,
      messages: [msg(1, 'u-nadia', 'Nadia Okonkwo', today(275), text: 'no rush on any of it, truly')],
    ),
  ];
}
