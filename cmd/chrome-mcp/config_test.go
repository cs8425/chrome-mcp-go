package main

import (
	"encoding/json"
	"testing"
)

func strp(s string) *string { return &s }
func intptr(i int) *int     { return &i }
func boolp(b bool) *bool    { return &b }

func TestDefaults(t *testing.T) {
	s := defaultSettings()
	if s.HTTP != "127.0.0.1:8766" || s.CDP != "127.0.0.1:9222" || s.BrowserVRoot != "/" {
		t.Fatalf("bad defaults: %+v", s)
	}
	if !s.Packs.Core || !s.Packs.Browse || !s.Packs.Dev || !s.Packs.Files || s.Packs.Perf {
		t.Errorf("bad default packs: %+v", s.Packs)
	}
	// Timeout/focus defaults: per-CDP-call 60s; whole-request OFF (long tools
	// like evaluate need room); structural select/new never raise the window.
	if s.CDPCallTimeoutSec != 60 || s.RequestTimeoutSec != 0 || s.ActivateOnSelect {
		t.Errorf("bad timeout/focus defaults: cdp_call=%d request=%d activate=%v",
			s.CDPCallTimeoutSec, s.RequestTimeoutSec, s.ActivateOnSelect)
	}
}

func TestApplyFilePartialOverlay(t *testing.T) {
	s := defaultSettings()
	var fc fileConfig
	doc := `{"cdp":"10.0.0.1:9222","packs":{"dev":false}}`
	if err := json.Unmarshal([]byte(doc), &fc); err != nil {
		t.Fatal(err)
	}
	s.applyFileConfig(&fc)
	if s.CDP != "10.0.0.1:9222" {
		t.Errorf("cdp not overlaid: %s", s.CDP)
	}
	if s.Packs.Dev {
		t.Error("packs.dev should be false from file")
	}
	// keys not in the doc keep their defaults
	if !s.Packs.Core || s.HTTP != "127.0.0.1:8766" {
		t.Errorf("unspecified keys changed: %+v", s)
	}
}

func TestEnvBeatsFile(t *testing.T) {
	s := defaultSettings()
	s.applyFileConfig(&fileConfig{CDP: strp("from-file")})
	env := map[string]string{
		"CDPMCP_CDP":           "from-env",
		"CDPMCP_MAX_BYTES":     "1234",
		"CDPMCP_PACK_DEV":      "false",
		"CDPMCP_PACK_PERF":     "true",
		"CDPMCP_BROWSER_VROOT": "/dl",
	}
	s.applyEnv(func(k string) string { return env[k] })
	if s.CDP != "from-env" {
		t.Errorf("env should win over file: %s", s.CDP)
	}
	if s.MaxBytes != 1234 {
		t.Errorf("max_bytes env: %d", s.MaxBytes)
	}
	if s.Packs.Dev || !s.Packs.Perf {
		t.Errorf("pack env: %+v", s.Packs)
	}
	if s.BrowserVRoot != "/dl" {
		t.Errorf("browser_vroot env: %s", s.BrowserVRoot)
	}
}

func TestEnvEmptyIgnored(t *testing.T) {
	s := defaultSettings()
	s.applyEnv(func(string) string { return "" }) // all empty -> nothing changes
	if s != defaultSettings() {
		t.Errorf("empty env mutated settings: %+v", s)
	}
}

func TestParseBool(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE", "yes", "y", "on", "t"} {
		if !parseBool(v) {
			t.Errorf("parseBool(%q)=false want true", v)
		}
	}
	for _, v := range []string{"", "0", "false", "no", "off", "n", "garbage"} {
		if parseBool(v) {
			t.Errorf("parseBool(%q)=true want false", v)
		}
	}
}

// TestTimeoutAndFocusLayers covers cdp_call_timeout_seconds,
// request_timeout_seconds and activate_on_select across the file and env layers
// (env wins over file), the pointer-zero handling for an explicit 0, and that
// an empty env leaves them untouched.
func TestTimeoutAndFocusLayers(t *testing.T) {
	// --- file layer ---
	s := defaultSettings()
	var fc fileConfig
	doc := `{"cdp_call_timeout_seconds":30,"request_timeout_seconds":90,"activate_on_select":true}`
	if err := json.Unmarshal([]byte(doc), &fc); err != nil {
		t.Fatal(err)
	}
	s.applyFileConfig(&fc)
	if s.CDPCallTimeoutSec != 30 || s.RequestTimeoutSec != 90 || !s.ActivateOnSelect {
		t.Fatalf("file overlay wrong: cdp_call=%d request=%d activate=%v",
			s.CDPCallTimeoutSec, s.RequestTimeoutSec, s.ActivateOnSelect)
	}

	// An explicit request_timeout_seconds:0 from file must be honoured (the
	// pointer distinguishes "unset" from "explicitly off").
	var off fileConfig
	if err := json.Unmarshal([]byte(`{"request_timeout_seconds":0}`), &off); err != nil {
		t.Fatal(err)
	}
	s2 := defaultSettings()
	s2.RequestTimeoutSec = 500 // pretend a higher prior value
	s2.applyFileConfig(&off)
	if s2.RequestTimeoutSec != 0 {
		t.Errorf("explicit request_timeout_seconds:0 from file ignored: %d", s2.RequestTimeoutSec)
	}

	// --- env layer, env beats file ---
	s3 := defaultSettings()
	s3.applyFileConfig(&fileConfig{
		CDPCallTimeoutSec: intptr(10),
		RequestTimeoutSec: intptr(20),
		ActivateOnSelect:  boolp(false),
	})
	env := map[string]string{
		"CDPMCP_CDP_CALL_TIMEOUT":   "45",
		"CDPMCP_REQUEST_TIMEOUT":    "900",
		"CDPMCP_ACTIVATE_ON_SELECT": "true",
	}
	s3.applyEnv(func(k string) string { return env[k] })
	if s3.CDPCallTimeoutSec != 45 || s3.RequestTimeoutSec != 900 || !s3.ActivateOnSelect {
		t.Errorf("env should win over file: cdp_call=%d request=%d activate=%v",
			s3.CDPCallTimeoutSec, s3.RequestTimeoutSec, s3.ActivateOnSelect)
	}

	// empty env leaves values untouched (including a non-zero request timeout).
	base := defaultSettings()
	base.RequestTimeoutSec = 120
	base.applyEnv(func(string) string { return "" })
	if base.RequestTimeoutSec != 120 || base.CDPCallTimeoutSec != 60 || base.ActivateOnSelect {
		t.Errorf("empty env mutated timeout/focus: %+v", base)
	}
}
