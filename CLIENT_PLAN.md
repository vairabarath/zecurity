# Zecurity Client Application Plan

> Sprint 7 Client Application — Phase 1

---

## Overview

Build the first step of the Zecurity client application (CLI).

---

## Architecture

### Connect To

- **Controller gRPC** — for login, enrollment, status
- **Connector** — for tunnel access (later phase)

### Configuration

| Field | Type | Example |
|-------|------|---------|
| `controller_address` | string | `controller.example.com:9090` |
| `workspace_slug` | string | `myworkspace` |
| `mode` | string | `tun` / `socks5` |

### State Store

- Location: `{state_dir}/client.json`
- Format: JSON

---

## Phase 1: Login + Status

### Login Flow

```
1. Device authorize → Controller (gRPC)
2. Return OAuth URL
3. User signs in with Google (browser)
4. Token exchange
5. Enroll device → get mTLS certificate
```

```
Client
    │
    ├── device_authorize(tenant_slug, code_challenge, redirect_uri)
    │         │
    │         ▼
    │    OAuth URL ← User logs in with Google
    │         │
    │         ▼
    │    Code ← Callback
    │         │
    ├── token_exchange(code, code_verifier, state)
    │         │
    │         ▼
    │    Access token + Refresh token
    │         │
    ├── enroll_device(access_token, device_info)
    │         │
    │         ▼
    │    mTLS certificate + SPIFFE ID
```

### Status Command

```
$ zecurity-client status

Email:           user@example.com
Workspace:      myworkspace
Status:          Connected
Session:         Active (expires in 24h)
Device:           Verified
Mode:             tun
Controller:       controller.example.com:9090
```

### Commands

| Command | Description |
|---------|-------------|
| `setup` | Configure controller address + workspace |
| `login` | Start login flow |
| `status` | Show connection status |
| `logout` | Clear session and cert |
| `invite` | Invite a new user to the workspace |

---

## Client Invitation Flow

### How It Works

```
Admin (via API / Admin UI)
    │
    │
    ▼
Controller (create invitation)
    │
    │
    ▼
Email sent to user ──click link──► Verify Email page
```

### Invitation Data Model

| Field | Type | Description |
|-------|------|-------------|
| `id` | UUID | Invitation ID |
| `email` | string | User email |
| `workspace_id` | UUID | Target workspace |
| `invited_by` | UUID | Admin user ID |
| `status` | enum | `pending` / `accepted` / `expired` |
| `expires_at` | timestamp | Expiration time |
| `created_at` | timestamp | Created time |

### CLI Command

```
$ zecurity-client invite --email user@example.com
```

### API

```
POST /api/invitations
{
    "email": "user@example.com",
    "workspace_id": "uuid"
}

Response:
{
    "id": "inv-uuid",
    "email": "user@example.com",
    "status": "pending",
    "expires_at": "2026-05-01T00:00:00Z"
}
```

### Invitation Email Content

```
Subject: You've been invited to Zecurity

Body:
You've been invited to join myworkspace on Zecurity.

Click here to accept: https://zecurity.example.com/invite/abc123

This invitation expires in 7 days.
```

### User Journey (after clicking link)

```
1. User clicks invite link
   │
2. Verify Email page (web)
   │  - Shows: "Sign in to accept invitation"
   │  - User signs in with Google
   │
3. OAuth consent screen
   │  - User approves
   │
4. Client Installation page
   │  - Download client for their OS
   │  - Installation instructions
   │
5. User installs client
   │
6. User runs: zecurity-client login
   │
7. Device enrollment → get mTLS cert
   │
8. Client connects to Connector :9092 → access resources
```

### Invitation States

| State | Description |
|-------|-------------|
| `pending` | Invitation sent, not yet accepted |
| `accepted` | User signed up and enrolled device |
| `expired` | Invitation expired (optional) |

---

## Additional Features (Later Phases)

- Tunnel to Connector `:9092`
- TUN mode / SOCKS5 mode
- Device verification (second email verification)
- Access log viewer
- QUIC support
- Auto-cert renewal