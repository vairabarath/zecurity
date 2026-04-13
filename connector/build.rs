// build.rs — Proto compilation build script
//
// Cargo runs this BEFORE compiling any src/ files.
//
// What it does:
//   Reads  → ../controller/proto/connector/connector.proto  (Member 2's proto definition)
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
// IMPORTANT: We do NOT own the proto file — Member 2 does.
//   If you need a proto change, coordinate with the controller team.

fn main() -> Result<(), Box<dyn std::error::Error>> {
    // Tell cargo to re-run this build script only when the proto file changes.
    println!("cargo:rerun-if-changed=../controller/proto/connector/connector.proto");

    // Compile the proto into Rust gRPC client stubs.
    // This generates both client and server code, but we only use the client side.
    tonic_prost_build::compile_protos("../controller/proto/connector/connector.proto")?;

    Ok(())
}
