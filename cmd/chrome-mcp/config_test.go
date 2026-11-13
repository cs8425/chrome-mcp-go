package main

import (
	"encoding/json"
	"testing"
)

func strp(s string) *string { return &s }

func TestDefaults(t *testing.T) {
	s := defaultSettings()
	if s.HTTP != "127.0.0.1:8766" || s.CDP != "127.0.0.1:9222" || s.BrowserVRoot != "/" {
		t.Fatalf("bad defaults: %+v", s)
	}
	if !s.Packs.Core || !s.Packs.Browse || !s.Packs.Dev || !s.Packs.Files || s.Packs.Perf {
		t.Errorf("bad default packs: %+v", s.Packs)
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
