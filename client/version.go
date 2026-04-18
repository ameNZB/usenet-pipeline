package client

// AgentProtocolVersion is an integer that identifies the API contract this
// agent binary speaks with the site. Bump it any time a breaking change is
// made to what the agent sends or expects in return (request shape, required
// fields, status values, etc). The site advertises its own minimum required
// version; anything below that gets 426 Upgrade Required and is expected to
// pause until the operator updates the binary.
//
// Changelog:
//   1 — initial versioned protocol. Adds the "aborted" completion status
//       (agent-local errors release the lock without marking failed), and
//       X-Agent-Protocol / X-Agent-Version headers on every request.
//   2 — private torrent uploads. Task payload carries Private + TorrentFileURL
//       fields; when set, the agent MUST fetch the .torrent from the site
//       over HTTPS and MUST NOT resolve the info hash via DHT (which would
//       leak the release off the user's private tracker). Minimum is
//       bumped to v2 so a pre-v2 agent can't silently do the unsafe thing
//       on a private task.
const AgentProtocolVersion = 2

// AgentVersion is the human-readable build string logged on the site when
// this agent polls. Not used for compatibility gating — that's
// AgentProtocolVersion's job — but useful for debugging which agents in the
// field have picked up a release.
const AgentVersion = "1.1.0"
