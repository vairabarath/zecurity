// The platform side (Android: MainActivity.kt; iOS: AppDelegate.swift): files
// directory, the daemon's service (iOS: the tunnel extension), the VPN
// permission prompt, and the login page.

import 'package:flutter/services.dart';

class Host {
  static const _channel = MethodChannel('io.zecurity.client/platform');

  static Future<String> filesDir() async =>
      (await _channel.invokeMethod<String>('filesDir'))!;

  static Future<void> startService() => _channel.invokeMethod('startService');

  /// Restarts the daemon, e.g. so it reads a changed config.
  static Future<void> restartService() => _channel.invokeMethod('restartService');

  /// Shows Android's VPN permission prompt if needed; true once granted.
  static Future<bool> prepareVpn() async =>
      await _channel.invokeMethod<bool>('prepareVpn') ?? false;

  static Future<void> openUrl(String url) =>
      _channel.invokeMethod('openUrl', {'url': url});

  /// Closes the in-app login page (iOS); nothing to do on Android.
  static Future<void> closeBrowser() => _channel.invokeMethod('closeBrowser');
}
