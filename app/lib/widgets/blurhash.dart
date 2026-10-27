// A blurhash, decoded and drawn. The server computes the hash on ingest
// (`image_placeholder` on the wire); a client only draws it. It is CONTENT
// color, not a token: it is the page's own image, blurred.
//
// The decode is the reference algorithm (blurha.sh), at 32×32 and scaled up
// by the draw, as the canvas specifies. A hash that does not parse draws
// nothing, so the box below it still stands at its stored size.

import 'dart:math' as math;
import 'dart:typed_data';
import 'dart:ui' as ui;

import 'package:flutter/widgets.dart';

const _alphabet = '0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz#\$%*+,-.:;=?@[]^_{|}~';

int? _base83(String s, int from, int to) {
  var v = 0;
  for (var i = from; i < to; i++) {
    final d = _alphabet.indexOf(s[i]);
    if (d < 0) return null;
    v = v * 83 + d;
  }
  return v;
}

double _toLinear(int v) {
  final c = v / 255;
  return c <= 0.04045 ? c / 12.92 : math.pow((c + 0.055) / 1.055, 2.4).toDouble();
}

int _toSrgb(double v) {
  final c = v.clamp(0.0, 1.0);
  return ((c <= 0.0031308 ? c * 12.92 : 1.055 * math.pow(c, 1 / 2.4) - 0.055) * 255 + 0.5).toInt();
}

double _signPow(double v, double e) => (v < 0 ? -1 : 1) * math.pow(v.abs(), e).toDouble();

/// The RGBA pixels of [hash] at [size]×[size], or null when it is not a
/// well-formed blurhash.
Uint8List? decodeBlurhash(String hash, {int size = 32}) {
  if (hash.length < 6) return null;
  final flag = _base83(hash, 0, 1);
  if (flag == null) return null;
  final numY = flag ~/ 9 + 1, numX = flag % 9 + 1;
  if (hash.length != 4 + 2 * numX * numY) return null;
  final quant = _base83(hash, 1, 2);
  final dc = _base83(hash, 2, 6);
  if (quant == null || dc == null) return null;
  final maxVal = (quant + 1) / 166;
  final colors = <List<double>>[
    [_toLinear(dc >> 16), _toLinear((dc >> 8) & 255), _toLinear(dc & 255)],
  ];
  for (var i = 1; i < numX * numY; i++) {
    final ac = _base83(hash, 4 + i * 2, 6 + i * 2);
    if (ac == null) return null;
    colors.add([
      _signPow((ac ~/ (19 * 19) - 9) / 9, 2) * maxVal,
      _signPow(((ac ~/ 19) % 19 - 9) / 9, 2) * maxVal,
      _signPow((ac % 19 - 9) / 9, 2) * maxVal,
    ]);
  }
  final out = Uint8List(size * size * 4);
  for (var y = 0; y < size; y++) {
    for (var x = 0; x < size; x++) {
      var r = 0.0, g = 0.0, b = 0.0;
      for (var j = 0; j < numY; j++) {
        for (var i = 0; i < numX; i++) {
          final basis = math.cos(math.pi * x * i / size) * math.cos(math.pi * y * j / size);
          final c = colors[i + j * numX];
          r += c[0] * basis;
          g += c[1] * basis;
          b += c[2] * basis;
        }
      }
      final o = (y * size + x) * 4;
      out[o] = _toSrgb(r);
      out[o + 1] = _toSrgb(g);
      out[o + 2] = _toSrgb(b);
      out[o + 3] = 255;
    }
  }
  return out;
}

/// Fills its box with the decoded hash, scaled up smooth. [fallback] is what
/// shows until the decode lands, or when the hash is malformed.
class BlurhashFill extends StatefulWidget {
  const BlurhashFill({super.key, required this.hash, required this.fallback});

  final String hash;
  final Color fallback;

  @override
  State<BlurhashFill> createState() => _BlurhashFillState();
}

class _BlurhashFillState extends State<BlurhashFill> {
  ui.Image? _image;

  @override
  void initState() {
    super.initState();
    _decode();
  }

  @override
  void didUpdateWidget(BlurhashFill old) {
    super.didUpdateWidget(old);
    if (old.hash != widget.hash) _decode();
  }

  void _decode() {
    final hash = widget.hash;
    final pixels = decodeBlurhash(hash);
    if (pixels == null) return;
    ui.decodeImageFromPixels(pixels, 32, 32, ui.PixelFormat.rgba8888, (image) {
      if (!mounted || widget.hash != hash) {
        image.dispose();
        return;
      }
      setState(() {
        _image?.dispose();
        _image = image;
      });
    });
  }

  @override
  void dispose() {
    _image?.dispose();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) => ColoredBox(
        color: widget.fallback,
        child: _image == null ? const SizedBox.expand() : RawImage(image: _image, fit: BoxFit.fill, filterQuality: FilterQuality.medium),
      );
}
