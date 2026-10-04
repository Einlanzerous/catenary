// The enrollment screen: what a device that holds no credential shows first,
// and what RE-ENROLL on a credential terminal opens. The twin of the login
// form in web/src/components/AccountView.vue.
//
// RULING 0 → TWO FIELDS THAT ALSO ACCEPT A LINK. A server address and an
// enrollment token, and a device name beside them. Pasting
// `https://<host>/#enroll=<token>` into either of the first two fills both
// (`parseEnrollLink`); a string that is not that shape stays where it was
// pasted. RULING 1: no build carries an address, so a first enrollment starts
// with the field empty. On a RE-ENROLL the address is the one this device
// already belongs to: shown, and not editable (RULING 3).
//
// NOTHING HERE TOUCHES THE CLIENT PACKAGE. The screen is handed [onSubmit],
// which the store answers (`AppStore.enroll` / `AppStore.reenroll`), and shows
// whatever text comes back. Nothing is claimed about the connection: a token
// is spent over whatever the address says, and the thread header says `TLS`
// or `CLEARTEXT` from the same address once there is a session.

import 'package:flutter/material.dart';

import 'metrics.dart';
import 'store/enrollment.dart';
import 'tokens.dart';
import 'widgets/logomark.dart';

/// Submits the form's trimmed contents. Completes with what came of it.
typedef EnrollSubmit = Future<EnrollOutcome> Function(String address, String token, String deviceName);

class EnrollScreen extends StatefulWidget {
  const EnrollScreen({
    super.key,
    required this.onSubmit,
    required this.deviceName,
    this.lockedAddress,
    this.onCancel,
  });

  final EnrollSubmit onSubmit;

  /// What the device-name field starts as (`defaultDeviceName`).
  final String deviceName;

  /// Set on a RE-ENROLL: the address the device belongs to, shown and not
  /// editable. Null on a first enrollment, where the field starts empty.
  final String? lockedAddress;

  /// Set on a RE-ENROLL, which can be backed out of; a first enrollment has
  /// nowhere to go back to.
  final VoidCallback? onCancel;

  @override
  State<EnrollScreen> createState() => _EnrollScreenState();
}

class _EnrollScreenState extends State<EnrollScreen> {
  late final _address = TextEditingController(text: widget.lockedAddress ?? '');
  final _token = TextEditingController();
  late final _name = TextEditingController(text: widget.deviceName);
  String? _error;
  var _busy = false;
  var _filling = false;

  bool get _locked => widget.lockedAddress != null;

  @override
  void initState() {
    super.initState();
    _address.addListener(_pasted);
    _token.addListener(_pasted);
  }

  @override
  void dispose() {
    _address.dispose();
    _token.dispose();
    _name.dispose();
    super.dispose();
  }

  /// A link in either field fills both. On a locked address the origin is
  /// ignored and only the token is taken.
  void _pasted() {
    if (_filling || !mounted) return;
    setState(() {});
    final link = parseEnrollLink(_address.text) ?? parseEnrollLink(_token.text);
    if (link == null) return;
    _filling = true;
    if (!_locked) _address.text = link.origin;
    _token.text = link.token;
    _filling = false;
  }

  bool get _ready => !_busy && _address.text.trim().isNotEmpty && _token.text.trim().isNotEmpty && _name.text.trim().isNotEmpty;

  Future<void> _submit() async {
    setState(() {
      _busy = true;
      _error = null;
    });
    final outcome = await widget.onSubmit(_address.text.trim(), _token.text.trim(), _name.text.trim());
    if (!mounted) return;
    setState(() {
      _busy = false;
      if (outcome is EnrollFailed) _error = outcome.text;
    });
  }

  @override
  Widget build(BuildContext context) {
    final t = CatenaryTokens.of(context);
    final label = CatenaryType.label.tracked.copyWith(color: t.textMeta);
    Widget field(String name, TextEditingController c, {String? hint, bool readOnly = false, TextInputType? type, Key? key}) => Column(
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            Text(name, style: label),
            const SizedBox(height: CatenaryMetrics.s2),
            TextField(
              key: key,
              controller: c,
              readOnly: readOnly || _busy,
              enabled: !readOnly,
              keyboardType: type,
              autocorrect: false,
              enableSuggestions: false,
              onSubmitted: (_) => _ready ? _submit() : null,
              style: CatenaryType.body.style.copyWith(color: readOnly ? t.textDim : t.textPrimary),
              decoration: InputDecoration(hintText: hint),
            ),
          ],
        );
    return Scaffold(
      backgroundColor: t.surfaceBase,
      body: SafeArea(
        child: Center(
          child: SingleChildScrollView(
            padding: const EdgeInsets.all(CatenaryMetrics.s6),
            child: ConstrainedBox(
              constraints: const BoxConstraints(maxWidth: 420),
              child: Column(
                crossAxisAlignment: CrossAxisAlignment.start,
                children: [
                  const Logomark(size: 28),
                  const SizedBox(height: CatenaryMetrics.s6),
                  Text(_locked ? 'Re-enroll this device' : 'Enroll this device',
                      style: CatenaryType.display.style.copyWith(color: t.textPrimary)),
                  const SizedBox(height: CatenaryMetrics.s2),
                  Text(
                    _locked
                        ? 'This device is no longer signed in. Enter a fresh enrollment token for the server below. Messages waiting to send stay on this device, and send only from the account that wrote them.'
                        : 'Enter the server address and the enrollment token you were given, or paste the invitation link into either field. Name this device — a revocation list only helps if you can tell its entries apart.',
                    style: CatenaryType.secondary.style.copyWith(color: t.textSecondary),
                  ),
                  const SizedBox(height: CatenaryMetrics.s6),
                  field('SERVER ADDRESS', _address,
                      hint: 'chat.example.com', readOnly: _locked, type: TextInputType.url, key: const Key('enroll-address')),
                  const SizedBox(height: CatenaryMetrics.s4),
                  field('ENROLLMENT TOKEN', _token, hint: 'paste the token you were sent', key: const Key('enroll-token')),
                  const SizedBox(height: CatenaryMetrics.s4),
                  field('DEVICE NAME', _name, key: const Key('enroll-name')),
                  if (_error != null) ...[
                    const SizedBox(height: CatenaryMetrics.s4),
                    Text(_error!, key: const Key('enroll-error'), style: CatenaryType.secondary.style.copyWith(color: t.signalFault)),
                  ],
                  const SizedBox(height: CatenaryMetrics.s6),
                  Row(children: [
                    FilledButton(
                      key: const Key('enroll-submit'),
                      onPressed: _ready ? _submit : null,
                      child: Text(_busy ? 'ENROLLING…' : (_locked ? 'RE-ENROLL DEVICE' : 'ENROLL DEVICE'), style: CatenaryType.label.tracked.copyWith(color: t.onAccent)),
                    ),
                    if (widget.onCancel != null) ...[
                      const SizedBox(width: CatenaryMetrics.s3),
                      TextButton(
                        key: const Key('enroll-cancel'),
                        onPressed: _busy ? null : widget.onCancel,
                        child: Text('CANCEL', style: CatenaryType.label.tracked),
                      ),
                    ],
                  ]),
                ],
              ),
            ),
          ),
        ),
      ),
    );
  }
}
