fn main() -> Result<(), Box<dyn std::error::Error>> {
    tonic_prost_build::configure()
        .build_client(true)
        .build_server(false)
        .compile_protos(&["../proto/shield/v1/shield.proto"], &["../proto"])?;

    // Test-only: a server-side copy of the same proto in its own directory,
    // included only under #[cfg(test)] (enrollment_tests.rs) to fake the
    // controller. The shipped binary keeps the client-only code above.
    let test_out = std::path::PathBuf::from(std::env::var("OUT_DIR")?).join("test_server");
    std::fs::create_dir_all(&test_out)?;
    tonic_prost_build::configure()
        .build_client(false)
        .build_server(true)
        .out_dir(&test_out)
        .compile_protos(&["../proto/shield/v1/shield.proto"], &["../proto"])?;
    Ok(())
}
