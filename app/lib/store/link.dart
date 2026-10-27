// What a link preview card says, derived from the strings the wire carries
// (CANT-325, `Catenary Link Preview.dc.html`, "UNTRUSTED TEXT + HONESTY"). The
// twin of the web client's (CANT-330): the two keep these rules the same.
//
// THE HOST IS DERIVED FROM THE URL, never from `og:site_name`: it is the one
// thing on a card that is always true, because the sender's own URL says it.
// Title and description are text the TARGET PAGE chose, and are cleaned here
// and drawn as plain text, never as markup.
//
// TODO(CANT-331): a redirect to a different domain shows both hosts
// (`youtu.be → youtube.com`), and a login wall or a bot check is stored as an
// empty preview. Both wait on that decision; nothing here guesses either
// client-side.

/// The host a card shows: lowercased, a leading `www.` dropped, the port kept
/// when the URL names one, and an internationalized label shown as punycode
/// (`xn--`) so a lookalike host looks different. The URL itself when it has no
/// host to show.
String linkHost(String url) {
  final u = Uri.tryParse(url.trim());
  if (u == null || u.host.isEmpty) return url.trim();
  var host = _decode(u.host).toLowerCase();
  if (host.startsWith('www.')) host = host.substring(4);
  final ascii = host.split('.').map((l) => l.runes.every((r) => r < 0x80) ? l : 'xn--${_punycode(l)}').join('.');
  return u.hasPort ? '$ascii:${u.port}' : ascii;
}

String _decode(String s) {
  try {
    return Uri.decodeComponent(s);
  } on ArgumentError {
    return s;
  }
}

// RFC 3492, the encoder only: a label's non-ASCII code points as ASCII.
String _punycode(String label) {
  const base = 36, tMin = 1, tMax = 26, skew = 38, damp = 700;
  final input = label.runes.toList();
  final out = StringBuffer();
  for (final r in input) {
    if (r < 0x80) out.writeCharCode(r);
  }
  final basic = out.length;
  var handled = basic;
  if (basic > 0) out.write('-');
  var n = 128, delta = 0, bias = 72;
  String digit(int d) => String.fromCharCode(d < 26 ? 97 + d : 22 + d);
  int adapt(int d, int points, bool first) {
    d = first ? d ~/ damp : d ~/ 2;
    d += d ~/ points;
    var k = 0;
    while (d > ((base - tMin) * tMax) ~/ 2) {
      d ~/= base - tMin;
      k += base;
    }
    return k + ((base - tMin + 1) * d) ~/ (d + skew);
  }

  while (handled < input.length) {
    var m = 0x10FFFF;
    for (final r in input) {
      if (r >= n && r < m) m = r;
    }
    delta += (m - n) * (handled + 1);
    n = m;
    for (final r in input) {
      if (r < n) delta++;
      if (r == n) {
        var q = delta;
        for (var k = base;; k += base) {
          final t = k <= bias ? tMin : (k >= bias + tMax ? tMax : k - bias);
          if (q < t) break;
          out.write(digit(t + (q - t) % (base - t)));
          q = (q - t) ~/ (base - t);
        }
        out.write(digit(q));
        bias = adapt(delta, handled + 1, handled == basic);
        delta = 0;
        handled++;
      }
    }
    delta++;
    n++;
  }
  return out.toString();
}

/// Page-chosen text made safe to draw: whitespace runs and newlines collapse
/// to one space, control and bidi-override characters are stripped. The
/// server caps and strips too; a client does not trust that it did.
String cleanPageText(String s) => s
    .replaceAll(RegExp(r'\s+'), ' ')
    .replaceAll(RegExp(r'[\x00-\x1F\x7F-\x9F\u061C\u200E\u200F\u202A-\u202E\u2066-\u2069]'), '')
    .trim();

final _urlPattern = RegExp(r'https?://[^\s<>"]+', caseSensitive: false);

String _trimUrl(String u) {
  var end = u.length;
  while (end > 0) {
    final c = u[end - 1];
    final closing = c == ')' && !u.substring(0, end).contains('(');
    if ('.,;:!?\'"]}'.contains(c) || closing) {
      end--;
    } else {
      break;
    }
  }
  return u.substring(0, end);
}

/// The first `http` or `https` URL in a draft: the one a chip is made for.
/// One link chip per draft.
String? firstUrl(String text) {
  for (final m in _urlPattern.allMatches(text)) {
    final u = _trimUrl(m.group(0)!);
    if (u.length > 8) return u;
  }
  return null;
}

/// A piece of message text, and whether it is a URL (drawn underlined in the
/// meta color; the text itself is left exactly as pasted).
typedef TextSpanPart = ({String text, bool isUrl});

List<TextSpanPart> urlParts(String text) {
  final parts = <TextSpanPart>[];
  var at = 0;
  for (final m in _urlPattern.allMatches(text)) {
    final u = _trimUrl(m.group(0)!);
    if (u.length <= 8) continue;
    if (m.start > at) parts.add((text: text.substring(at, m.start), isUrl: false));
    parts.add((text: u, isUrl: true));
    at = m.start + u.length;
  }
  if (at < text.length) parts.add((text: text.substring(at), isUrl: false));
  return parts;
}

/// The box a card's image is drawn in, known before any byte arrives: the
/// card's width times the stored height over width, clamped between 2:1 and
/// 1:1 (a portrait caps at square), and capped at [maxHeight].
double linkImageHeight({required double width, required int imageWidth, required int imageHeight, double maxHeight = 240}) {
  final ratio = (imageHeight / imageWidth).clamp(0.5, 1.0);
  final h = width * ratio;
  return h > maxHeight ? maxHeight : h;
}
