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
- Request bodies are parsed strictly: unknown fields are rejected with `invalid_request`. Send only the documented fields.
- Text is UTF-8.
- IDs are 64-bit integers.
- Every response has `Cache-Control: no-store`, because responses may contain tokens or private data.

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
| `invalid_request` | 400 | The body is not valid JSON, has unknown fields, has the wrong types, contains more than one JSON value, or is larger than 64 KiB. |
| `invalid_username` | 400 | Username breaks the [username rules](#account-rules). |
| `invalid_display_name` | 400 | Display name breaks the [display name rules](#account-rules). |
| `invalid_password` | 400 | Password breaks the [password rules](#account-rules). |
| `invalid_invite` | 403 | Invite code is unknown, expired, or used up. |
| `invalid_setup_token` | 403 | Owner setup token is wrong, or the server already has an owner. |
| `not_found` | 404 | No route matches the method and path. |
| `username_taken` | 409 | Another account already has this username (comparison ignores case). |
| `internal_error` | 500 | Unexpected server error. Details are only in the server log. |

---

## 3. Authentication

### Session tokens
Registering (and, soon, logging in) returns a **session token**: an opaque string like `vs_3kQ9xZ...` (46 characters: the prefix `vs_` plus 43 URL-safe base64 characters).

- Treat it like a password: store it securely (for example in the OS keychain), never log it, never put it in a URL.
- Send it on every authenticated request in the `Authorization` header:
  ```
  Authorization: Bearer vs_3kQ9xZ...
  ```
  > Endpoints that require a token arrive in the next step (login, logout, `GET /api/v1/me`).
- A session expires after **30 days without use**. Using it extends the expiry.
- There is no separate refresh token.

### Owner setup (first run)
A new server has no owner. While that is true, the server prints a **one-time setup token** (`vo_...`) to its console at every start. The person running the server registers with it, using it as the `invite_code`, and becomes the **owner**. After that the setup token never works again, and no new one is printed.

### Invites
Everyone else needs an **invite code** (`vi_...`) from the owner. Invite codes have a maximum number of uses and always expire.

### Account rules
| Field | Rules |
|---|---|
| `username` | 3-32 characters: `a-z`, `0-9`, `_`, `.`, `-`; must start with a letter or digit. Case-insensitive: it is stored in lowercase, and `Osama` and `osama` are the same username. |
| `display_name` | 1-32 characters, any language and emoji. No control characters or invisible formatting characters (for example right-to-left overrides). Leading and trailing spaces are removed. |
| `password` | 8-256 characters. Any characters, including spaces and emoji. No other complexity rules. |

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

### `POST /api/v1/register`
Creates an account and logs it in. Use the owner setup token as `invite_code` for the very first account (see [Owner setup](#owner-setup-first-run)); everyone else uses an invite code.

- **Auth:** none (the invite code or setup token is the permission)
- **Request body:**

```json
{
  "username": "Osama",
  "display_name": "Osama",
  "password": "correct horse battery staple",
  "invite_code": "vi_Qm9vb3Ro..."
}
```

| Field | Type | Required | Notes |
|---|---|---|---|
| `username` | string | yes | Stored in lowercase. See [Account rules](#account-rules). |
| `display_name` | string | no | Defaults to `username` as typed (before lowercasing). |
| `password` | string | yes | See [Account rules](#account-rules). |
| `invite_code` | string | yes | An invite code (`vi_...`) or the owner setup token (`vo_...`). |

- **Response `201 Created`:**

```json
{
  "user": {
    "id": 1,
    "username": "osama",
    "display_name": "Osama",
    "is_owner": true
  },
  "token": "vs_3kQ9xZ..."
}
```

| Field | Type | Meaning |
|---|---|---|
| `user.id` | integer | Account ID. |
| `user.username` | string | Normalized (lowercase) username. |
| `user.display_name` | string | Name to show in the UI. Treat it as plain text, never as markup. |
| `user.is_owner` | boolean | `true` only for the server owner. |
| `token` | string | Session token. See [Session tokens](#session-tokens). |

- **Errors:** `invalid_request` (400), `invalid_username` (400), `invalid_display_name` (400), `invalid_password` (400), `invalid_invite` (403), `invalid_setup_token` (403), `username_taken` (409).
- **Order of checks:** the input fields are validated first. Then the invite or setup token is checked. Only with a valid invite can `username_taken` be returned, so people without an invite cannot probe which usernames exist.
- A failed registration never uses up an invite.

Example:
```
curl -X POST http://127.0.0.1:8080/api/v1/register \
  -H "Content-Type: application/json" \
  -d '{"username":"osama","password":"correct horse battery staple","invite_code":"vo_..."}'
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
| Request body size | 64 KiB |
| Username / display name / password length | See [Account rules](#account-rules) |

Rate limits on authentication endpoints arrive in the next step of M1.

---

## Changelog

### Protocol version 1 (in development)
- Added `GET /api/v1/health` (reports database status; 503 when the database is unreachable).
- Added `GET /api/v1/info`.
- Added the standard error format.
- Added `POST /api/v1/register`, session tokens, owner setup, invites, and account rules.
- All responses now send `Cache-Control: no-store`; request bodies are limited to 64 KiB and parsed strictly.
