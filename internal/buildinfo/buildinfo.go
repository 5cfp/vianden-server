// Package buildinfo holds the server and protocol versions.
package buildinfo

// Version is the server's own version. Release builds will override it with
// -ldflags "-X github.com/5cfp/vianden-server/internal/buildinfo.Version=1.2.3".
var Version = "0.1.0-dev"

// ProtocolVersion is the version of the client/server API described in docs/API.md.
// Increase it on every breaking API change. Clients compare it via GET /api/v1/info.
const ProtocolVersion = 1
