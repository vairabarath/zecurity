# Workspace-discovery rate limiting (FU-11)

The controller rate-limits its public workspace-discovery entry points. They
answer unauthenticated questions about which workspaces exist and who belongs to
them. The limit **slows enumeration down; it does not remove the information.**
Decision record: ADR-009 → *Workspace-discovery enumeration (FU-11)*.

## What is limited

| Entry point | Budgets |
|---|---|
| GraphQL `lookupWorkspacesByEmail(email)` | shared per-IP **and** per-email |
| GraphQL `lookupWorkspace(slug)` | shared per-IP |
| GraphQL `lookupIdpConnections(workspaceSlug)` | shared per-IP |
| REST `GET /workspaces/{slug}/auth` | shared per-IP |

- **Shared budget:** all four draw from **one** budget per client IP, so switching
  between them, or between GraphQL and REST, doesn't help.
- **What's charged:** every execution. Aliases, fragments, the authenticated
  `/graphql` route, and the GET, POST, multipart and websocket transports all
  count.

## Configuration

| Variable | Default | Meaning |
|---|---|---|
| `DISCOVERY_RATE_LIMIT_IP_PER_MIN` | `30` | Requests per client IP per 60-second window (IPv6: per /64) |
| `DISCOVERY_RATE_LIMIT_EMAIL_PER_HOUR` | `5` | Email lookups per canonical email per 1-hour window |
| `DISCOVERY_TRUSTED_PROXY_CIDRS` | empty | Reverse-proxy addresses or CIDRs whose `X-Forwarded-For` is trusted |

- **Limit values:** invalid or non-positive values fall back to the default, with
  a log line (the usual `envOrInt` behaviour).
- **Proxy setting:** a malformed proxy entry stops the controller at startup.
  That includes a bad CIDR, host bits set, or an empty entry such as `a,,b`.
- **Starting values:** the email limit is a starting value; revisit it after
  watching `discovery_requests_total{result="rate_limited_email"}` in production.

Example lines for `.env.example`, not added automatically:

```
# FU-11 workspace-discovery rate limits (defaults shown)
DISCOVERY_RATE_LIMIT_IP_PER_MIN=30
DISCOVERY_RATE_LIMIT_EMAIL_PER_HOUR=5
# Reverse proxies whose X-Forwarded-For is trusted (empty = connection address only)
DISCOVERY_TRUSTED_PROXY_CIDRS=
```

## Client IP and reverse proxies

- **No proxies configured (default):** the TCP connection's address is the
  client. `X-Forwarded-For`, `X-Real-IP` and `Forwarded` are ignored.
- **With `DISCOVERY_TRUSTED_PROXY_CIDRS` set:** `X-Forwarded-For` is read only when
  the connection comes from one of those ranges.
  - **Which address counts:** the header is read from the right, and the first
    address that is not itself a trusted proxy is the client.
  - **Spoofing:** addresses further left were written by the client and ignored.
  - **Malformed hop:** the request is charged to the proxy.
- **Proxy responsibility:** the reverse proxy must **append** the client address
  to `X-Forwarded-For` (nginx `$proxy_add_x_forwarded_for`), or overwrite the
  header. It must never forward a client-supplied value as the right-most entry.
- **Behind a proxy without this setting:** every user shares the proxy's single
  budget.
- **Scope:** provider-console login keeps its own connection-address-only
  handling; this setting doesn't affect it.

## Responses

| Situation | GraphQL (`/graphql`) | REST |
|---|---|---|
| Over a budget | HTTP **429**, `Retry-After: <s>`, `{"errors":[{"message":"Too many requests. Try again later.","extensions":{"code":"RATE_LIMITED","retryAfterSeconds":<s>}}],"data":null}` | HTTP **429**, `Retry-After`, `{"error":"rate_limited"}` |
| Limiter unavailable (Valkey down) | HTTP **503**, `extensions.code: SERVICE_UNAVAILABLE`, `"data":null` | HTTP **503**, `{"error":"service_unavailable"}` |
| Websocket | the same GraphQL error, with no data (a websocket message has no HTTP status) | — |

- **Whole-operation refusal:** if any discovery field in a GraphQL operation is
  refused, the whole operation returns no data. Fields that ran first, such as
  other aliases, don't leak.
- **Fail closed:** when the limiter can't decide, the request is refused; it is
  never served without the limit. The infrastructure error goes to the
  controller log only.
- **Admin login page:** a 429 or 503 currently shows "Workspace lookup failed".
  A dedicated "too many attempts" message is a follow-up (`admin/`).

## Fixed windows

- **How:** each budget is a Valkey counter with a fixed window (`INCR` plus expiry
  in one atomic script). A counter that somehow lost its expiry gets one again
  on the next request.
- **Limitation:** across a window boundary a client can get up to **about twice
  the allowance** in a short burst (the end of one window plus the start of the
  next).

## Email buckets and the master secret

- **Bucket key:** the email is canonicalized (trimmed, lower-cased, the
  application's rule everywhere). The key is the hex HMAC-SHA256 of that string,
  under a key derived with HKDF-SHA256 from `PKI_MASTER_SECRET` (info
  `zecurity/discovery-ratelimit/email/v1`).
- **Privacy:** raw emails never appear in Valkey keys, logs or metrics.
- **Rotation:** rotating `PKI_MASTER_SECRET` changes every bucket. The counters
  restart, so each email gets at most one extra window of attempts. Nothing
  breaks.
- **Residual risk:** someone can deliberately use up a specific email's budget.
  That blocks only the "find my workspace by email" picker for up to an hour;
  signing in with the workspace name still works.

## Monitoring

- **Metric:** `discovery_requests_total{operation, result}` on the internal
  metrics listener.
  - `operation`: `lookup_workspaces_by_email`, `lookup_workspace`,
    `lookup_idp_connections` or `workspace_auth_discovery`.
  - `result`: `allowed`, `rate_limited_ip`, `rate_limited_email` or
    `limiter_error`.
  - Labels are never an IP or an email. Aggregate discovery volume is the sum
    over all series.
- **Logs (`discovery:` prefix):**
  - `rate limited … scope=ip client=<ip or /64>`: once per IP per window.
  - `rate limited … scope=email email_key=<12 hex>`: the first refusal of an email
    bucket in its window, then every 10th. A repeated `email_key` across many
    clients is targeted enumeration of one person.
  - `limiter unavailable, failing closed`: Valkey trouble; requests are being
    refused.
