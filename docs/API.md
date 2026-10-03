# Vianden Server API

This document describes everything a client needs to talk to a Vianden server.
You should be able to write a client in any language using only this file.

- **Protocol version:** 1
- **Status:** early development. The API may change in breaking ways until 1.0; every change is listed in the [Changelog](#changelog).

## Contents
1. [Connection basics](#1-connection-basics)
2. [Standard error format](#2-standard-error-format)
3. [Authentication](#3-authentication)
4. [REST endpoints](#4-rest-endpoints)
5. [WebSocket](#5-websocket)
6. [Voice signaling](#6-voice-signaling)
7. [Rate limits and size limits](#7-rate-limits-and-size-limits)
8. [Changelog](#changelog)

---

## 1. Connection basics

### Base URL
The user gives the client a server address (domain or IP, optionally with a port). All REST routes live under:

```
<scheme>://<host>[:<port>]/api/v1/
```

### Transport and TLS
> Not finished yet: HTTPS modes arrive in milestone M4.

During development the server speaks plain HTTP (default `http://127.0.0.1:8080`).
Planned modes: `autocert` (Let's Encrypt), `self-signed` (clients use trust-on-first-use of the certificate fingerprint), and `plain` (local development or behind a reverse proxy).

### Content type
- Requests with a body send JSON (`Content-Type: application/json`).
- All responses, including errors, are JSON (`Content-Type: application/json; charset=utf-8`).
- Field names use `snake_case`.

### Version handshake
Before anything else, a client calls [`GET /api/v1/info`](#get-apiv1info) and compares `protocol_version` with the version it was built for. If they differ, the client should not continue and should show the user a clear message (for example, "This server needs a newer version of the app").

---

## 2. Standard error format

Every error response (any 4xx or 5xx status) has this body:

```json
{
  "error": {
    "code": "not_found",
    "message": "route not found"
  }
}
```

| Field | Type | Meaning |
|---|---|---|
| `error.code` | string | Stable, machine-readable code. Clients should branch on this. |
| `error.message` | string | Human-readable English text. For logs and debugging; may change at any time. |

### Error codes
| Code | HTTP status | Meaning |
|---|---|---|
| `not_found` | 404 | No route matches the method and path. |
| `internal_error` | 500 | Unexpected server error. Details are only in the server log. |

---

## 3. Authentication
> Not available yet: arrives in milestone M1 (register, login, token refresh, how to send tokens).

---

## 4. REST endpoints

### `GET /api/v1/health`
Checks that the server is running and can reach its database. Useful for monitoring.

- **Auth:** none
- **Response `200 OK`:** everything works.

```json
{ "status": "ok", "database": "ok" }
```

- **Response `503 Service Unavailable`:** the server is running but its database is not reachable.

```json
{ "status": "unavailable", "database": "unreachable" }
```

| Field | Type | Values |
|---|---|---|
| `status` | string | `ok`, `unavailable` |
| `database` | string | `ok`, `unreachable` |

This endpoint does not use the standard error format: both responses have the same shape so monitoring tools can read them the same way.

Example:
```
curl http://127.0.0.1:8080/api/v1/health
```

### `GET /api/v1/info`
Returns the server's name and versions. Used for the [version handshake](#version-handshake).

- **Auth:** none
- **Response `200 OK`:**

```json
{
  "name": "My Vianden Server",
  "version": "0.1.0-dev",
  "protocol_version": 1
}
```

| Field | Type | Meaning |
|---|---|---|
| `name` | string | Server name chosen by the host (at most 64 characters). |
| `version` | string | Server software version. For display only; do not use it for compatibility checks. |
| `protocol_version` | integer | API protocol version. Compare this for compatibility. |

Example:
```
curl http://127.0.0.1:8080/api/v1/info
```

---

## 5. WebSocket
> Not available yet: arrives in milestone M3 (`/api/v1/ws`, authentication, event types, reconnect rules).

---

## 6. Voice signaling
> Not available yet: arrives in milestone M7.

---

## 7. Rate limits and size limits

| Limit | Value |
|---|---|
| Request headers must be fully sent within | 10 seconds |
| Idle keep-alive connections are closed after | 2 minutes |

Rate limits arrive with authentication in M1.

---

## Changelog

### Protocol version 1 (in development)
- Added `GET /api/v1/health` (reports database status; 503 when the database is unreachable).
- Added `GET /api/v1/info`.
- Added the standard error format.
