# chrome-mcp-go

A pure-Go rewrite of a Chrome DevTools MCP server. It speaks the
[Model Context Protocol](https://modelcontextprotocol.io) (Streamable HTTP +
stdio) and drives a running Chrome via the Chrome DevTools Protocol (CDP) using
a typed client generated from the pinned protocol JSON.

No CGO. Go 1.27+.

## Architecture

```
   +-------------------+   MCP: stdio (pipes)    +----------------------+
   |                   |   or Streamable HTTP    |                      |
   |   MCP client /    |  ---------------------> |    chrome-mcp-go     |
   |      LLM          |   (tools/list,          |      (this repo)     |
   |                   |    tools/call)          |                      |
   +-------------------+                         +-----------+----------+
                                                            |
                                                            | CDP over
                                                            | WebSocket
                                                            v
                                                +-----------+----------+
                                                |  Chrome / Chromium    |
                                                |  --remote-debugging-  |
                                                |       port=9222       |
                                                |   (may run in Docker) |
                                                +-----------------------+
```

## Tools

Grouped into packs that can be enabled/disabled independently (see
[Configuration](#configuration)):

| pack | default | tools |
|---|---|---|
| core | on | `tabs`, `dialog`, `window` |
| browse | on | `pointer`, `input`, `scroll`, `wait`, `read`, `find`, `interact` |
| dev | on | `evaluate`, `console`, `emulate`, `network`, `storage`, `raw_call` |
| files | on | `upload`, `download` |
| perf | off | *(planned: trace / heapsnapshot)* |

## Build

```sh
cd chrome-mcp-go
go build ./...
go test ./...
(cd mcptransport && go test ./...)
```

## Run

The server talks to a Chrome that is already running with remote debugging:

```sh
chromium --remote-debugging-port=9222
```

### stdio

```sh
go run ./cmd/chrome-mcp -stdio -cdp 127.0.0.1:9222
```

### Streamable HTTP

```sh
go run ./cmd/chrome-mcp -l 127.0.0.1:8766 -cdp 127.0.0.1:9222
```

Default bind when HTTP is selected is `127.0.0.1:8766`. Bind to localhost unless
you add authentication and understand the exposure (the server applies an
Origin allow-list as a DNS-rebinding defence but has no auth token today).

## Configuration

Settings resolve with this precedence (**lowest → highest**):

```
built-in defaults  <  JSON config file  <  environment (CDPMCP_*)  <  CLI flags
```

The config file is located by `-config <path>`, else `$CDPMCP_CONFIG`, else
`./chrome-mcp.json` if it exists. See [`config.sample.json`](./config.sample.json).

```json
{
  "stdio": false,
  "http": "127.0.0.1:8766",
  "cdp": "127.0.0.1:9222",
  "max_bytes": 0,
  "file_root": "/path/to/workspace",
  "browser_root": "/tmp/Downloads",
  "browser_vroot": "/downloads",
  "tool_prefix": "",
  "packs": { "core": true, "browse": true, "dev": true, "files": true, "perf": false }
}
```

### Flags

| flag | config key | env | default | meaning |
|---|---|---|---|---|
| `-config` | — | `CDPMCP_CONFIG` | `./chrome-mcp.json` | config file path |
| `-stdio` | `stdio` | `CDPMCP_STDIO` | false | serve over stdio instead of HTTP |
| `-l` | `http` | `CDPMCP_HTTP` | `127.0.0.1:8766` | HTTP bind address |
| `-cdp` | `cdp` | `CDPMCP_CDP` | `127.0.0.1:9222` | Chrome remote-debugging host:port |
| `-max-bytes` | `max_bytes` | `CDPMCP_MAX_BYTES` | 0 (built-in) | cap tool-result text bytes |
| `-root` | `file_root` | `CDPMCP_ROOT` | `""` (writes disabled) | jail for all file I/O |
| `-browser-root` | `browser_root` | `CDPMCP_BROWSER_ROOT` | `""` (same FS) | path the browser sees for `browser_vroot` |
| `-browser-vroot` | `browser_vroot` | `CDPMCP_BROWSER_VROOT` | `/` | virtual dir `browser_root` maps to |
| — | `tool_prefix` | `CDPMCP_TOOL_PREFIX` | `""` | prefix added to every tool name |
| — | `packs.*` | `CDPMCP_PACK_{CORE,BROWSE,DEV,FILES,PERF}` | see table | enable/disable a pack |

## File sandbox and path model

All file-touching tools (`read` `saveAs`, `upload`, `download`, and the planned
`storage` state) are jailed to `-root`. Paths in tool arguments and results are
**virtual**, always `/`-rooted; the real host path is never shown. `..` escapes
are refused.

Everything the agent touches is confined to the jail. Only the `browser_vroot`
subtree is shared with the browser:

```
  -root  (jail; all file I/O is confined here)
  +--------------------------------------------------------------+
  |  workspace           (host filesystem - agent reads/writes)  |
  |                                                              |
  |     +------------------------------+                         |
  |     | /downloads   = browser_vroot |  <- only this subtree   |
  |     |   visible to the browser     |     is bind-mounted     |
  |     +------------------------------+     into the container   |
  |                                                              |
  |   /shot.png   /probe.txt   ...   (host-only, NOT shared)     |
  +--------------------------------------------------------------+
```

The same file has three names — one per filesystem — kept in sync by the
`-browser-root` / `-browser-vroot` mapping:

```
  VIRTUAL (LLM/tools)     HOST (-root = .../workspace)      CHROME (container)
  -------------------     ----------------------------      ------------------
  /downloads/up.txt  ==   workspace/downloads/up.txt    ==  /tmp/Downloads/up.txt
  /downloads/a/b.pdf ==   workspace/downloads/a/b.pdf   ==  /tmp/Downloads/a/b.pdf
  /shot.png          ==   workspace/shot.png            ==  (not visible)
```

Uploads from outside the shared subtree are refused instead of being handed a
path the container cannot resolve (which would crash the renderer):

```
  upload  /probe.txt
     |
     +-- under browser_vroot (/downloads)?  -->  NO
     |
     +-- REFUSE: "not accessible to the browser (only /downloads is shared)"
```

To configure the mapping when Chrome runs in a container sharing only one
subdirectory:

- `-root` — the host directory that is jailed (this is also where the agent writes).
- `-browser-root` — the path **inside the browser/container** for that shared dir.
- `-browser-vroot` — the **virtual** subdir (under `-root`) that maps to `-browser-root`.

Example (host `…/workspace/downloads` bind-mounted to container `/tmp/Downloads`):

```sh
docker run ... -v "$PWD/workspace/downloads:/tmp/Downloads" chromium --remote-debugging-port=9222

go run ./cmd/chrome-mcp -cdp 127.0.0.1:9222 \
  --root "$PWD/workspace" \
  --browser-root /tmp/Downloads \
  --browser-vroot /downloads
```

Then virtual `/downloads/file.bin` == host `workspace/downloads/file.bin` ==
container `/tmp/Downloads/file.bin`.

## MCP protocol support

The HTTP transport implements the modern per-request-metadata revision
`2026-07-28` (POST-only, `Mcp-Method`/`Mcp-Name`/`MCP-Protocol-Version` headers,
`server/discover`) and falls back to the legacy `initialize` handshake
`2025-11-25`. Responses are `application/json`; there is no SSE server-push,
resources, or prompts (this is a tool-only server).

## Reconnecting after a browser restart

If Chrome is restarted while the server is running, the DevTools WebSocket
connection drops. The server reconnects **lazily**: the next tool call
detects the dead connection, transparently re-dials the endpoint, and
re-adopts a tab — you do not need to restart the server. A call that was
in-flight when the browser went away fails once (`ErrClosed`); the following
call recovers. If Chrome is not back up yet the call returns
`browser unavailable: …` and retries on the next call. (Startup still requires
Chrome to be running: the server dials once at start.)

## Regenerating the CDP client

`internal/cdp/cdp_*_gen.go` is generated from the vendored protocol JSON and
must not be edited by hand. To bump the protocol or after changing the
generator:

```sh
cd chrome-mcp-go
go generate ./...
gofmt -w ./internal/cdp
go build ./... && go test ./...
```

See [`docs/protocol-gen.md`](./docs/protocol-gen.md) for where the protocol JSON
comes from and the full refresh workflow, and
[`internal/cdp/protocol/README.md`](./internal/cdp/protocol/README.md) for the
provenance of the vendored schema.

## License

The original source in this repository is licensed under the
[Apache License 2.0](./LICENSE).

The vendored Chrome DevTools Protocol schema
(`internal/cdp/protocol/*.json`) and the Go code generated from it
(`internal/cdp/cdp_*_gen.go`) derive from Chromium and remain under Chromium's
BSD 3-Clause license — see
[`internal/cdp/protocol/LICENSE`](./internal/cdp/protocol/LICENSE). The two
licenses are compatible.
