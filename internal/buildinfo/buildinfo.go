// Package buildinfo holds the server and protocol versions.
package buildinfo

// Version is the server's own version (semantic versioning: MAJOR.MINOR.PATCH, plus a
// pre-release tag). Keep it the same as the client's version: apps refuse to connect to a
// server with a different MAJOR number. A build can still override it with
// -ldflags "-X github.com/5cfp/vianden-server/internal/buildinfo.Version=1.2.3".
var Version = "0.3.0-alpha.3"

// ProtocolVersion is the version of the client/server API described in docs/API.md.
// Increase it on every breaking API change. Clients compare it via GET /api/v1/info.
const ProtocolVersion = 1
