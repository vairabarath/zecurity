// Zecurity mobile client: the Android and iOS counterpart of
// `zecurity-client`'s CLI. The daemon runs in the app's VPN service process
// (iOS: its packet tunnel extension); this UI talks to it through the client
// core (native.dart), the way the CLI does on Linux.

import 'dart:async';

import 'package:material_ui/material_ui.dart';

import 'host.dart';
import 'native.dart';

void main() {
  runApp(const ZecurityApp());
}

class ZecurityApp extends StatelessWidget {
  const ZecurityApp({super.key});

  @override
  Widget build(BuildContext context) {
    return MaterialApp(
      title: 'Zecurity',
      theme: ThemeData(colorScheme: .fromSeed(seedColor: const Color(0xFF1F5FBF))),
      darkTheme: ThemeData(
        colorScheme: .fromSeed(seedColor: const Color(0xFF1F5FBF), brightness: .dark),
      ),
      home: const HomePage(),
    );
  }
}

class Resource {
  Resource(this.name, this.address, this.port, this.protocol);

  final String name;
  final String address;
  final int port;
  final String protocol;
}

class HomePage extends StatefulWidget {
  const HomePage({super.key});

  @override
  State<HomePage> createState() => _HomePageState();
}

class _HomePageState extends State<HomePage> {
  bool _ready = false;
  bool _busy = false;
  String? _message;

  // Config
  String _workspace = '';
  String _controller = '';
  String _httpBase = '';

  // Daemon status
  bool _daemonUp = false;
  String? _daemonError;
  String? _email;
  String? _deviceId;
  String? _deviceState;
  bool _tunnelUp = false;
  String? _tunnelRestoring;
  String? _dnsNames;
  int? _certExpiresAt;
  List<Resource> _resources = [];

  Timer? _poll;

  @override
  void initState() {
    super.initState();
    _start();
  }

  @override
  void dispose() {
    _poll?.cancel();
    super.dispose();
  }

  Future<void> _start() async {
    final dir = await Host.filesDir();
    await Core.init(dir);
    await Host.startService();
    await _loadConfig();
    setState(() => _ready = true);
    await _refresh();
    _poll = Timer.periodic(const Duration(seconds: 3), (_) => _refresh());
  }

  Future<void> _loadConfig() async {
    final c = await Core.config();
    _workspace = c.str('workspace') ?? '';
    _controller = c.str('controller') ?? '';
    _httpBase = c.str('http_base') ?? '';
  }

  Future<void> _refresh() async {
    if (_busy) return;
    final s = await Core.request('Status');
    if (!mounted) return;
    if (!s.ok) {
      setState(() {
        _daemonUp = false;
        _daemonError = s.error;
        _tunnelUp = false;
      });
      return;
    }
    List<Resource> resources = _resources;
    if (s.str('email') != null) {
      final r = await Core.request('Resources');
      if (r.ok) {
        resources = ((r.json['resources'] as List?) ?? [])
            .map((e) => e as Map<String, dynamic>)
            .map((e) => Resource(
                  e['name'] as String? ?? '',
                  e['address'] as String? ?? '',
                  (e['port'] as num?)?.toInt() ?? 0,
                  e['protocol'] as String? ?? '',
                ))
            .toList();
      }
    } else {
      resources = [];
    }
    if (!mounted) return;
    setState(() {
      _daemonUp = true;
      _daemonError = null;
      _email = s.str('email');
      _deviceId = s.str('device_id');
      _deviceState = s.str('device_state');
      _tunnelUp = s.flag('tunnel_up') ?? false;
      _tunnelRestoring = s.str('tunnel_restoring');
      _dnsNames = s.str('dns_names');
      _certExpiresAt = s.integer('cert_expires_at');
      _resources = resources;
    });
  }

  Future<void> _run(String label, Future<String?> Function() action) async {
    setState(() {
      _busy = true;
      _message = label;
    });
    String? result;
    try {
      result = await action();
    } catch (e) {
      result = '$e';
    }
    if (!mounted) return;
    setState(() {
      _busy = false;
      _message = result;
    });
    await _refresh();
  }

  Future<void> _connect() => _run('Connecting…', () async {
        if (!await Host.prepareVpn()) return 'VPN permission was not granted.';
        final r = await Core.request('Up');
        if (!r.ok) return r.error ?? 'Could not connect.';
        return r.integer('synced_resources') == 0
            ? 'Connected. No resources are assigned to this device yet.'
            : 'Connected.';
      });

  Future<void> _disconnect() => _run('Disconnecting…', () async {
        final r = await Core.request('Down');
        return r.ok ? 'Disconnected.' : (r.error ?? 'Could not disconnect.');
      });

  Future<void> _sync() => _run('Syncing…', () async {
        final r = await Core.request('Sync');
        return r.ok ? 'Resources are up to date.' : (r.error ?? 'Sync failed.');
      });

  Future<void> _login() => _run('Signing in…', () async {
        final start = await Core.loginStart();
        if (!start.ok) return start.error ?? 'Could not start sign-in.';
        for (var i = 0; i < 400; i++) {
          final p = await Core.loginPoll();
          final url = p.str('url');
          if (url != null) await Host.openUrl(url);
          switch (p.str('state')) {
            case 'done':
              await Host.closeBrowser();
              return 'Signed in.';
            case 'error':
              await Host.closeBrowser();
              return p.error ?? 'Sign-in failed.';
          }
          await Future<void>.delayed(const Duration(milliseconds: 750));
        }
        return 'Sign-in timed out.';
      });

  Future<void> _logout() => _run('Signing out…', () async {
        final r = await Core.logout();
        return r.ok ? 'Signed out.' : (r.error ?? 'Sign-out failed.');
      });

  Future<void> _editSettings() async {
    final workspace = TextEditingController(text: _workspace);
    final controller = TextEditingController(text: _controller);
    final httpBase = TextEditingController(text: _httpBase);
    final saved = await showModalBottomSheet<bool>(
      context: context,
      isScrollControlled: true,
      builder: (context) => Padding(
        padding: EdgeInsets.only(
          left: 16,
          right: 16,
          top: 16,
          bottom: MediaQuery.of(context).viewInsets.bottom + 16,
        ),
        child: Column(
          mainAxisSize: .min,
          crossAxisAlignment: .stretch,
          children: [
            Text('Workspace settings', style: Theme.of(context).textTheme.titleLarge),
            const SizedBox(height: 12),
            TextField(
              controller: workspace,
              decoration: const InputDecoration(labelText: 'Workspace', hintText: 'acme'),
            ),
            const SizedBox(height: 8),
            TextField(
              controller: controller,
              decoration: const InputDecoration(
                labelText: 'Controller address (dev only)',
                hintText: 'host:9090',
              ),
            ),
            const SizedBox(height: 8),
            TextField(
              controller: httpBase,
              decoration: const InputDecoration(
                labelText: 'Controller HTTP base (dev only)',
                hintText: 'http://host:8080',
              ),
            ),
            const SizedBox(height: 16),
            FilledButton(
              onPressed: () => Navigator.of(context).pop(true),
              child: const Text('Save'),
            ),
          ],
        ),
      ),
    );
    if (saved != true) return;
    await _run('Saving…', () async {
      final r = await Core.setup(
        workspace: workspace.text,
        controller: controller.text,
        httpBase: httpBase.text,
      );
      if (!r.ok) return r.error ?? 'Could not save settings.';
      await _loadConfig();
      // The daemon reads its config when it starts.
      await Host.restartService();
      await Future<void>.delayed(const Duration(seconds: 2));
      return 'Settings saved.';
    });
  }

  @override
  Widget build(BuildContext context) {
    final theme = Theme.of(context);
    final signedIn = _email != null;
    return Scaffold(
      appBar: AppBar(
        title: const Text('Zecurity'),
        actions: [
          IconButton(
            tooltip: 'Settings',
            icon: const Icon(Icons.settings_outlined),
            onPressed: _busy || !_ready ? null : _editSettings,
          ),
          PopupMenuButton<String>(
            enabled: !_busy && signedIn,
            onSelected: (v) => v == 'sync' ? _sync() : _logout(),
            itemBuilder: (context) => const [
              PopupMenuItem(value: 'sync', child: Text('Sync resources')),
              PopupMenuItem(value: 'logout', child: Text('Sign out')),
            ],
          ),
        ],
      ),
      body: !_ready
          ? const Center(child: CircularProgressIndicator())
          : RefreshIndicator(
              onRefresh: _refresh,
              child: ListView(
                padding: const EdgeInsets.all(16),
                children: [
                  _statusCard(theme, signedIn),
                  if (_message != null) ...[
                    const SizedBox(height: 12),
                    Text(_message!, style: theme.textTheme.bodyMedium),
                  ],
                  const SizedBox(height: 16),
                  if (signedIn) ..._resourceList(theme),
                ],
              ),
            ),
    );
  }

  Widget _statusCard(ThemeData theme, bool signedIn) {
    final scheme = theme.colorScheme;
    final String title;
    final String subtitle;
    if (_workspace.isEmpty) {
      title = 'Not set up';
      subtitle = 'Enter your workspace in Settings.';
    } else if (!_daemonUp) {
      title = 'Service not running';
      subtitle = _daemonError ?? 'Starting…';
    } else if (_deviceState == 'revoked') {
      title = 'Device revoked';
      subtitle = 'An administrator revoked this device. Sign in again.';
    } else if (!signedIn) {
      title = 'Signed out';
      subtitle = 'Workspace: $_workspace';
    } else {
      title = _tunnelUp ? 'Connected' : 'Disconnected';
      subtitle = '$_email · $_workspace';
    }
    return Card(
      child: Padding(
        padding: const EdgeInsets.all(16),
        child: Column(
          crossAxisAlignment: .start,
          children: [
            Row(
              children: [
                Icon(
                  _tunnelUp ? Icons.lock : Icons.lock_open,
                  color: _tunnelUp ? scheme.primary : scheme.outline,
                  size: 32,
                ),
                const SizedBox(width: 12),
                Expanded(
                  child: Column(
                    crossAxisAlignment: .start,
                    children: [
                      Text(title, style: theme.textTheme.titleLarge),
                      Text(subtitle, style: theme.textTheme.bodyMedium),
                    ],
                  ),
                ),
              ],
            ),
            if (_tunnelRestoring != null) ...[
              const SizedBox(height: 8),
              Text(
                _tunnelRestoring!.isEmpty
                    ? 'Restoring the tunnel…'
                    : 'Restoring the tunnel: $_tunnelRestoring',
              ),
            ],
            if (_dnsNames != null) ...[
              const SizedBox(height: 8),
              Text('DNS names: $_dnsNames'),
            ],
            if (signedIn && _deviceId != null) ...[
              const SizedBox(height: 8),
              Text('Device $_deviceId', style: theme.textTheme.bodySmall),
              if (_certExpiresAt != null)
                Text(
                  'Certificate valid until ${DateTime.fromMillisecondsSinceEpoch(_certExpiresAt! * 1000).toLocal()}',
                  style: theme.textTheme.bodySmall,
                ),
            ],
            const SizedBox(height: 16),
            SizedBox(
              width: double.infinity,
              child: _primaryButton(signedIn),
            ),
          ],
        ),
      ),
    );
  }

  Widget _primaryButton(bool signedIn) {
    if (_workspace.isEmpty) {
      return FilledButton(onPressed: _busy ? null : _editSettings, child: const Text('Set up'));
    }
    if (!_daemonUp) {
      return OutlinedButton(
        onPressed: _busy ? null : () => _run('Starting…', () async {
          await Host.restartService();
          await Future<void>.delayed(const Duration(seconds: 2));
          return null;
        }),
        child: const Text('Start service'),
      );
    }
    if (!signedIn || _deviceState == 'revoked') {
      return FilledButton(onPressed: _busy ? null : _login, child: const Text('Sign in'));
    }
    return _tunnelUp
        ? OutlinedButton(onPressed: _busy ? null : _disconnect, child: const Text('Disconnect'))
        : FilledButton(onPressed: _busy ? null : _connect, child: const Text('Connect'));
  }

  List<Widget> _resourceList(ThemeData theme) {
    return [
      Text('Resources', style: theme.textTheme.titleMedium),
      const SizedBox(height: 8),
      if (_resources.isEmpty)
        const Text('No resources are assigned to this device.')
      else
        for (final r in _resources)
          ListTile(
            contentPadding: .zero,
            leading: const Icon(Icons.dns_outlined),
            title: Text(r.name.isEmpty ? r.address : r.name),
            subtitle: Text('${r.address}:${r.port} ${r.protocol.toUpperCase()}'),
          ),
    ];
  }
}
