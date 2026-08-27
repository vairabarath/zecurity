// Thin shell around the library. The socket layer and the systemd-resolved calls are
// the next slice (ADR-023); nothing here is privileged yet, so the binary refuses to
// run rather than pretending to work.

fn main() {
    eprintln!(
        "zecurity-dns-helper: not implemented yet.\n\
         The validation whitelist is complete (see zecurity_dns_helper::validate);\n\
         the socket layer, SO_PEERCRED peer authentication and the resolved D-Bus\n\
         calls are the next slice. See ADR-023."
    );
    std::process::exit(1);
}
