// A resource the device may reach, and its row in the resource list.
// Tapping the row copies the address to paste into a browser or app.

import 'package:flutter/services.dart';
import 'package:material_ui/material_ui.dart';

class Resource {
  Resource(this.name, this.address, this.port, this.protocol);

  final String name;
  final String address;
  final int port;
  final String protocol;

  /// What tapping the row copies: `address:port`, or the address alone when
  /// the resource has no single port.
  String get copyText => port > 0 ? '$address:$port' : address;
}

class ResourceTile extends StatelessWidget {
  const ResourceTile({super.key, required this.resource});

  final Resource resource;

  Future<void> _copy(BuildContext context) async {
    await Clipboard.setData(ClipboardData(text: resource.copyText));
    if (!context.mounted) return;
    ScaffoldMessenger.of(context)
      ..hideCurrentSnackBar()
      ..showSnackBar(
        SnackBar(
          content: Text('Copied ${resource.copyText}'),
          duration: const Duration(seconds: 2),
          behavior: SnackBarBehavior.floating,
        ),
      );
  }

  @override
  Widget build(BuildContext context) {
    final r = resource;
    return ListTile(
      contentPadding: .zero,
      leading: const Icon(Icons.dns_outlined),
      title: Text(r.name.isEmpty ? r.address : r.name),
      subtitle: Text('${r.address}${r.port > 0 ? ':${r.port}' : ''} ${r.protocol.toUpperCase()}'),
      trailing: IconButton(
        tooltip: 'Copy address',
        icon: const Icon(Icons.copy_outlined),
        onPressed: () => _copy(context),
      ),
      onTap: () => _copy(context),
    );
  }
}
