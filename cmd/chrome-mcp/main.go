// chrome-mcp is a pure-Go rewrite of chrome-devtools-mcp. It serves
// action-aggregated Chrome DevTools tools (tabs/dialog/resize in P1) over stdio
// or Streamable HTTP using the mcptransport module, talking CDP to a running
// Chrome via the typed client generated from the pinned protocol JSON.
package main

import (
	"context"
	"log"
	"net/http"
	"os"

	"chrome-mcp/internal/tools"
	"mcptransport"
)

const serverVersion = "0.1.0"

func main() {
	cfg := loadConfig()

	log.SetOutput(os.Stderr)

	ctx := context.Background()
	core, err := tools.NewCore(ctx, cfg.CDP)
	if err != nil {
		log.Fatalf("connect CDP at %s: %v", cfg.CDP, err)
	}
	if cfg.MaxBytes > 0 {
		core.SetMaxBytes(cfg.MaxBytes)
	}
	if cfg.FileRoot != "" {
		if err := core.SetFileRoot(cfg.FileRoot, cfg.BrowserRoot, cfg.BrowserVRoot); err != nil {
			log.Fatalf("open file root %s: %v", cfg.FileRoot, err)
		}
	}

	reg := mcptransport.NewRegistry(cfg.ToolPrefix) // "" = no name prefix
	tools.Register(reg, core)                       // core + browse packs (split in P4 step 2)
	if cfg.Packs.Dev {
		tools.RegisterDev(reg, core) // dev pack
	}
	if cfg.Packs.Files {
		tools.RegisterFiles(reg, core) // files pack (upload/download; needs root for writes)
	}

	srv := &mcptransport.Server{
		Name:         "chrome-mcp",
		Version:      serverVersion,
		Instructions: "Chrome DevTools MCP server (pure Go).",
		Reg:          reg,
		Origin:       map[string]bool{"*": true},
	}

	if cfg.Stdio {
		if err := mcptransport.NewStdioServer(srv).Serve(); err != nil {
			log.Fatal(err)
		}
		return
	}

	log.Printf("chrome-mcp Streamable HTTP on %s (CDP %s)", cfg.HTTP, cfg.CDP)
	if err := http.ListenAndServe(cfg.HTTP, srv.Handler()); err != nil {
		log.Fatal(err)
	}
}
