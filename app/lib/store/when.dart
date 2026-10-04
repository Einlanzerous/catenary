// How the app words a time. The twin of web/src/lib/format.ts.
//
// THE RAIL'S VOCABULARY IS RELATIVE: today is a clock, this week is a weekday,
// older is a date. All three are the same width class, so the column never
// reflows. It is always computed against `now` and never pinned: the canvas is
// a mock of a single moment with its timestamps fixed, and a list rendered
// from stamps fixed that way goes dim, weekday by weekday, within a week
// (CANT-43).

const _months = ['JAN', 'FEB', 'MAR', 'APR', 'MAY', 'JUN', 'JUL', 'AUG', 'SEP', 'OCT', 'NOV', 'DEC'];
const _days = ['MON', 'TUE', 'WED', 'THU', 'FRI', 'SAT', 'SUN'];

String _pad(int n) => n.toString().padLeft(2, '0');

DateTime _startOfDay(DateTime d) => DateTime(d.year, d.month, d.day);

bool sameDay(DateTime a, DateTime b) => _startOfDay(a) == _startOfDay(b);

/// 24-hour clock, `14:12`. A thread carries one of these on every group
/// header.
String clockTime(DateTime at) => '${_pad(at.hour)}:${_pad(at.minute)}';

/// The date separator inside a thread: `15 AUG`.
String dayLabel(DateTime at) => '${at.day} ${_months[at.month - 1]}';

/// Whole calendar days from [at]'s date to [now]'s. Counted between the two
/// dates as UTC midnights, so a day with a clock change — 23 or 25 hours long
/// — is still one day.
int calendarDaysBetween(DateTime at, DateTime now) =>
    DateTime.utc(now.year, now.month, now.day).difference(DateTime.utc(at.year, at.month, at.day)).inDays;

/// The rail's stamp for a conversation's last message, relative to [now].
/// Calendar days, not 24-hour spans: yesterday at 23:50 is a weekday at 00:10
/// today.
String railStamp(DateTime at, DateTime now) {
  if (sameDay(at, now)) return clockTime(at);
  if (calendarDaysBetween(at, now) < 7) return _days[at.weekday - 1];
  return dayLabel(at);
}

/// How many words a text has. A transcript's `EXPAND · 96 W` is derived from
/// the text on screen with this, so the count cannot lie.
int countWords(String text) => text.trim().split(RegExp(r'\s+')).where((w) => w.isNotEmpty).length;
