fn main() -> Result<(), Box<dyn std::error::Error>> {
    tonic_build::configure()
        .build_server(false)
        .build_client(true)
        .compile_protos(&["../proto/client/v1/client.proto"], &["../proto"])?;
    Ok(())
}
