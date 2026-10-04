// The typing indicator's naming rule (deliberate call 10a, spec card F).
//
// IT IS A RULE AND NOT A STRING, which is why it lives in the store and not in
// the widget that shows it: both clients are held to the same cases. The web
// client's is `typingLabel` in web/src/store.ts, and its assertions are the
// five `typing ·` checks in web/smoke.ts; test/typing_test.dart holds this
// function to the same five, with the same names.
//
//   nobody          nothing
//   one             a first name
//   two or three    comma-separated, IN THE ORDER THEY STARTED
//   four or more    "Several people" — past three the list churns faster than
//                   it can be read

/// The label for the people typing in one conversation, or null when nobody
/// is. [names] are their display names in the order each started typing.
String? typingLabel(List<String> names) {
  if (names.isEmpty) return null;
  if (names.length >= 4) return 'Several people';
  return names.map((n) => n.split(' ').first).join(', ');
}
