/// `ConversationsApi` against a scripted `HttpFetch` (CANT-273): the requests
/// are the generated encoders', the answers the generated decoders', and a
/// refusal, an unreadable body and a dead network are three different things.
library;

import 'dart:convert';

import 'package:catenary_client/catenary_client.dart';
import 'package:catenary_wire/catenary_wire.dart';
import 'package:test/test.dart';

const _cred = Credential(userId: '00000000-0000-4000-8000-000000000001', deviceId: '00000000-0000-4000-8000-0000000000d1', accessToken: 'tok-1');
const _room = '00000000-0000-4000-8000-0000000000c1';

Map<String, dynamic> _conversation() => const Conversation(
      id: _room,
      kind: ConversationKind.direct,
      name: 'Nadia',
      memberCount: 2,
      headSeq: 0,
    ).toJson();

final class _Server {
  _Server(this.answer);

  final HttpAnswer Function(HttpExchange x, int n) answer;
  final requests = <HttpExchange>[];

  Future<HttpAnswer> fetch(HttpExchange x) async {
    requests.add(x);
    return answer(x, requests.length);
  }

  ConversationsApi api({CredentialSeam? credential}) =>
      ConversationsApi(baseUrl: 'https://catenary.test//', credential: credential ?? HeldCredential(_cred), fetch: fetch);
}

void main() {
  test('the roster is GET /users with the bearer token, decoded by the generated type', () async {
    final server = _Server((_, _) => HttpAnswer(
          200,
          jsonEncode({
            'users': [
              {'id': '00000000-0000-4000-8000-000000000002', 'name': 'Nadia Okonkwo', 'initials': 'NO', 'handle': 'nadia'},
            ],
          }),
        ));
    final r = await server.api().roster();
    final x = server.requests.single;
    expect(x.method, 'GET');
    expect(x.url.toString(), 'https://catenary.test/users');
    expect(x.headers['Authorization'], 'Bearer tok-1');
    expect(x.body, isNull);
    expect(((r as Started<List<RosterEntry>>).value).single.handle, 'nadia');
  });

  test('a direct posts DirectConversationRequest to /conversations/direct and says whether it made one', () async {
    final server = _Server((_, n) => HttpAnswer(n == 1 ? 200 : 201, jsonEncode(_conversation())));
    final found = await server.api().startDirect('nadia') as Started<Conversation>;
    final x = server.requests.single;
    expect(x.method, 'POST');
    expect(x.url.toString(), 'https://catenary.test/conversations/direct');
    expect(jsonDecode(x.body!), {'handle': 'nadia'});
    expect(found.value.id, _room);
    expect(found.created, isFalse);
  });

  test('a group posts CreateGroupRequest to /conversations, with the request_id when there is one', () async {
    final server = _Server((_, _) => HttpAnswer(201, jsonEncode(_conversation())));
    final r = await server.api().createGroup('Kitchen', ['nadia', 'ilse'], requestId: '00000000-0000-4000-8000-0000000000a1') as Started<Conversation>;
    final x = server.requests.single;
    expect(x.url.toString(), 'https://catenary.test/conversations');
    expect(jsonDecode(x.body!), {'name': 'Kitchen', 'member_handles': ['nadia', 'ilse'], 'request_id': '00000000-0000-4000-8000-0000000000a1'});
    expect(r.created, isTrue);
  });

  test('a refusal carries its status and the wire code', () async {
    final server = _Server((_, _) => HttpAnswer(404, jsonEncode({'code': 'conversation_not_found', 'error': 'x'})));
    final r = await server.api().startDirect('nobody') as StartRefused<Conversation>;
    expect(r.status, 404);
    expect(r.code, 'conversation_not_found');
  });

  test('a 5xx, an interception page and a body that does not decode are unreachable, never a refusal or a success', () async {
    expect(await _Server((_, _) => const HttpAnswer(502, '<html>bad gateway</html>')).api().startDirect('a'), isA<StartUnreachable<Conversation>>());
    expect(await _Server((_, _) => const HttpAnswer(200, '<html>login</html>')).api().startDirect('a'), isA<StartUnreachable<Conversation>>());
    expect(await _Server((_, _) => const HttpAnswer(200, '{"id":"nope"}')).api().startDirect('a'), isA<StartUnreachable<Conversation>>());
    final dead = ConversationsApi(baseUrl: 'https://catenary.test', credential: HeldCredential(_cred), fetch: (_) => throw Exception('offline'));
    expect(await dead.roster(), isA<StartUnreachable<List<RosterEntry>>>());
  });

  test('Catenary\'s own 401 asks the credential to refresh and retries once; a second one gives up', () async {
    final refreshing = _Refreshing();
    final server = _Server((x, n) => x.headers['Authorization'] == 'Bearer tok-2'
        ? HttpAnswer(200, jsonEncode(_conversation()))
        : const HttpAnswer(401, '{"code":"unauthorized"}'));
    expect(await server.api(credential: refreshing).startDirect('nadia'), isA<Started<Conversation>>());
    expect(server.requests.map((x) => x.headers['Authorization']), ['Bearer tok-1', 'Bearer tok-2']);

    final never = _Server((_, _) => const HttpAnswer(401, '{"code":"unauthorized"}'));
    expect(await never.api(credential: _Refreshing(always: true)).startDirect('nadia'), isA<StartUnreachable<Conversation>>());
    expect(never.requests, hasLength(2));
  });
}

final class _Refreshing implements CredentialSeam {
  _Refreshing({this.always = false});

  final bool always;
  var _token = 'tok-1';

  @override
  void attach(CredentialHost host) {}
  @override
  Future<Credential> current() async => Credential(userId: _cred.userId, deviceId: _cred.deviceId, accessToken: _token);
  @override
  Future<bool> withholdDial() async => false;
  @override
  Future<bool> withholdSync() async => false;
  @override
  bool get refused => false;
  @override
  Future<void> beforeDial() async {}
  @override
  Future<void> refreshIfDue() async {}
  @override
  Future<bool> onSyncUnauthorized(Credential presented) async {
    if (!always) _token = 'tok-2';
    return true;
  }

  @override
  void answered() {}
  @override
  CredentialStatus status() => const CredentialStatus();
}
