// The Rust client core (libzecurity_ffi, built from client/ffi) behind
// dart:ffi. Every native call blocks (network, the daemon's socket), so each
// one runs on a short-lived isolate, never on the UI isolate.

import 'dart:convert';
import 'dart:ffi';
import 'dart:io' show Platform;
import 'dart:isolate';

import 'package:ffi/ffi.dart';

typedef _NoArgNative = Pointer<Utf8> Function();
typedef _OneArgNative = Pointer<Utf8> Function(Pointer<Utf8>);
typedef _FreeNative = Void Function(Pointer<Utf8>);
typedef _FreeDart = void Function(Pointer<Utf8>);

/// Android: the shared library packaged in the APK. iOS: the static library
/// linked into the app itself (see ios/README.md).
DynamicLibrary _openCore() => Platform.isIOS
    ? DynamicLibrary.process()
    : DynamicLibrary.open('libzecurity_ffi.so');

/// Calls `zc_<name>` with an optional string argument and decodes the JSON
/// it returns. Runs on whichever isolate calls it.
Map<String, dynamic> _invoke(String name, String? arg) {
  final lib = _openCore();
  final free = lib.lookupFunction<_FreeNative, _FreeDart>('zc_free');
  Pointer<Utf8> out;
  if (arg == null) {
    out = lib.lookupFunction<_NoArgNative, _NoArgNative>('zc_$name')();
  } else {
    final input = arg.toNativeUtf8();
    try {
      out = lib.lookupFunction<_OneArgNative, _OneArgNative>('zc_$name')(input);
    } finally {
      malloc.free(input);
    }
  }
  try {
    return jsonDecode(out.toDartString()) as Map<String, dynamic>;
  } finally {
    free(out);
  }
}

/// A response from the core: `ok`, and the daemon's fields (see the client's
/// `IpcResponse`), or `error`.
class CoreResponse {
  CoreResponse(this.json);

  final Map<String, dynamic> json;

  bool get ok => json['ok'] == true;
  String? get error => json['error'] as String?;
  String? str(String key) => json[key] as String?;
  bool? flag(String key) => json[key] as bool?;
  int? integer(String key) => (json[key] as num?)?.toInt();
}

class Core {
  static Future<CoreResponse> _call(String name, [String? arg]) async =>
      CoreResponse(await Isolate.run(() => _invoke(name, arg)));

  /// Process start-up: the app's files directory (config, state, socket).
  static Future<CoreResponse> init(String filesDir) => _call('init', filesDir);

  static Future<CoreResponse> config() => _call('config');

  static Future<CoreResponse> setup({
    required String workspace,
    String controller = '',
    String httpBase = '',
  }) =>
      _call(
        'setup',
        jsonEncode({
          'workspace': workspace,
          'controller': controller,
          'http_base': httpBase,
        }),
      );

  /// One daemon request: Status, Resources, Sync, Up, Down.
  static Future<CoreResponse> request(String type) =>
      _call('request', jsonEncode({'type': type}));

  static Future<CoreResponse> loginStart() => _call('login_start');
  static Future<CoreResponse> loginPoll() => _call('login_poll');
  static Future<CoreResponse> logout() => _call('logout');
}
