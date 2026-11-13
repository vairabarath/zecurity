// Turns the client core's error text (Rust error chains such as
// "connect to daemon socket …: Connection refused (os error 111)") into a
// message a user can act on. The raw text stays available as `details`.

/// A line shown under the status card: what happened, and for errors the
/// technical text behind it.
class Note {
  const Note(this.text, {this.details, this.isError = false});

  /// A plain success or progress message.
  const Note.info(this.text)
      : details = null,
        isError = false;

  /// An error explained for the user, keeping the raw text as details.
  factory Note.error(String? raw) {
    final explained = explainError(raw);
    return Note(explained, details: raw, isError: true);
  }

  final String text;
  final String? details;
  final bool isError;
}

/// True when the error only means the app can't reach its own service (the
/// daemon in the VPN service process): stopped, or still starting.
bool isServiceDown(String? raw) {
  final e = (raw ?? '').toLowerCase();
  return e.contains('daemon socket') ||
      e.contains('service is not running') ||
      e.contains('daemon.sock');
}

/// A short explanation of `raw`, saying what to do where that's possible.
String explainError(String? raw) {
  final e = (raw ?? '').toLowerCase();
  bool has(String s) => e.contains(s);

  if (raw == null || raw.trim().isEmpty) {
    return 'Something went wrong. Please try again.';
  }
  if (isServiceDown(raw)) {
    return 'The Zecurity service isn\'t running yet. It is being started.';
  }
  if (has('not configured') || has('requires a configured workspace')) {
    return 'Enter your workspace in Settings.';
  }
  if (has('revoked')) {
    return 'This device was revoked by an administrator. Sign in again to '
        'register it.';
  }
  if (has('vpn permission') || has('vpn interface could not be created') ||
      has('another vpn app')) {
    return 'Android didn\'t allow the VPN. Accept the VPN request, and turn off '
        'any other VPN app (only one can be active).';
  }
  if (has('vpn service is attached') || has('no vpn service')) {
    return 'The Zecurity service isn\'t ready yet. Try again in a moment.';
  }
  if (has('certificate') || has('unknown issuer') || has('platform root') ||
      has('invalid peer')) {
    return 'The controller\'s certificate isn\'t trusted by this app. The app '
        'must be built for this controller.';
  }
  if (has('unauthenticated') || (has('session') && has('expired')) ||
      has('refresh token')) {
    return 'Your session has ended. Sign in again.';
  }
  if (has('timed out') && (has('login') || has('callback') || has('browser'))) {
    return 'Sign-in wasn\'t completed in time. Try again.';
  }
  if ((has('permission denied') && has('policy')) || has('access denied') ||
      has('not allowed')) {
    return 'Access was denied by your organisation\'s policy.';
  }
  if (has('transport error') || has('tcp connect') || has('connection refused') ||
      has('timed out') || has('unreachable') || has('dns error') ||
      has('failed to lookup') || has('network')) {
    return 'Can\'t reach the Zecurity controller. Check your network '
        'connection and the controller address in Settings.';
  }
  return 'Something went wrong. Tap Details for more information.';
}
