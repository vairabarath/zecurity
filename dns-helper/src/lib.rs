// zecurity-dns-helper — the privileged half of ADR-023 option C.
//
// Structured as a library plus a thin binary on purpose: the validation whitelist is
// the entire security value of this component, and as library API it can be tested,
// reviewed and reasoned about without a root process, a socket, or systemd anywhere
// near it. The binary is a shell that reads a request, calls `validate`, and — only
// if that succeeds — talks to systemd-resolved.

pub mod validate;
