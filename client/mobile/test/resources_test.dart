import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:material_ui/material_ui.dart';
import 'package:zecurity_client/resources.dart';

void main() {
  test('copies address:port, or the address alone without a single port', () {
    expect(Resource('files', '192.168.1.46', 8088, 'tcp').copyText, '192.168.1.46:8088');
    expect(Resource('any', '10.0.0.5', 0, 'any').copyText, '10.0.0.5');
  });

  testWidgets('tapping a resource copies its address and says so', (tester) async {
    String? copied;
    tester.binding.defaultBinaryMessenger.setMockMethodCallHandler(SystemChannels.platform, (call) async {
      if (call.method == 'Clipboard.setData') {
        copied = (call.arguments as Map)['text'] as String?;
      }
      return null;
    });

    await tester.pumpWidget(MaterialApp(
      home: Scaffold(
        body: ResourceTile(resource: Resource('files-seechan', '192.168.1.46', 8088, 'tcp')),
      ),
    ));
    expect(find.text('192.168.1.46:8088 TCP'), findsOneWidget);

    await tester.tap(find.text('files-seechan'));
    await tester.pump();
    expect(copied, '192.168.1.46:8088');
    expect(find.text('Copied 192.168.1.46:8088'), findsOneWidget);

    copied = null;
    await tester.tap(find.byTooltip('Copy address'));
    await tester.pump();
    expect(copied, '192.168.1.46:8088');
  });
}
