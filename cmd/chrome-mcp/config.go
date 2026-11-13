package main

// Config loading for chrome-mcp. Precedence (lowest -> highest):
//   built-in defaults < JSON config file < environment (CDPMCP_*) < CLI flags.
// The config file collapses the growing set of flags into one document and adds
// per-pack toggles. Stdlib only — no external config dependency.

import (
	"encoding/json"
	"flag"
	"log"
	"os"
	"strconv"
	"strings"
)

// Packs selects which tool packs to register. core+browse are the default
// surface; perf stays off unless explicitly enabled.
type Packs struct {
	Core   bool
	Browse bool
	Dev    bool
	Files  bool
	Perf   bool
}

// settings is the fully-resolved runtime configuration.
type settings struct {
	Stdio        bool
	HTTP         string // Streamable HTTP bind address when Stdio is false
	CDP          string // Chrome remote-debugging host:port
	MaxBytes     int
	FileRoot     string
	BrowserRoot  string
	BrowserVRoot string
	ToolPrefix   string
	Packs        Packs
}

func defaultSettings() settings {
	return settings{
		HTTP:         "127.0.0.1:8766",
		CDP:          "127.0.0.1:9222",
		BrowserVRoot: "/",
		Packs:        Packs{Core: true, Browse: true, Dev: true, Files: true},
	}
}

// fileConfig mirrors settings with pointer fields so a JSON document can omit a
// key ("unset") versus set it to a zero value. Only present keys overlay.
type fileConfig struct {
	Stdio        *bool      `json:"stdio"`
	HTTP         *string    `json:"http"`
	CDP          *string    `json:"cdp"`
	MaxBytes     *int       `json:"max_bytes"`
	FileRoot     *string    `json:"file_root"`
	BrowserRoot  *string    `json:"browser_root"`
	BrowserVRoot *string    `json:"browser_vroot"`
	ToolPrefix   *string    `json:"tool_prefix"`
	Packs        *packsFile `json:"packs"`
}

type packsFile struct {
	Core   *bool `json:"core"`
	Browse *bool `json:"browse"`
	Dev    *bool `json:"dev"`
	Files  *bool `json:"files"`
	Perf   *bool `json:"perf"`
}

func (s *settings) applyFileConfig(fc *fileConfig) {
	if fc.Stdio != nil {
		s.Stdio = *fc.Stdio
	}
	if fc.HTTP != nil {
		s.HTTP = *fc.HTTP
	}
	if fc.CDP != nil {
		s.CDP = *fc.CDP
	}
	if fc.MaxBytes != nil {
		s.MaxBytes = *fc.MaxBytes
	}
	if fc.FileRoot != nil {
		s.FileRoot = *fc.FileRoot
	}
	if fc.BrowserRoot != nil {
		s.BrowserRoot = *fc.BrowserRoot
	}
	if fc.BrowserVRoot != nil {
		s.BrowserVRoot = *fc.BrowserVRoot
	}
	if fc.ToolPrefix != nil {
		s.ToolPrefix = *fc.ToolPrefix
	}
	if p := fc.Packs; p != nil {
		if p.Core != nil {
			s.Packs.Core = *p.Core
		}
		if p.Browse != nil {
			s.Packs.Browse = *p.Browse
		}
		if p.Dev != nil {
			s.Packs.Dev = *p.Dev
		}
		if p.Files != nil {
			s.Packs.Files = *p.Files
		}
		if p.Perf != nil {
			s.Packs.Perf = *p.Perf
		}
	}
}

// applyEnv overlays CDPMCP_* environment variables. Missing/empty vars are ignored.
func (s *settings) applyEnv(getenv func(string) string) {
	if v := getenv("CDPMCP_STDIO"); v != "" {
		s.Stdio = parseBool(v)
	}
	setStr(&s.HTTP, getenv("CDPMCP_HTTP"))
	setStr(&s.CDP, getenv("CDPMCP_CDP"))
	setStr(&s.FileRoot, getenv("CDPMCP_ROOT"))
	setStr(&s.BrowserRoot, getenv("CDPMCP_BROWSER_ROOT"))
	setStr(&s.BrowserVRoot, getenv("CDPMCP_BROWSER_VROOT"))
	setStr(&s.ToolPrefix, getenv("CDPMCP_TOOL_PREFIX"))
	if v := getenv("CDPMCP_MAX_BYTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			s.MaxBytes = n
		}
	}
	if v := getenv("CDPMCP_PACK_CORE"); v != "" {
		s.Packs.Core = parseBool(v)
	}
	if v := getenv("CDPMCP_PACK_BROWSE"); v != "" {
		s.Packs.Browse = parseBool(v)
	}
	if v := getenv("CDPMCP_PACK_DEV"); v != "" {
		s.Packs.Dev = parseBool(v)
	}
	if v := getenv("CDPMCP_PACK_FILES"); v != "" {
		s.Packs.Files = parseBool(v)
	}
	if v := getenv("CDPMCP_PACK_PERF"); v != "" {
		s.Packs.Perf = parseBool(v)
	}
}

// loadConfig parses flags (defining them), then resolves file -> env -> CLI.
func loadConfig() settings {
	configPath := flag.String("config", "", "path to JSON config file (default: $CDPMCP_CONFIG or ./chrome-mcp.json if present)")
	address := flag.String("l", "", "bind Streamable HTTP MCP server at this address")
	stdioMode := flag.Bool("stdio", false, "serve MCP over stdio instead of HTTP")
	cdpAddr := flag.String("cdp", "", "Chrome remote-debugging host:port")
	maxBytes := flag.Int("max-bytes", 0, "cap tool-result text at this many bytes (0 = built-in default)")
	rootDir := flag.String("root", "", "confine file reads/writes (screenshot saveAs, upload/download, ...) to this directory")
	browserRoot := flag.String("browser-root", "", "path the BROWSER sees for the shared dir (Chrome in a container, e.g. /tmp/Downloads)")
	browserVRoot := flag.String("browser-vroot", "", "virtual dir of the file box that --browser-root maps to (e.g. /downloads)")
	flag.Parse()

	// Only flags the user actually set override lower layers.
	set := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { set[f.Name] = true })

	cfg := defaultSettings()

	path := *configPath
	if path == "" {
		path = os.Getenv("CDPMCP_CONFIG")
	}
	if path == "" {
		if _, err := os.Stat("chrome-mcp.json"); err == nil {
			path = "chrome-mcp.json"
		}
	}
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			log.Fatalf("read config %s: %v", path, err)
		}
		var fc fileConfig
		if err := json.Unmarshal(b, &fc); err != nil {
			log.Fatalf("parse config %s: %v", path, err)
		}
		cfg.applyFileConfig(&fc)
	}

	cfg.applyEnv(os.Getenv)

	if set["l"] {
		cfg.HTTP = *address
	}
	if set["stdio"] {
		cfg.Stdio = *stdioMode
	}
	if set["cdp"] {
		cfg.CDP = *cdpAddr
	}
	if set["max-bytes"] {
		cfg.MaxBytes = *maxBytes
	}
	if set["root"] {
		cfg.FileRoot = *rootDir
	}
	if set["browser-root"] {
		cfg.BrowserRoot = *browserRoot
	}
	if set["browser-vroot"] {
		cfg.BrowserVRoot = *browserVRoot
	}
	return cfg
}

func setStr(dst *string, v string) {
	if v != "" {
		*dst = v
	}
}

func parseBool(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "t", "yes", "y", "on":
		return true
	default:
		return false
	}
}
