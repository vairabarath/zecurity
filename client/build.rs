fn main() -> Result<(), Box<dyn std::error::Error>> {
    tonic_build::configure()
        // Server codegen is unused in production (this binary only ever
        // dials OUT to the Go controller as a client) but is enabled so
        // daemon.rs's e2e tests can stand up a real in-process
        // ClientServiceServer to drive RenewCert against — see
        // daemon::renewal_tests. Dead weight in the release binary, never
        // instantiated by main.rs.
        .build_server(true)
        .build_client(true)
        .compile_protos(&["../proto/client/v1/client.proto"], &["../proto"])?;
    Ok(())
}
