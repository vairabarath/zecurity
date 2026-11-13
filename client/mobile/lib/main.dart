// Zecurity mobile client: the Android and iOS counterpart of
// `zecurity-client`'s CLI. The daemon runs in the app's VPN service process
// (iOS: its packet tunnel extension); this UI talks to it through the client
// core (native.dart), the way the CLI does on Linux.

import 'dart:async';
import 'dart:io' show File;

import 'package:material_ui/material_ui.dart';

import 'errors.dart';
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
  Note? _note;
  bool _showDetails = false;
  String _filesDir = '';

  // Service recovery: the app starts its service again when it can't reach
  // it (stopped by sign-out, killed by the system), a few times at most.
  static const _maxServiceStarts = 3;
  static const _serviceStartSpacing = Duration(seconds: 8);
  int _serviceStarts = 0;
  DateTime? _lastServiceStart;
  String? _serviceStartError;

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
    _filesDir = dir;
    await Core.init(dir);
    await _loadConfig();
    // Show the screen now. The service starts in the background (its own
    // process); until it answers, the card reads "Starting Zecurity…".
    if (mounted) setState(() => _ready = true);
    await _refresh();
    if (!_daemonUp) {
      // Show the real state the moment the service answers, rather than at
      // the next 3 s poll.
      await _startService();
      if (await _waitForService(up: true)) await _refresh();
    }
    _poll = Timer.periodic(const Duration(seconds: 3), (_) => _refresh());
  }

  Future<void> _loadConfig() async {
    final c = await Core.config();
    _workspace = c.str('workspace') ?? '';
    _controller = c.str('controller') ?? '';
    _httpBase = c.str('http_base') ?? '';
  }

  /// Starts the service, remembering when (the recovery spacing).
  Future<void> _startService() async {
    _lastServiceStart = DateTime.now();
    _serviceStarts++;
    try {
      await Host.startService();
    } catch (_) {
      // Reported through the next status check.
    }
  }

  /// The reason the service last failed to start, if it left one.
  Future<String?> _readServiceError() async {
    if (_filesDir.isEmpty) return null;
    try {
      final f = File('$_filesDir/daemon.error');
      if (!await f.exists()) return null;
      final text = (await f.readAsString()).trim();
      return text.isEmpty ? null : text;
    } catch (_) {
      return null;
    }
  }

  /// The service isn't answering: start it again (spaced out, a few times),
  /// then give up and explain why, with a manual retry.
  Future<void> _recoverService() async {
    if (_workspace.isEmpty) return;
    final last = _lastServiceStart;
    final due = last == null || DateTime.now().difference(last) >= _serviceStartSpacing;
    if (due && _serviceStarts < _maxServiceStarts) {
      await _startService();
    } else if (_serviceStarts >= _maxServiceStarts) {
      final reason = await _readServiceError();
      if (mounted) setState(() => _serviceStartError = reason ?? _daemonError);
    }
  }

  /// Waits until the service answers (true) or `timeout` passes.
  Future<bool> _waitForService({required bool up, Duration timeout = const Duration(seconds: 10)}) async {
    final end = DateTime.now().add(timeout);
    while (DateTime.now().isBefore(end)) {
      final s = await Core.request('Status');
      if (s.ok == up) return true;
      await Future<void>.delayed(const Duration(milliseconds: 400));
    }
    return false;
  }

  Future<void> _refresh() async {
    if (_busy) return;
    final s = await Core.request('Status');
    if (!mounted) return;
    if (!s.ok) {
      if (isServiceDown(s.error)) unawaited(_recoverService());
      // Nothing about the device is known while the daemon is down: don't
      // keep showing the last signed-in state.
      setState(() {
        _daemonUp = false;
        _daemonError = s.error;
        _tunnelUp = false;
        _email = null;
        _deviceId = null;
        _deviceState = null;
        _certExpiresAt = null;
        _resources = [];
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
      _serviceStarts = 0;
      _serviceStartError = null;
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

  Future<void> _run(String label, Future<Note?> Function() action) async {
    setState(() {
      _busy = true;
      _note = Note.info(label);
      _showDetails = false;
    });
    Note? result;
    try {
      result = await action();
    } catch (e) {
      result = Note.error('$e');
    }
    if (!mounted) return;
    setState(() {
      _busy = false;
      _note = result;
    });
    await _refresh();
  }

  static const _vpnDenied = Note(
    'Android didn\'t allow the VPN. Tap again and accept the VPN request; '
    'turn off any other VPN app first.',
    isError: true,
  );

  Future<void> _connect() => _run('Connecting…', () async {
        if (!await Host.prepareVpn()) return _vpnDenied;
        final r = await Core.request('Up');
        if (!r.ok) return Note.error(r.error);
        return Note.info(r.integer('synced_resources') == 0
            ? 'Connected. No resources are assigned to this device yet.'
            : 'Connected.');
      });

  Future<void> _disconnect() => _run('Disconnecting…', () async {
        final r = await Core.request('Down');
        return r.ok ? const Note.info('Disconnected.') : Note.error(r.error);
      });

  Future<void> _sync() => _run('Syncing…', () async {
        final r = await Core.request('Sync');
        return r.ok ? const Note.info('Resources are up to date.') : Note.error(r.error);
      });

  Future<void> _login() => _run('Signing in…', () async {
        // Signing in brings the tunnel up (as `zecurity-client login` does),
        // which needs Android's VPN permission first.
        if (!await Host.prepareVpn()) return _vpnDenied;
        final start = await Core.loginStart();
        if (!start.ok) return Note.error(start.error);
        String? opened;
        for (var i = 0; i < 400; i++) {
          final p = await Core.loginPoll();
          final url = p.str('url');
          if (url != null && url != opened) {
            opened = url;
            await Host.openUrl(url);
          }
          switch (p.str('state')) {
            case 'done':
              await Host.closeBrowser();
              return const Note.info('Signed in.');
            case 'error':
              await Host.closeBrowser();
              return Note.error(p.error);
          }
          await Future<void>.delayed(const Duration(milliseconds: 750));
        }
        return const Note(
          'Sign-in wasn\'t completed in time. Try again.',
          isError: true,
        );
      });

  Future<void> _logout() => _run('Signing out…', () async {
        final r = await Core.logout();
        // Sign-out stops the daemon (as `zecurity-client logout` does), which
        // ends the VPN service process. Start it again only once that process
        // is gone: a start sent to the exiting process is lost with it.
        await _waitForService(up: false, timeout: const Duration(seconds: 5));
        await Future<void>.delayed(const Duration(milliseconds: 600));
        _serviceStarts = 0;
        await _startService();
        await _waitForService(up: true);
        return r.ok ? const Note.info('Signed out.') : Note.error(r.error);
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
      if (!r.ok) return Note.error(r.error);
      await _loadConfig();
      // The daemon reads its config when it starts.
      _serviceStarts = 0;
      _lastServiceStart = DateTime.now();
      await Host.restartService();
      await _waitForService(up: true);
      return const Note.info('Settings saved.');
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
                  if (_note != null) ...[
                    const SizedBox(height: 12),
                    _noteView(theme, _note!),
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
    } else if (!_daemonUp && _serviceStartError != null) {
      title = 'Zecurity couldn\'t start';
      subtitle = explainError(_serviceStartError);
    } else if (!_daemonUp) {
      title = 'Starting Zecurity…';
      subtitle = 'Getting the secure connection service ready.';
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
    if (!_daemonUp && _serviceStartError == null) {
      return const OutlinedButton(onPressed: null, child: Text('Starting…'));
    }
    if (!_daemonUp) {
      return OutlinedButton(
        onPressed: _busy
            ? null
            : () => _run('Starting Zecurity…', () async {
                  setState(() => _serviceStartError = null);
                  _serviceStarts = 0;
                  _lastServiceStart = DateTime.now();
                  await Host.restartService();
                  final up = await _waitForService(up: true);
                  return up ? null : Note.error(await _readServiceError() ?? _daemonError);
                }),
        child: const Text('Try again'),
      );
    }
    if (!signedIn || _deviceState == 'revoked') {
      return FilledButton(onPressed: _busy ? null : _login, child: const Text('Sign in'));
    }
    return _tunnelUp
        ? OutlinedButton(onPressed: _busy ? null : _disconnect, child: const Text('Disconnect'))
        : FilledButton(onPressed: _busy ? null : _connect, child: const Text('Connect'));
  }

  /// The result of the last action; errors explained, with the raw text
  /// behind a Details toggle.
  Widget _noteView(ThemeData theme, Note note) {
    final scheme = theme.colorScheme;
    if (!note.isError) return Text(note.text, style: theme.textTheme.bodyMedium);
    return Container(
      padding: const EdgeInsets.all(12),
      decoration: BoxDecoration(
        color: scheme.errorContainer,
        borderRadius: BorderRadius.circular(12),
      ),
      child: Column(
        crossAxisAlignment: .start,
        children: [
          Row(
            crossAxisAlignment: .start,
            children: [
              Icon(Icons.error_outline, color: scheme.onErrorContainer),
              const SizedBox(width: 8),
              Expanded(
                child: Text(
                  note.text,
                  style: theme.textTheme.bodyMedium?.copyWith(color: scheme.onErrorContainer),
                ),
              ),
            ],
          ),
          if (note.details != null && note.details!.isNotEmpty) ...[
            Align(
              alignment: .centerRight,
              child: TextButton(
                onPressed: () => setState(() => _showDetails = !_showDetails),
                child: Text(_showDetails ? 'Hide details' : 'Details'),
              ),
            ),
            if (_showDetails)
              SelectableText(
                note.details!,
                style: theme.textTheme.bodySmall?.copyWith(color: scheme.onErrorContainer),
              ),
          ],
        ],
      ),
    );
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
