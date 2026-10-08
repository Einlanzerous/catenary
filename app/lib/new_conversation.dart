// Starting a conversation (CANT-253's app half, CANT-273). Neither canvas draws
// this flow, so it follows the rail and the enrollment form's styling and
// invents no new token: one person starts a direct, two or more reveal a name
// field and start a room. The roster is `GET /users`; what is on this screen
// is only what the server listed.
//
// THE CONTROL IS DISABLED WHILE A START IS IN FLIGHT, and a failure is a
// visible, retryable line under the form: a refusal says the server said no,
// an unreachable answer says nothing is known and asking again is safe. A
// room's `request_id` is minted per form state, so retrying the same form is
// a replay and editing it is a new room.
//
// Nothing here says who can read what: the thread header's transport word is
// derived from the origin where the thread is drawn, and not repeated here.

import 'package:flutter/material.dart';

import 'metrics.dart';
import 'store/start.dart';
import 'tokens.dart';

/// The longest a room's name may be, as the wire states it (1 to 80).
const roomNameMax = 80;

class NewConversationScreen extends StatefulWidget {
  const NewConversationScreen({
    super.key,
    required this.loadRoster,
    required this.onDirect,
    required this.onGroup,
    required this.onDone,
    this.onCancel,
  });

  /// `GET /users`. Null is a roster that could not be had.
  final Future<List<RosterPerson>?> Function() loadRoster;

  /// One person picked: the direct with them.
  final Future<StartOutcome> Function(RosterPerson person) onDirect;

  /// Two or more picked: a room named [name] with them, under [requestId].
  final Future<StartOutcome> Function(String name, List<RosterPerson> people, String requestId) onGroup;

  /// A start succeeded. [held] is whether the journal already has the
  /// conversation, which is whether it can be opened now.
  final void Function(String conversationId, bool held) onDone;
  final VoidCallback? onCancel;

  @override
  State<NewConversationScreen> createState() => _NewConversationScreenState();
}

class _NewConversationScreenState extends State<NewConversationScreen> {
  final _name = TextEditingController();
  final _picked = <String>[];
  List<RosterPerson>? _roster;
  var _loading = true;
  var _busy = false;
  String? _error;
  var _requestId = newRequestId();

  @override
  void initState() {
    super.initState();
    _name.addListener(_edited);
    _load();
  }

  @override
  void dispose() {
    _name.dispose();
    super.dispose();
  }

  Future<void> _load() async {
    setState(() => _loading = true);
    List<RosterPerson>? roster;
    try {
      roster = await widget.loadRoster();
    } on Object {
      roster = null;
    }
    if (!mounted) return;
    setState(() {
      _roster = roster;
      _loading = false;
    });
  }

  /// A changed form is a different room: its `request_id` is new.
  void _edited() {
    if (_busy) return;
    setState(() {
      _requestId = newRequestId();
      _error = null;
    });
  }

  List<RosterPerson> get _people => [
        for (final id in _picked)
          for (final p in _roster ?? const <RosterPerson>[])
            if (p.id == id) p,
      ];

  bool get _isGroup => _picked.length >= 2;

  bool get _ready => !_busy && (_isGroup ? _name.text.trim().isNotEmpty : _picked.length == 1);

  void _toggle(RosterPerson p) {
    if (_busy) return;
    setState(() {
      _picked.contains(p.id) ? _picked.remove(p.id) : _picked.add(p.id);
      _requestId = newRequestId();
      _error = null;
    });
  }

  Future<void> _submit() async {
    final people = _people;
    setState(() {
      _busy = true;
      _error = null;
    });
    StartOutcome outcome;
    try {
      outcome = people.length >= 2 ? await widget.onGroup(_name.text.trim(), people, _requestId) : await widget.onDirect(people.single);
    } on Object {
      outcome = const StartUnreachableNow();
    }
    if (!mounted) return;
    switch (outcome) {
      case StartDone(:final conversationId, :final held):
        setState(() => _busy = false);
        widget.onDone(conversationId, held);
      case StartRefusedBy(:final code):
        setState(() {
          _busy = false;
          _error = code == 'conversation_not_found'
              ? 'The server could not find one of them. They may no longer be active. Nothing was created.'
              : 'The server refused this. Nothing was created.';
        });
      case StartUnreachableNow():
        setState(() {
          _busy = false;
          _error = 'Could not reach the server. Try again: asking twice does not make two.';
        });
    }
  }

  @override
  Widget build(BuildContext context) {
    final t = CatenaryTokens.of(context);
    final mono = TextStyle(fontFamily: fontMono, color: t.textMeta);
    final roster = _roster;
    return Scaffold(
      backgroundColor: t.surfaceBase,
      body: SafeArea(
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.stretch,
          children: [
            Container(
              height: 48,
              decoration: BoxDecoration(color: t.surfaceRail, border: Border(bottom: BorderSide(color: t.lineHair))),
              child: Row(
                children: [
                  GestureDetector(
                    key: const ValueKey('new-back'),
                    behavior: HitTestBehavior.opaque,
                    onTap: _busy ? null : (widget.onCancel ?? () => Navigator.of(context).maybePop()),
                    child: SizedBox(
                      width: 44,
                      height: 48,
                      child: Center(child: Text('‹', style: mono.copyWith(fontSize: 13, color: t.accentWire))),
                    ),
                  ),
                  Text('NEW CONVERSATION', style: mono.copyWith(fontSize: 12, letterSpacing: 2.4, color: t.textPrimary)),
                ],
              ),
            ),
            Expanded(
              child: _loading
                  ? Center(child: Text('LOADING…', key: const ValueKey('new-loading'), style: mono.copyWith(fontSize: 10, letterSpacing: 1.6)))
                  : roster == null
                      ? _RosterFailed(onRetry: _load)
                      : roster.isEmpty
                          ? Center(
                              child: Padding(
                                padding: const EdgeInsets.all(CatenaryMetrics.s6),
                                child: Text(
                                  'Nobody else is signed up on this server yet.',
                                  key: const ValueKey('new-empty'),
                                  textAlign: TextAlign.center,
                                  style: CatenaryType.secondary.style.copyWith(color: t.textSecondary),
                                ),
                              ),
                            )
                          : ListView(
                              padding: EdgeInsets.zero,
                              children: [
                                Padding(
                                  padding: const EdgeInsets.fromLTRB(CatenaryMetrics.s4, 14, CatenaryMetrics.s4, 6),
                                  child: Text(
                                    'PEOPLE · PICK ONE FOR A DIRECT, TWO OR MORE FOR A ROOM',
                                    style: TextStyle(fontFamily: fontMono, fontSize: 10, height: 1.2, letterSpacing: 1.6, color: t.textMeta),
                                  ),
                                ),
                                for (final p in roster) _PersonRow(person: p, picked: _picked.contains(p.id), onTap: () => _toggle(p)),
                              ],
                            ),
            ),
            if (roster != null && roster.isNotEmpty)
              Container(
                decoration: BoxDecoration(color: t.surfaceRail, border: Border(top: BorderSide(color: t.lineHair))),
                padding: const EdgeInsets.all(CatenaryMetrics.s4),
                child: Column(
                  crossAxisAlignment: CrossAxisAlignment.stretch,
                  children: [
                    if (_isGroup) ...[
                      Text('ROOM NAME', style: CatenaryType.label.tracked.copyWith(color: t.textMeta)),
                      const SizedBox(height: CatenaryMetrics.s2),
                      TextField(
                        key: const ValueKey('new-room-name'),
                        controller: _name,
                        enabled: !_busy,
                        maxLength: roomNameMax,
                        buildCounter: (_, {required currentLength, required isFocused, maxLength}) => null,
                        onSubmitted: (_) => _ready ? _submit() : null,
                        style: CatenaryType.body.style.copyWith(color: t.textPrimary),
                        decoration: const InputDecoration(hintText: 'Kitchen Table'),
                      ),
                      const SizedBox(height: CatenaryMetrics.s3),
                    ],
                    if (_error != null) ...[
                      Text(_error!, key: const ValueKey('new-error'), style: CatenaryType.secondary.style.copyWith(color: t.signalFault)),
                      const SizedBox(height: CatenaryMetrics.s3),
                    ],
                    FilledButton(
                      key: const ValueKey('new-submit'),
                      onPressed: _ready ? _submit : null,
                      child: Text(
                        _busy ? 'STARTING…' : (_isGroup ? 'CREATE ROOM · ${_picked.length + 1} MEMBERS' : 'START DIRECT'),
                        style: CatenaryType.label.tracked.copyWith(color: t.onAccent),
                      ),
                    ),
                  ],
                ),
              ),
          ],
        ),
      ),
    );
  }
}

class _RosterFailed extends StatelessWidget {
  const _RosterFailed({required this.onRetry});

  final VoidCallback onRetry;

  @override
  Widget build(BuildContext context) {
    final t = CatenaryTokens.of(context);
    return Center(
      child: Padding(
        padding: const EdgeInsets.all(CatenaryMetrics.s6),
        child: Column(
          mainAxisSize: MainAxisSize.min,
          children: [
            Text(
              'Could not load the people on this server.',
              key: const ValueKey('new-roster-failed'),
              textAlign: TextAlign.center,
              style: CatenaryType.secondary.style.copyWith(color: t.signalFault),
            ),
            const SizedBox(height: CatenaryMetrics.s4),
            TextButton(
              key: const ValueKey('new-roster-retry'),
              onPressed: onRetry,
              child: Text('RETRY', style: CatenaryType.label.tracked),
            ),
          ],
        ),
      ),
    );
  }
}

class _PersonRow extends StatelessWidget {
  const _PersonRow({required this.person, required this.picked, required this.onTap});

  final RosterPerson person;
  final bool picked;
  final VoidCallback onTap;

  @override
  Widget build(BuildContext context) {
    final t = CatenaryTokens.of(context);
    return GestureDetector(
      key: ValueKey('person-${person.id}'),
      behavior: HitTestBehavior.opaque,
      onTap: onTap,
      child: Container(
        height: 56,
        padding: const EdgeInsets.symmetric(horizontal: CatenaryMetrics.s4),
        child: Row(
          children: [
            Container(
              width: 32,
              height: 32,
              alignment: Alignment.center,
              color: t.surfaceAvatar,
              child: Text(person.tile, style: TextStyle(fontFamily: fontMono, fontSize: 11, letterSpacing: 1.1, color: t.textPrimary)),
            ),
            const SizedBox(width: 12),
            Expanded(
              child: Text(
                person.name,
                maxLines: 1,
                overflow: TextOverflow.ellipsis,
                style: TextStyle(fontFamily: fontSans, fontSize: 14, height: 19 / 14, fontWeight: FontWeight.w600, color: t.textPrimary),
              ),
            ),
            Text(
              '@${person.handle}',
              style: TextStyle(fontFamily: fontMono, fontSize: 10.5, color: t.textMeta),
            ),
            const SizedBox(width: 12),
            // A square, not copper: picking is not one of the accent's four meanings.
            Container(
              key: ValueKey('picked-${person.id}'),
              width: 16,
              height: 16,
              decoration: BoxDecoration(
                color: picked ? t.textPrimary : null,
                border: Border.all(color: picked ? t.textPrimary : t.lineInner),
              ),
              child: picked ? Center(child: Container(width: 6, height: 6, color: t.surfaceBase)) : null,
            ),
          ],
        ),
      ),
    );
  }
}
