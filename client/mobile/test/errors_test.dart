import 'package:flutter_test/flutter_test.dart';
import 'package:zecurity_client/errors.dart';

void main() {
  test('the raw daemon-socket error from the live run is explained', () {
    const raw = 'connect to daemon socket /data/user/0/io.zecurity.client/files/'
        'daemon.sock: Connection refused (os error 111)';
    expect(isServiceDown(raw), isTrue);
    final text = explainError(raw);
    expect(text, contains('service'));
    expect(text, isNot(contains('os error')));
    expect(text, isNot(contains('daemon.sock')));
  });

  test('a missing socket also means the service is down', () {
    expect(isServiceDown('connect to daemon socket /x/daemon.sock: No such file or directory (os error 2)'), isTrue);
  });

  test('a controller connection failure is not mistaken for the service', () {
    const raw = 'connect to controller: transport error: tcp connect error: Connection refused (os error 111)';
    expect(isServiceDown(raw), isFalse);
    expect(explainError(raw), contains('controller'));
  });

  test('VPN, revocation, settings and certificate errors get specific advice', () {
    expect(explainError('failed to configure split-tunnel routes: the VPN interface could not be created'),
        contains('VPN'));
    expect(explainError('device state is revoked'), contains('revoked'));
    expect(explainError('Not configured. Run `zecurity-client setup --workspace <name>` first.'),
        contains('Settings'));
    expect(explainError('invalid peer certificate: UnknownIssuer'), contains('certificate'));
  });

  test('anything else falls back to a generic message, keeping the details', () {
    final note = Note.error('weird internal failure 42');
    expect(note.isError, isTrue);
    expect(note.text, contains('Details'));
    expect(note.details, 'weird internal failure 42');
    expect(explainError(null), isNotEmpty);
  });
}
