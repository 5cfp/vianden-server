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
| `invalid_max_uses` | 400 | Invite `max_uses` is outside 1-100. |
| `invalid_expires_in_hours` | 400 | Invite `expires_in_hours` is outside 1-720. |
| `invalid_invite` | 403 | Invite code is unknown, expired, or used up. |
| `invalid_credentials` | 401 | Login failed: wrong username or password (the response never says which). |
| `unauthorized` | 401 | The session token is missing, malformed, unknown, expired, or revoked. The response also has the header `WWW-Authenticate: Bearer`. |
| `invalid_setup_token` | 403 | Owner setup token is wrong, or the server already has an owner. |
| `forbidden` | 403 | You are logged in, but not allowed to do this (for example, a member managing invites). |
| `not_found` | 404 | No route matches the method and path. |
| `username_taken` | 409 | Another account already has this username (comparison ignores case). |
| `rate_limited` | 429 | Too many attempts. Wait the number of seconds in the `Retry-After` header. |
| `internal_error` | 500 | Unexpected server error. Details are only in the server log. |

---

## 3. Authentication

### Session tokens
Registering and logging in return a **session token**: an opaque string like `vs_3kQ9xZ...` (46 characters: the prefix `vs_` plus 43 URL-safe base64 characters).

- Treat it like a password: store it securely (for example in the OS keychain), never log it, never put it in a URL.
- Send it on every authenticated request in the `Authorization` header:
  ```
  Authorization: Bearer vs_3kQ9xZ...
  ```
- A session expires after **30 days without use**. Using it extends the expiry.
- There is no separate refresh token.
- Each login creates a new session, so every device has its own token. Logging out ends only that session.
- If a request with a token gets `401 unauthorized`, the token is no longer valid: delete it and show the login screen. Do **not** do this on `500` errors (the server may just be having trouble).

### Owner setup (first run)
A new server has no owner. While that is true, the server prints a **one-time setup token** (`vo_...`) to its console at every start. The person running the server registers with it, using it as the `invite_code`, and becomes the **owner**. After that the setup token never works again, and no new one is printed.

### Invites
Everyone else needs an **invite code** (`vi_...`) from the owner. Invite codes have a maximum number of uses and always expire. The owner creates them with [`POST /api/v1/invites`](#post-apiv1invites). The code is shown **only once**, when it is created (the server stores only its hash).

In this protocol version only the owner can manage invites. Roles (admins, moderators) come later.

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
- **Rate limited** together with login (see [Rate limits](#rate-limits)).

Example:
```
curl -X POST http://127.0.0.1:8080/api/v1/register \
  -H "Content-Type: application/json" \
  -d '{"username":"osama","password":"correct horse battery staple","invite_code":"vo_..."}'
```

### `POST /api/v1/login`
Logs in and starts a new session.

- **Auth:** none
- **Request body:**

```json
{ "username": "Osama", "password": "correct horse battery staple" }
```

| Field | Type | Required | Notes |
|---|---|---|---|
| `username` | string | yes | Case-insensitive; surrounding spaces are ignored. |
| `password` | string | yes | |

- **Response `200 OK`:** same shape as [register](#post-apiv1register): `{ "user": {...}, "token": "vs_..." }`.
- **Errors:** `invalid_request` (400), `invalid_credentials` (401), `rate_limited` (429).
- Wrong password and unknown username give the **same** error and take about the same time, so login cannot be used to find out which usernames exist.
- **Rate limited** together with register (see [Rate limits](#rate-limits)).

Example:
```
curl -X POST http://127.0.0.1:8080/api/v1/login \
  -H "Content-Type: application/json" \
  -d '{"username":"osama","password":"correct horse battery staple"}'
```

### `POST /api/v1/logout`
Ends the current session. Its token stops working immediately. Other sessions of the same user (other devices) are not affected.

- **Auth:** session token
- **Request body:** none
- **Response `204 No Content`:** no body.
- **Errors:** `unauthorized` (401).

Example:
```
curl -X POST http://127.0.0.1:8080/api/v1/logout -H "Authorization: Bearer vs_..."
```

### `GET /api/v1/me`
Returns the logged-in user. Useful at app start to check whether a stored token is still valid.

- **Auth:** session token
- **Response `200 OK`:**

```json
{
  "user": { "id": 1, "username": "osama", "display_name": "Osama", "is_owner": true }
}
```

- **Errors:** `unauthorized` (401).

Example:
```
curl http://127.0.0.1:8080/api/v1/me -H "Authorization: Bearer vs_..."
```

### `POST /api/v1/invites`
Creates an invite code.

- **Auth:** session token; **owner only**
- **Request body:** a JSON object (send `{}` to use all defaults).

```json
{ "max_uses": 3, "expires_in_hours": 48 }
```

| Field | Type | Required | Notes |
|---|---|---|---|
| `max_uses` | integer | no | 1-100. Default: 1. |
| `expires_in_hours` | integer | no | 1-720 (30 days). Default: 168 (7 days). |

- **Response `201 Created`:**

```json
{
  "invite": {
    "id": 1,
    "created_by": 1,
    "max_uses": 3,
    "uses": 0,
    "expires_at": "2026-10-06T12:00:00Z",
    "created_at": "2026-10-04T12:00:00Z"
  },
  "code": "vi_Qm9vb3Ro..."
}
```

| Field | Type | Meaning |
|---|---|---|
| `invite` | object | The [invite object](#invite-object). |
| `code` | string | The invite code to share. **Shown only in this response**; it cannot be retrieved again. |

- **Errors:** `invalid_request` (400), `invalid_max_uses` (400), `invalid_expires_in_hours` (400), `unauthorized` (401), `forbidden` (403).

#### Invite object
| Field | Type | Meaning |
|---|---|---|
| `id` | integer | Invite ID (used to delete it). |
| `created_by` | integer | User ID of the creator. |
| `max_uses` | integer | How many accounts can be created with it. |
| `uses` | integer | How many have been created so far. |
| `expires_at` | string | Expiry time, RFC 3339 in UTC. |
| `created_at` | string | Creation time, RFC 3339 in UTC. |

An invite is usable while `uses < max_uses` and `expires_at` is in the future.

### `GET /api/v1/invites`
Lists all invites, newest first, including used-up and expired ones. Codes are never included.

- **Auth:** session token; **owner only**
- **Response `200 OK`:** `{ "invites": [ <invite object>, ... ] }` (an empty list is `[]`).
- **Errors:** `unauthorized` (401), `forbidden` (403).

### `DELETE /api/v1/invites/{id}`
Deletes an invite. Its code stops working immediately (use this if a code leaked). Accounts already created with it are not affected.

- **Auth:** session token; **owner only**
- **Response `204 No Content`:** no body.
- **Errors:** `unauthorized` (401), `forbidden` (403), `not_found` (404).

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

### Rate limits
| Endpoints | Limit |
|---|---|
| `POST /register` and `POST /login` (shared) | 10 requests at once, then 1 more every 6 seconds, per client IP address (IPv6: per /64 network) |

Over the limit the server answers `429 rate_limited` with a `Retry-After` header (seconds). Refused requests do not count against the limit.

---

## Changelog

### Protocol version 1 (in development)
- Added `GET /api/v1/health` (reports database status; 503 when the database is unreachable).
- Added `GET /api/v1/info`.
- Added the standard error format.
- Added `POST /api/v1/register`, session tokens, owner setup, invites, and account rules.
- Added `POST /api/v1/login`, `POST /api/v1/logout`, `GET /api/v1/me`, bearer token authentication, and rate limits on register and login.
- Added `POST /api/v1/invites`, `GET /api/v1/invites`, `DELETE /api/v1/invites/{id}` (owner only).
- All responses now send `Cache-Control: no-store`; request bodies are limited to 64 KiB and parsed strictly.
