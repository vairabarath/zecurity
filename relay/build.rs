fn main() -> Result<(), Box<dyn std::error::Error>> {
    println!("cargo:rerun-if-changed=../proto/relay/v1/relay.proto");

    tonic_prost_build::configure()
        .build_client(true)
        .build_server(false)
        .compile_protos(&["../proto/relay/v1/relay.proto"], &[".."])?;

    Ok(())
}
