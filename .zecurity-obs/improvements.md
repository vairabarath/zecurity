# Production Optimizations & Best Practices

This document outlines critical improvements for transitioning the ZECURITY gRPC/mTLS infrastructure from a development prototype to a production-grade system.

## 1. gRPC Infrastructure

### gRPC Native Keepalives
**Problem:** NAT gateways and cloud load balancers often silently drop idle TCP connections, causing "ghost" streams where the server thinks a client is connected but packets are being dropped.
**Optimization:**
- **Controller (Go):** Implement `keepalive.ServerParameters` and `keepalive.EnforcementPolicy`. Set `Time` to 30s and `Timeout` to 10s.
- **Connector/Shield (Rust):** Configure Tonic `Channel` with `.keep_alive_while_idle(true)` and `.http2_keep_alive_interval(Duration::from_secs(30))`.

### Connection Draining
**Optimization:** Implement graceful shutdown for the Controller. When the Controller receives a SIGTERM, it should send a `Goodbye` signal or close the streams with a `GOAWAY` frame, allowing Connectors to reconnect to a different instance immediately rather than waiting for a timeout.

## 2. Security & Identity

### Real-time Revocation (OCSP/CRL)
**Problem:** Currently, revocation is checked only at stream establishment. If a connector is revoked while a stream is active, it stays active until the next heartbeat check (up to 90s) or cert expiry.
**Optimization:** Implement a "Kill Switch" in the `ConnectorRegistry`. When a tenant is suspended or a connector revoked, the registry should immediately call `stream.Context().Cancel()` to kill the active gRPC goroutine.

### Short-Lived Certificates
**Optimization:** Reduce certificate TTL from 7 days to 24 hours. This minimizes the "window of vulnerability" if a private key is compromised, as the attacker has less time before they must re-authenticate with a valid enrollment token or renewal key.

## 3. Scalability

### Layer 4 Load Balancing
**Strategy:** Use a Cloud NLB (Network Load Balancer) or HAProxy in TCP mode. 
- **CRITICAL:** Do not use Layer 7 (Application) Load Balancers unless they support TLS Passthrough. Terminators like Nginx/ALB will break the mTLS handshake because they cannot present the client's certificate to the Go Controller.

### Stateful Stream Routing
**Problem:** In a multi-controller setup, Shield instructions must reach the *specific* controller instance where the Connector has an open stream.
**Optimization:** Use Valkey/Redis as a "Stream Routing Table." When a Connector connects to Controller A, it writes its location to Redis. When a Shield request comes in, the Controller checks Redis and forwards the instruction if the Connector is on a different node.

## 4. Observability

### Distributed Tracing
**Optimization:** Inject `traceparent` headers into gRPC metadata. This allows following a request from:
`Admin UI (Browser) -> Controller GQL -> Controller gRPC -> Connector -> Shield`.

### Health Metrics
**Recommended Metrics:**
- `zecurity_active_streams_total`: Gauge of currently connected agents.
- `zecurity_stream_reconnect_count`: Counter for detecting unstable networks.
- `zecurity_instruction_latency_seconds`: Time from GQL request to Resource Ack.
