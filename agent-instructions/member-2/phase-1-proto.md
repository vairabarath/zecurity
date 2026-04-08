# Phase 1 — connector.proto + gRPC Stub Generation

## Objective

Create the protobuf service definition for the connector gRPC API. This is the **DAY 1 BLOCKER** — commit and push before doing anything else. It unblocks:

- **Member 3**: runs `protoc` to generate Go stubs, then writes Enroll/Heartbeat handlers
- **Member 4**: adds `tonic-build` to Rust `build.rs`, generates Rust stubs

---

## Prerequisites

- None. This is the first thing you do.

---

## Files to Create

```
controller/proto/connector/connector.proto
```

## Files to Modify

```
controller/go.mod   ← add gRPC + protobuf dependencies
```

---

## Step 1 — Create the proto file

**File: `controller/proto/connector/connector.proto`**

```protobuf
syntax = "proto3";

package connector;
option go_package = "github.com/yourorg/ztna/controller/proto/connector";

service ConnectorService {

  // Called once during enrollment.
  // Uses plain TLS — connector has no cert yet.
  // Connector presents enrollment JWT + PKCS#10 CSR.
  rpc Enroll(EnrollRequest) returns (EnrollResponse);

  // Called every CONNECTOR_HEARTBEAT_INTERVAL seconds after enrollment.
  // Uses mTLS — connector presents its SPIFFE-certified cert.
  rpc Heartbeat(HeartbeatRequest) returns (HeartbeatResponse);
}

message EnrollRequest {
  string enrollment_token = 1;  // signed JWT — single-use enrollment token
  bytes  csr_der          = 2;  // DER-encoded PKCS#10 CSR (EC P-384)
  string version          = 3;  // CARGO_PKG_VERSION
  string hostname         = 4;
}

message EnrollResponse {
  bytes  certificate_pem      = 1;  // signed leaf cert — 7-day validity, SPIFFE SAN
  bytes  workspace_ca_pem     = 2;  // WorkspaceCA cert (PEM)
  bytes  intermediate_ca_pem  = 3;  // Intermediate CA cert (PEM)
  string connector_id         = 4;  // confirmed connector UUID
}

message HeartbeatRequest {
  string connector_id = 1;  // for logging only — NOT authoritative identity
  string version      = 2;  // CARGO_PKG_VERSION
  string hostname     = 3;
  string public_ip    = 4;  // optional
}

message HeartbeatResponse {
  bool   ok             = 1;
  string latest_version = 2;  // controller informs connector of latest release
  bool   re_enroll      = 3;  // always false this sprint — field plumbed for next sprint
}
```

Key decisions locked in:
- `go_package` uses the actual module path: `github.com/yourorg/ztna/controller/proto/connector`
- `re_enroll` field 3 in `HeartbeatResponse` — Member 3's handler returns `false` this sprint. No proto change needed next sprint.
- `connector_id` in `HeartbeatRequest` is for logging only — authoritative identity comes from the mTLS SPIFFE cert (Member 3's interceptor).
- Field numbers are final. Do NOT renumber.

---

## Step 2 — Add gRPC dependencies

```bash
cd controller
go get google.golang.org/grpc
go get google.golang.org/protobuf
go get google.golang.org/grpc/cmd/protoc-gen-go-grpc
go get google.golang.org/protobuf/cmd/protoc-gen-go
```

---

## Step 3 — Generate Go stubs

Ensure `protoc`, `protoc-gen-go`, and `protoc-gen-go-grpc` are installed, then:

```bash
cd controller
protoc \
  --go_out=. --go_opt=paths=source_relative \
  --go-grpc_out=. --go-grpc_opt=paths=source_relative \
  proto/connector/connector.proto
```

This generates two files alongside the proto:
```
controller/proto/connector/connector.pb.go        ← message types
controller/proto/connector/connector_grpc.pb.go   ← service stubs
```

Verify the generated files compile:

```bash
cd controller && go build ./proto/connector/...
```

---

## Step 4 — Tidy modules

```bash
cd controller && go mod tidy
```

---

## Verification

- [ ] `controller/proto/connector/connector.proto` exists with correct `go_package`
- [ ] `protoc` generates `connector.pb.go` and `connector_grpc.pb.go` without errors
- [ ] `go build ./proto/connector/...` compiles cleanly
- [ ] `go.mod` includes `google.golang.org/grpc` and `google.golang.org/protobuf`
- [ ] `go mod tidy` passes with no issues

---

## DO NOT TOUCH

- Do not create any files under `controller/internal/connector/` yet — that's Phase 2+
- Do not modify `controller/cmd/server/main.go` — that's Phase 5
- Do not modify any file under `controller/internal/appmeta/` — Member 3 owns that

---

## After This Phase

**Immediately commit and push.** Then notify:
- Member 3: "proto is merged, run protoc to generate Go stubs"
- Member 4: "proto is merged, add tonic-build to build.rs"

Then proceed to Phase 2 (config.go).
