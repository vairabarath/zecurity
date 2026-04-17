// build.rs — Proto compilation build script
//
// Cargo runs this BEFORE compiling any src/ files.
//
// What it does:
//   Reads  → ../proto/connector/v1/connector.proto and ../proto/shield/v1/shield.proto
//   Calls  → protoc (system protobuf compiler) via tonic-prost-build
//   Writes → target/build/<hash>/out/connector.rs  (auto-generated Rust structs + gRPC client)
//
// The generated code contains:
//   - EnrollRequest, EnrollResponse       (used by enrollment.rs in Phase 5)
//   - HeartbeatRequest, HeartbeatResponse  (used by heartbeat.rs in Phase 6)
//   - ConnectorServiceClient              (gRPC client for both RPCs)
//
// main.rs pulls the generated code in via:  tonic::include_proto!("connector")
//
// Rebuild trigger:
//   cargo:rerun-if-changed ensures this script re-runs only when the proto file changes.
//   Unrelated source changes do NOT trigger a proto recompile.
//
// IMPORTANT: The copied proto must stay byte-for-byte compatible with the
// controller's proto definition.

fn main() -> Result<(), Box<dyn std::error::Error>> {
    // Tell cargo to re-run this build script only when the proto file changes.
    println!("cargo:rerun-if-changed=../proto/connector/v1/connector.proto");
    println!("cargo:rerun-if-changed=../proto/shield/v1/shield.proto");

    // Compile the protos into Rust gRPC stubs.
    // This generates both client and server code, but we only use the client side.
    tonic_prost_build::configure().compile_protos(
        &[
            "../proto/connector/v1/connector.proto",
            "../proto/shield/v1/shield.proto",
        ],
        &["../proto"],
    )?;

    Ok(())
}
