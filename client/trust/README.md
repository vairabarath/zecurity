# Client trust anchor

`zecurity-client` trusts exactly one thing it did not download: the **platform root CA** compiled into the binary (certificate audit bug 1). At login it accepts `/ca.crt` only if it is a single Platform Intermediate issued by that root.

## Official releases

`platform-root.pem` in this directory is the production platform's root CA certificate. The release workflow embeds it (`ZECURITY_PLATFORM_ROOT_PEM_FILE=trust/platform-root.pem`). Until the file exists, the client release workflow fails on purpose, so that no release ships without an anchor.

It is a **public certificate**: safe to commit and safe to extract from the binary. Never put a private key here.

To add or replace it:
1. On the production controller's database, run `scripts/export-platform-root.sh trust-root.pem`. It reads only `ca_root.certificate_pem`.
2. Compare the SHA-256 fingerprint the script prints with one obtained through a separate channel you trust (for example, read directly on the production host by the operator).
3. Commit the file as `client/trust/platform-root.pem` in a reviewed PR. Changing it requires a new client release, and every installed client then needs to update.

The production controller's PKI must never be reset or regenerated. A new root would lock out every installed client until it is updated.

## Dev and LAN controllers

Each controller database creates its own root, so a client for a dev or LAN controller must embed **that** controller's root:

```bash
client/scripts/dev-build.sh                                  # local compose DB
ROOT_PEM=/path/to/root.pem client/scripts/dev-build.sh --release   # root copied from another host
```

A debug build without a root compiles, but `zecurity-client login` refuses to run. A release build without one fails to compile.
