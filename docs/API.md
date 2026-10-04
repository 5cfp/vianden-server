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
A server runs in one of three modes (chosen by its host):

| Mode | Address users type | What a client must do |
|---|---|---|
| `autocert` | `chat.example.com` | Nothing special: a normal, publicly trusted certificate (Let's Encrypt). Plain `http://` requests are redirected to `https://` with `308`. |
| `self-signed` | `203.0.113.7:443` or a name | The certificate is not signed by a known authority. Use **trust on first use** (below). |
| `plain` | `http://127.0.0.1:8080` | No encryption. Only for local development, or behind a reverse proxy that provides HTTPS. |

- Only TLS 1.2 and newer are accepted.
- A client should default to `https://` when the user types an address without a scheme.

#### Trust on first use (self-signed servers)
1. Connect normally. Certificate verification fails.
2. Compute the certificate's **fingerprint**: SHA-256 of the certificate's DER bytes, written as 32 uppercase hex pairs joined by colons, e.g. `A7:E3:45:...:FD:C8`. The server prints the same value in its log at every start, so the owner can share it.
3. Show it to the user and ask them to compare it with the one the owner gave them. Only if they confirm, remember ("pin") it for this `host:port` and connect again, accepting exactly that certificate.
4. On later connections, accept the certificate only if its fingerprint equals the pin. If it differs, **refuse**, warn the user that the connection may be intercepted, and do not send the session token.

Pins apply to both the REST API and the WebSocket.

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
| `invalid_request` | 400 | The body is empty or not valid JSON, has unknown fields or wrong types, contains more than one JSON value, or is larger than 64 KiB. The message says which (e.g. `field "username" has the wrong type`). |
| `invalid_username` | 400 | Username breaks the [username rules](#account-rules). |
| `invalid_display_name` | 400 | Display name breaks the [display name rules](#account-rules). |
| `invalid_password` | 400 | Password breaks the [password rules](#account-rules). |
| `invalid_max_uses` | 400 | Invite `max_uses` is outside 1-100. |
| `invalid_expires_in_hours` | 400 | Invite `expires_in_hours` is outside 1-720. |
| `invalid_role` | 400 | `role` is not `admin`, `moderator`, or `member`. |
| `invalid_reason` | 400 | Ban reason longer than 200 characters, or with control/invisible characters. |
| `invalid_name` | 400 | Channel name breaks the [channel rules](#channel-and-message-rules). |
| `invalid_topic` | 400 | Channel topic breaks the [channel rules](#channel-and-message-rules). |
| `invalid_content` | 400 | Message text breaks the [message rules](#channel-and-message-rules). |
| `invalid_view_role` / `invalid_send_role` | 400 | Not a valid role, or `send_role` below `view_role`. |
| `invalid_before` | 400 | The `before` query parameter is not a positive message id. |
| `invalid_limit` | 400 | The `limit` query parameter is not between 1 and 100. |
| `invalid_invite` | 403 | Invite code is unknown, expired, or used up. |
| `invalid_credentials` | 401 | Login failed: wrong username or password (the response never says which). |
| `unauthorized` | 401 | The session token is missing, malformed, unknown, expired, or revoked. The response also has the header `WWW-Authenticate: Bearer`. |
| `invalid_setup_token` | 403 | Owner setup token is wrong, or the server already has an owner. |
| `forbidden` | 403 | You are logged in, but not allowed to do this (for example, a member managing invites). |
| `account_banned` | 403 | Login: the account is banned. The message includes the reason, if one was given. Only returned when the password was correct. |
| `read_only` | 403 | You can see this channel but your role may not write in it. |
| `not_found` | 404 | No route matches the method and path. |
| `username_taken` | 409 | Another account already has this username (comparison ignores case). |
| `channel_name_taken` | 409 | Another channel already has this name (comparison ignores case). |
| `rate_limited` | 429 | Too many attempts. Wait the number of seconds in the `Retry-After` header. |
| `too_many_connections` | 429 | `GET /api/v1/ws` only: this account already has 10 open WebSocket connections. |
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

Invites are managed by users with the `manage_invites` permission (see [Roles and permissions](#roles-and-permissions)). Without it, the endpoints answer `403 forbidden`.

### Roles and permissions
Every user has exactly one role. Roles are ranked: `owner` > `admin` > `moderator` > `member`.

| Permission | What it allows | owner | admin | moderator | member |
|---|---|:-:|:-:|:-:|:-:|
| `manage_channels` | create, rename, delete channels | ✓ | ✓ | | |
| `manage_invites` | create, list, delete invite codes | ✓ | ✓ | | |
| `manage_roles` | change the role of users below you | ✓ | ✓ | | |
| `delete_messages` | delete other people's messages | ✓ | ✓ | ✓ | |
| `kick_members` | sign a user out on all devices | ✓ | ✓ | ✓ | |
| `ban_members` | block a user from logging in | ✓ | ✓ | | |

**Hierarchy rule:** actions on another user (role change, kick, ban) need the permission **and** a target whose role is strictly below yours. A new role must also be below yours. There is exactly one owner; no one can become owner or act on the owner.

Clients receive `role` and `permissions` in every user object of the logged-in user, to show only the actions that are allowed. The server checks every request regardless.

New accounts are `member`s; the account created with the setup token is the `owner`.

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
    "is_owner": true,
    "role": "owner",
    "permissions": ["manage_channels", "manage_invites", "manage_roles", "delete_messages", "kick_members", "ban_members"]
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
| `user.role` | string | `owner`, `admin`, `moderator`, or `member`. See [Roles and permissions](#roles-and-permissions). |
| `user.permissions` | array of strings | What this user may do. Use it to hide actions; the server checks every request anyway. |
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
- **Errors:** `invalid_request` (400), `invalid_credentials` (401), `account_banned` (403), `rate_limited` (429).
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
  "user": { "id": 1, "username": "osama", "display_name": "Osama", "is_owner": true,
            "role": "owner", "permissions": ["manage_channels", "manage_invites", "manage_roles", "delete_messages", "kick_members", "ban_members"] }
}
```

- **Errors:** `unauthorized` (401).

Example:
```
curl http://127.0.0.1:8080/api/v1/me -H "Authorization: Bearer vs_..."
```

### `POST /api/v1/invites`
Creates an invite code.

- **Auth:** session token; permission `manage_invites`
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

- **Auth:** session token; permission `manage_invites`
- **Response `200 OK`:** `{ "invites": [ <invite object>, ... ] }` (an empty list is `[]`).
- **Errors:** `unauthorized` (401), `forbidden` (403).

### `DELETE /api/v1/invites/{id}`
Deletes an invite. Its code stops working immediately (use this if a code leaked). Accounts already created with it are not affected.

- **Auth:** session token; permission `manage_invites`
- **Response `204 No Content`:** no body.
- **Errors:** `unauthorized` (401), `forbidden` (403), `not_found` (404).

### Channel and message rules
| Field | Rules |
|---|---|
| Channel `name` | 1-32 characters after trimming spaces; any language and emoji; no control or invisible formatting characters. Unique, ignoring case. |
| Channel `topic` | 0-120 characters, same character rules as names. |
| Message `content` | 1-4000 characters; must contain more than whitespace. Line breaks (`\n`) and tabs are allowed; `\r\n` is stored as `\n`. No other control characters. Leading and trailing spaces are kept. |

Every new server has one channel, **General**. Each channel has a **minimum role to see it** (`view_role`) and a **minimum role to write in it** (`send_role`, never lower than `view_role`). Examples: an announcements channel (`view_role: member`, `send_role: moderator`) is read-only for members; a staff channel (`view_role: moderator`) is invisible to members. Only users with the `manage_channels` permission can create, change, or delete channels.

**A channel you cannot see does not exist for you:** it is missing from `GET /channels`, and its endpoints answer `404 not_found` (never `403`), so its existence is not revealed. Writing in a channel you can see but not write in answers `403 read_only`.

Managers can only change or delete channels they can see themselves, and cannot set `view_role` or `send_role` above their own role (`403 forbidden`).

#### Channel object
| Field | Type | Meaning |
|---|---|---|
| `id` | integer | Channel ID. |
| `name` | string | Display name, e.g. `General`. |
| `topic` | string | Short description; may be empty. |
| `type` | string | Always `text` in this version (`voice` comes later). Clients should ignore channels of types they do not know. |
| `position` | integer | Sort order in the room list, lowest first. |
| `view_role` | string | Minimum role to see the channel: `owner`, `admin`, `moderator`, or `member`. |
| `send_role` | string | Minimum role to write in it. Compare with your own role (order: owner > admin > moderator > member) to decide whether to show the message box. |
| `last_message` | object or null | Preview of the newest message: `author_name` (string; empty if the account was deleted), `content` (at most 100 characters, then `…`), `created_at` (RFC 3339, UTC). `null` if the channel has no messages. Only in `GET /channels`; `null` in create/update responses. |

#### Message object
| Field | Type | Meaning |
|---|---|---|
| `id` | integer | Message ID. Newer messages always have higher IDs. |
| `channel_id` | integer | Channel it belongs to. |
| `author` | object or null | `id`, `username`, `display_name` of the sender; `null` if the account was deleted. |
| `content` | string | The text. Display it as plain text, never as markup. |
| `created_at` | string | Send time, RFC 3339 in UTC. |
| `deleted` | boolean | `true` if its author or a moderator deleted it. Then `content` is `""` (the text is erased on the server, not hidden); show a placeholder such as "Message deleted". `author` stays, so the conversation still makes sense. |
| `edited_at` | string or null | When the author last edited the text (RFC 3339, UTC); `null` if never edited. Show a small "(edited)" mark. |
| `reply_to` | object or null | If this message is a **reply**: a short quote of the message it answers, with `id`, `author` (as above; `null` if that account was deleted), `content` (at most 100 characters, then `…`; `""` if the original was deleted), and `deleted` (boolean). Show it above the message; the `id` lets the client jump to the original. `null` for normal messages. When the original is later edited or deleted, the stored reply is not changed, but clients receive [`message.updated`](#messageupdated) / [`message.deleted`](#messagedeleted) for the original and can update quotes they show. |

### `GET /api/v1/channels`
Lists all channels in room-list order, each with a preview of its newest message.

- **Auth:** session token
- **Response `200 OK`:**

```json
{
  "channels": [
    {
      "id": 1, "name": "General", "topic": "Everything and nothing", "type": "text", "position": 0,
      "last_message": { "author_name": "Sara", "content": "Anyone up for a round tonight?", "created_at": "2026-10-04T18:41:00Z" }
    },
    { "id": 2, "name": "Games", "topic": "", "type": "text", "position": 1, "last_message": null }
  ]
}
```

- **Errors:** `unauthorized` (401).

### `POST /api/v1/channels`
Creates a text channel at the end of the list.

- **Auth:** session token; permission `manage_channels`
- **Request body:** `{ "name": "Games", "topic": "Who is online tonight?", "view_role": "member", "send_role": "member" }` (`topic` optional; `view_role` defaults to `member`; `send_role` defaults to `view_role`)
- **Response `201 Created`:** `{ "channel": <channel object> }`
- **Errors:** `invalid_request` (400), `invalid_name` (400), `invalid_topic` (400), `unauthorized` (401), `forbidden` (403), `channel_name_taken` (409).
- Also sent to every connected client as a [`channel.created`](#channelcreated-channelupdated-channeldeleted) event.

### `PATCH /api/v1/channels/{id}`
Renames a channel and/or changes its topic. Send only the fields to change.

- **Auth:** session token; permission `manage_channels`
- **Request body:** any of `name`, `topic`, `view_role`, `send_role`, e.g. `{ "view_role": "moderator" }`. Raising `view_role` alone also raises `send_role` to at least the same role.
- **Response `200 OK`:** `{ "channel": <channel object> }`
- **Errors:** `invalid_request` (400), `invalid_name` (400), `invalid_topic` (400), `unauthorized` (401), `forbidden` (403), `not_found` (404), `channel_name_taken` (409).

### `DELETE /api/v1/channels/{id}`
Deletes a channel **and all its messages**. This cannot be undone.
Connected clients receive `channel.deleted`; renames (PATCH) send `channel.updated`.

- **Auth:** session token; permission `manage_channels`
- **Response `204 No Content`:** no body.
- **Errors:** `unauthorized` (401), `forbidden` (403), `not_found` (404).

### `GET /api/v1/channels/{id}/messages`
Returns one page of a channel's history, **oldest first** within the page.

- **Auth:** session token
- **Query parameters:**

| Parameter | Required | Meaning |
|---|---|---|
| `before` | no | Return only messages with an ID lower than this (older). Omit to get the newest page. |
| `limit` | no | Page size, 1-100. Default 50. |

- **Response `200 OK`:**

```json
{
  "messages": [
    { "id": 41, "channel_id": 1, "author": { "id": 2, "username": "sara", "display_name": "Sara" },
      "content": "Anyone up for a round tonight?", "created_at": "2026-10-04T18:41:00Z",
      "deleted": false, "edited_at": null, "reply_to": null },
    { "id": 42, "channel_id": 1, "author": { "id": 1, "username": "osama", "display_name": "Osama" },
      "content": "In 20 minutes.", "created_at": "2026-10-04T18:43:00Z",
      "deleted": false, "edited_at": "2026-10-04T18:44:00Z",
      "reply_to": { "id": 41, "author": { "id": 2, "username": "sara", "display_name": "Sara" },
                    "content": "Anyone up for a round tonight?", "deleted": false } }
  ],
  "has_more": true
}
```

- `has_more` is `true` when older messages exist.
- **Loading older messages** ("infinite scroll up"): call again with `before` = the `id` of the oldest message you have (the first one in the list). Repeat until `has_more` is `false`. This is keyset pagination: pages never shift or repeat when new messages arrive meanwhile.
- **Errors:** `invalid_before` (400), `invalid_limit` (400), `unauthorized` (401), `not_found` (404).

Example:
```
curl "http://127.0.0.1:8080/api/v1/channels/1/messages?limit=50" -H "Authorization: Bearer vs_..."
```

### `POST /api/v1/channels/{id}/messages`
Sends a message as the logged-in user.

- **Auth:** session token
- **Request body:** `{ "content": "Hello!" }`, or for a reply `{ "content": "Agreed!", "reply_to": 42 }`. `reply_to` (optional) is the id of a message **in the same channel** that is not deleted; otherwise the request fails with `invalid_reply_to`. (A reply cannot quote a message from another channel: that could show text from a channel the readers cannot see.)
- **Response `201 Created`:** `{ "message": <message object> }`
- **Errors:** `invalid_request` (400), `invalid_content` (400), `invalid_reply_to` (400), `unauthorized` (401), `read_only` (403), `not_found` (404), `rate_limited` (429).
- **Rate limited** per user (see [Rate limits](#rate-limits)).
- Every connected client (including the sender's other devices) also receives the new message as a [`message.created`](#messagecreated) WebSocket event.

### `PATCH /api/v1/channels/{id}/messages/{mid}`
Edits the text of one of **your own** messages. Nobody can edit someone else's message, not even the owner.

- **Auth:** session token; you must be the author, and you must still be allowed to write in the channel (`send_role`).
- **Request body:** `{ "content": "Fixed the typo" }` (same rules as sending).
- **Response `200 OK`:** `{ "message": <message object> }` with `edited_at` set. Everyone who can see the channel receives [`message.updated`](#messageupdated).
- **Errors:** `invalid_request` (400), `invalid_content` (400), `unauthorized` (401), `forbidden` (403: not your message), `read_only` (403), `not_found` (404: unknown or deleted message, or a channel you cannot see), `rate_limited` (429, shared with sending).

### `DELETE /api/v1/channels/{id}/messages/{mid}`
Deletes a message: its text is erased in the database and a placeholder stays (`deleted: true`).

- **Auth:** session token. **Your own messages:** always (even in a channel that became read-only for you). **Someone else's:** permission `delete_messages`, and the author must be **below your role** (messages of deleted accounts can always be removed). You must be able to see the channel.
- **Response `204 No Content`.** Everyone who can see the channel receives [`message.deleted`](#messagedeleted).
- **Errors:** `unauthorized` (401), `forbidden` (403), `not_found` (404: unknown or already deleted message, or a channel you cannot see).

### Members

#### Member object
| Field | Type | Meaning |
|---|---|---|
| `id` | integer | User ID. |
| `username` | string | Login name (lowercase). |
| `display_name` | string | Name to show. |
| `role` | string | `owner`, `admin`, `moderator`, or `member`. |
| `banned` | boolean | Only for viewers with `ban_members`: whether the account is banned. |
| `banned_at` | string | Only for viewers with `ban_members`, if banned: RFC 3339 time. |
| `ban_reason` | string | Only for viewers with `ban_members`, if banned: the reason (may be empty). |

### `GET /api/v1/users`
Lists all accounts, sorted by username.

- **Auth:** session token (any role)
- **Response `200 OK`:** `{ "users": [ <member object>, ... ] }`
- **Errors:** `unauthorized` (401).

### `PATCH /api/v1/users/{id}`
Changes a user's role.

- **Auth:** session token; permission `manage_roles`, and both the user's current role and the new role must be **below yours** (see [Roles and permissions](#roles-and-permissions)). Nobody can be made `owner`.
- **Request body:** `{ "role": "moderator" }`
- **Response `200 OK`:** `{ "user": <member object> }`. Everyone connected receives [`member.updated`](#memberupdated). The change applies at once, also to the user's open sessions.
- **Errors:** `invalid_request` (400), `invalid_role` (400), `unauthorized` (401), `forbidden` (403), `not_found` (404).

### `POST /api/v1/users/{id}/kick`
Signs the user out on every device. They can log in again.

- **Auth:** session token; permission `kick_members`, and the user must be below you.
- **Response `204 No Content`.** Their WebSocket connections close with code `4001`.
- **Errors:** `unauthorized` (401), `forbidden` (403), `not_found` (404).

### `POST /api/v1/users/{id}/ban`
Blocks the user from logging in and signs them out everywhere. Their messages stay.

- **Auth:** session token; permission `ban_members`, and the user must be below you.
- **Request body:** `{ "reason": "spamming" }` (`reason` optional, at most 200 characters; shown to the banned user when they try to log in).
- **Response `204 No Content`.** Their WebSocket connections close with code `4001`; their logins fail with `account_banned`.
- **Errors:** `invalid_request` (400), `invalid_reason` (400), `unauthorized` (401), `forbidden` (403), `not_found` (404).

### `DELETE /api/v1/users/{id}/ban`
Unbans the user (they can log in again).

- **Auth:** session token; permission `ban_members`, and the user must be below you.
- **Response `204 No Content`.**
- **Errors:** `unauthorized` (401), `forbidden` (403), `not_found` (404).

---

## 5. WebSocket

The WebSocket delivers **live events**: new messages, room changes, who is online, and who is typing. Everything else (sending messages, loading history) uses the REST API; the WebSocket only pushes.

### Connecting
```
GET /api/v1/ws
Authorization: Bearer vs_...
```
- Use `ws://` for an `http://` server and `wss://` for an `https://` server, same host and port.
- Authenticate with the **same `Authorization` header** as REST. Do not put the token in the URL: URLs end up in logs.
- Failures before the upgrade are normal HTTP errors in the [standard error format](#2-standard-error-format): `401 unauthorized` (missing or invalid token) or `429 too_many_connections` (more than **10** open connections for this account).
- Browsers on other websites cannot connect: a request with an `Origin` header that does not match the server's host is refused with `403` (protection against cross-site WebSocket hijacking). Native apps send no `Origin` and are not affected.

### Message format
Every message, in both directions, is one JSON text frame:
```json
{ "type": "message.created", "data": { ... } }
```
Clients must **ignore unknown `type` values** (newer servers may add events).

### Keepalive and limits
- The server sends a WebSocket **ping every 30 seconds**. A connection that does not answer within 10 seconds is closed. Standard WebSocket libraries answer pings automatically.
- Client messages may be at most **4096 bytes**; a bigger one closes the connection with code `1009`.
- Clients may send at most **10 messages per second** on average (short bursts of up to 20 are fine). More closes the connection with code `1008`.
- If a client reads events too slowly (more than 64 waiting), the server closes it with code `4008`.

### Close codes
| Code | Meaning | What the client should do |
|---|---|---|
| `1000` | Normal close | Nothing (reconnect if it was not you who closed it). |
| `1001` | Server shutting down | Reconnect with backoff. |
| `1009` | Your message was too big | Fix the client; reconnect. |
| `1008` | You sent too many messages too fast | Fix the client; reconnect with backoff. |
| `4001` | Session ended (logged out, kicked, banned, or revoked) | **Do not reconnect.** Delete the token and show the login screen. |
| `4008` | Too slow reading events | Reconnect, then reload what you show (you missed events). |
| other / no code | Network problem | Reconnect with backoff. |

### Reconnecting
Connections drop (Wi-Fi, sleep, server restart). Clients should:
1. Reconnect automatically, waiting a little longer after each failure (for example 1, 2, 4, 8 ... up to 30 seconds, plus a random part so many clients do not reconnect at the same moment).
2. If the reconnect fails, call `GET /api/v1/me`: a `401` means the session is gone (log out locally and stop reconnecting).
3. After reconnecting, **reload** the room list and the newest page of the open room: events sent while disconnected are not replayed.

### Server → client events

#### `ready`
First event after connecting.
```json
{ "type": "ready", "data": {
    "user": { "id": 1, "username": "osama", "display_name": "Osama" },
    "online": [ { "id": 1, "username": "osama", "display_name": "Osama" },
                { "id": 2, "username": "sara",  "display_name": "Sara" } ] } }
```
`online` lists every user with at least one open connection, including you.

#### `presence.updated`
A user came online (first connection) or went offline (last connection closed). Extra devices of an already-online user do not trigger it.
```json
{ "type": "presence.updated", "data": {
    "user": { "id": 2, "username": "sara", "display_name": "Sara" }, "online": false } }
```

#### `message.created`
A new message in any channel. `data` is a [message object](#message-object). The sender receives it too (it may arrive before or after the REST response); use the message `id` to avoid showing it twice.
```json
{ "type": "message.created", "data": {
    "id": 43, "channel_id": 1,
    "author": { "id": 2, "username": "sara", "display_name": "Sara" },
    "content": "On my way!", "created_at": "2026-10-04T18:44:00Z",
    "deleted": false, "edited_at": null, "reply_to": null } }
```

#### `channel.created`, `channel.updated`, `channel.deleted`
A manager created, changed, or deleted a channel. These events (and `message.created`, `typing.started`) are only sent to users whose role may **see** that channel. If a change hides a channel from you, you receive `channel.deleted` for it: remove it from your list. If a change reveals a channel to you, you receive `channel.updated` for a channel you did not know yet: add it (or simply reload the list on any of these events). For `created` and `updated`, `data` is a [channel object](#channel-object) (`last_message` is `null`); for `deleted` it is `{ "id": 3 }`.
```json
{ "type": "channel.deleted", "data": { "id": 3 } }
```

#### `message.updated`
A message was edited by its author. `data` is the full [message object](#message-object) (with `edited_at`): replace your copy.
```json
{ "type": "message.updated", "data": { "id": 43, "channel_id": 1, "author": { "id": 2, "username": "sara", "display_name": "Sara" },
    "content": "Fixed the typo", "created_at": "2026-10-04T18:30:00Z", "deleted": false,
    "edited_at": "2026-10-04T18:31:00Z", "reply_to": null } }
```

#### `message.deleted`
A message was deleted by its author or a moderator. Replace it with a placeholder (or remove it).
```json
{ "type": "message.deleted", "data": { "id": 43, "channel_id": 1 } }
```

#### `member.updated`
A user's role changed. `data` is a [member object](#member-object) (without ban details). If it is **you**, reload your own user (`GET /api/v1/me`) and the room list: your permissions and the channels you can see may have changed.
```json
{ "type": "member.updated", "data": { "id": 2, "username": "sara", "display_name": "Sara", "role": "moderator" } }
```

#### `typing.started`
Someone is typing in a channel. Show it for about **5 seconds**; repeated events keep it alive. You never receive your own typing.
```json
{ "type": "typing.started", "data": {
    "channel_id": 1, "user": { "id": 2, "username": "sara", "display_name": "Sara" } } }
```

### Client → server events

#### `typing`
Send while the user is typing in a channel, **at most every 3 seconds**. The server forwards it to everyone else as `typing.started`: at most once every 2 seconds per channel, and at most once per second per connection overall. Extra ones are dropped.
```json
{ "type": "typing", "data": { "channel_id": 1 } }
```
Unknown or malformed client messages are ignored.

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
| `POST /channels/{id}/messages` and `PATCH /channels/{id}/messages/{mid}` (shared) | 10 messages at once, then 1 more per second, **per user** |
| `GET /ws` | at most 10 open connections per account; client messages at most 4096 bytes and 10 per second (bursts of 20); `typing` forwarded at most every 2 s per channel and 1 s overall |

Over the limit the server answers `429 rate_limited` with a `Retry-After` header (seconds). Refused requests do not count against the limit.

---

## Changelog

### Protocol version 1 (in development)
- Added `GET /api/v1/health` (reports database status; 503 when the database is unreachable).
- Added `GET /api/v1/info`.
- Added the standard error format.
- Added `POST /api/v1/register`, session tokens, owner setup, invites, and account rules.
- Added `POST /api/v1/login`, `POST /api/v1/logout`, `GET /api/v1/me`, bearer token authentication, and rate limits on register and login.
- Added `POST /api/v1/invites`, `GET /api/v1/invites`, `DELETE /api/v1/invites/{id}` (owner only at the time; see roles below).
- Added channels (`GET`, `POST /api/v1/channels`, `PATCH`, `DELETE /api/v1/channels/{id}`) and messages (`GET`, `POST /api/v1/channels/{id}/messages`) with keyset pagination.
- Added the WebSocket at `GET /api/v1/ws` with events `ready`, `presence.updated`, `message.created`, `channel.created`, `channel.updated`, `channel.deleted`, `typing.started` (server to client) and `typing` (client to server).
- All responses now send `Cache-Control: no-store`; request bodies are limited to 64 KiB and parsed strictly.
- WebSocket: clients sending more than 10 messages per second are closed with `1008`; `typing` is also limited per connection. `invalid_request` messages no longer include internal details.
- Added roles (`owner`, `admin`, `moderator`, `member`) and `role` + `permissions` in user objects. Invites and channel management now need the `manage_invites` / `manage_channels` permission (owner and admins) instead of being owner-only.
- Added members: `GET /api/v1/users`, `PATCH /api/v1/users/{id}` (role), `POST /api/v1/users/{id}/kick`, `POST`/`DELETE /api/v1/users/{id}/ban`; WebSocket event `member.updated`; login error `account_banned`.
- Added per-channel `view_role` and `send_role` (channel object, create, update); hidden channels answer 404; new error `read_only`; live events are only sent to users who may see the channel.
- Added `DELETE /api/v1/channels/{id}/messages/{mid}`, the message field `deleted`, and the WebSocket event `message.deleted`. Room-list previews skip deleted messages.
- Added replies (`reply_to` in the send request and in message objects), message editing (`PATCH /api/v1/channels/{id}/messages/{mid}`, field `edited_at`, WebSocket event `message.updated`), and deleting your own messages. New error `invalid_reply_to`.
