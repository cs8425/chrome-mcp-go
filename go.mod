module chrome-mcp

go 1.27

require (
	github.com/coder/websocket v1.8.15
	mcptransport v0.0.0
)

replace mcptransport => ./mcptransport
