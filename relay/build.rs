fn main() -> Result<(), Box<dyn std::error::Error>> {
    println!("cargo:rerun-if-changed=../proto/relay/v1/relay.proto");

    tonic_prost_build::configure()
        .build_client(true)
        .build_server(false)
        .compile_protos(&["../proto/relay/v1/relay.proto"], &[".."])?;

    // Test-only: a server-side copy of the same proto in its own directory,
    // included only under #[cfg(test)] (provision.rs tests) to fake the
    // controller. The shipped binary keeps the client-only code above.
    let test_out = std::path::PathBuf::from(std::env::var("OUT_DIR")?).join("test_server");
    std::fs::create_dir_all(&test_out)?;
    tonic_prost_build::configure()
        .build_client(false)
        .build_server(true)
        .out_dir(&test_out)
        .compile_protos(&["../proto/relay/v1/relay.proto"], &[".."])?;

    Ok(())
}
