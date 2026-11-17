package tools

// The dev pack (cont.): console. The tab's console.log/warn/error output and
// uncaught exceptions are captured as CDP events and buffered per target; the
// `console` tool reads or clears that buffer.

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"

	"chrome-mcp/internal/cdp"
	"mcptransport"
)

const maxConsoleLines = 500

// startConsoleCapture subscribes a tab's session to console/exception/log events,
// buffering formatted lines under targetID. Subscriptions are per-target and live
// for the tab's whole life, so a tab keeps capturing even when it is not the
// current tab; re-selecting an already-captured tab is a no-op (no duplicate
// entries, no repeated Log replay).
func (c *Core) startConsoleCapture(ctx context.Context, page *cdp.Client, targetID string) {
	if !c.claimConsole(targetID) {
		return
	}
	var unsubs []func()

	unsubs = append(unsubs, page.Subscribe("Runtime.consoleAPICalled", func(params json.RawMessage) {
		var e struct {
			Type string `json:"type"`
			Args []struct {
				Type        string          `json:"type"`
				Subtype     string          `json:"subtype"`
				Value       json.RawMessage `json:"value"`
				Description string          `json:"description"`
			} `json:"args"`
		}
		if json.Unmarshal(params, &e) != nil {
			return
		}
		parts := make([]string, 0, len(e.Args))
		for _, a := range e.Args {
			switch {
			case len(a.Value) > 0:
				parts = append(parts, string(a.Value))
			case a.Description != "":
				parts = append(parts, a.Description)
			default:
				parts = append(parts, a.Type)
			}
		}
		c.appendConsole(targetID, "["+e.Type+"] "+strings.Join(parts, " "))
	}))

	unsubs = append(unsubs, page.Subscribe("Runtime.exceptionThrown", func(params json.RawMessage) {
		var e struct {
			ExceptionDetails struct {
				Text      string `json:"text"`
				Exception struct {
					Description string `json:"description"`
				} `json:"exception"`
			} `json:"exceptionDetails"`
		}
		if json.Unmarshal(params, &e) != nil {
			return
		}
		msg := e.ExceptionDetails.Text
		if e.ExceptionDetails.Exception.Description != "" {
			msg = e.ExceptionDetails.Exception.Description
		}
		c.appendConsole(targetID, "[exception] "+msg)
	}))

	unsubs = append(unsubs, page.Subscribe("Log.entryAdded", func(params json.RawMessage) {
		var e struct {
			Entry struct {
				Level  string `json:"level"`
				Source string `json:"source"`
				Text   string `json:"text"`
			} `json:"entry"`
		}
		if json.Unmarshal(params, &e) != nil {
			return
		}
		c.appendConsole(targetID, "["+e.Entry.Level+"] "+e.Entry.Text)
	}))

	// Install the per-target subscriptions (kept until the tab closes).
	c.mu.Lock()
	c.consoleSubs[targetID] = unsubs
	c.mu.Unlock()

	// Enable after subscribing so Log's replay of past entries is captured.
	_ = page.RawCall(ctx, "Runtime.enable", nil, nil)
	_ = page.RawCall(ctx, "Log.enable", nil, nil)
}

// claimConsole reserves targetID for console capture, returning true only for the
// first caller. A caller that gets false must not subscribe (capture is active).
func (c *Core) claimConsole(targetID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.consoleSubs == nil {
		c.consoleSubs = map[string][]func(){}
	}
	if _, ok := c.consoleSubs[targetID]; ok {
		return false
	}
	c.consoleSubs[targetID] = []func(){} // reserved; replaced with real subs below
	return true
}

func (c *Core) appendConsole(targetID, line string) {
	c.consolesMu.Lock()
	defer c.consolesMu.Unlock()
	if c.consoles == nil {
		c.consoles = map[string][]string{}
	}
	buf := append(c.consoles[targetID], line)
	if n := len(buf); n > maxConsoleLines {
		buf = buf[n-maxConsoleLines:]
	}
	c.consoles[targetID] = buf
}

var consoleTool = &mcptransport.Tool{
	Name:        "console",
	Description: "Read buffered console messages and uncaught exceptions for a tab (default the current tab; pass targetId to read another without switching). Every tab is captured continuously, so switching tabs never loses console output. Narrow with level (log|info|warning|error|debug) or grep (regex); clear empties the buffer.",
	InputSchema: mcptransport.Schema{
		Type: mcptransport.TypeObject,
		Properties: map[string]mcptransport.Schema{
			"action":   {Type: mcptransport.TypeString, Description: "list (default) | clear", Enum: []string{"list", "clear"}},
			"targetId": {Type: mcptransport.TypeString, Description: "read (and start capturing) this specific tab by id; defaults to the current tab — inspect several tabs without switching current"},
			"level":    {Type: mcptransport.TypeString, Description: "only these levels: debug|log|info|warn|error"},
			"grep":     {Type: mcptransport.TypeString, Description: "only lines matching this regex"},
			"limit":    {Type: mcptransport.TypeNumber, Description: "most recent N matching lines"},
		},
	},
}

func (c *Core) console(ctx context.Context, call *mcptransport.Call) (*mcptransport.ToolResult, error) {
	var a struct {
		Action   string `json:"action"`
		TargetID string `json:"targetId"`
		Level    string `json:"level"`
		Grep     string `json:"grep"`
		Limit    int    `json:"limit"`
	}
	if err := json.Unmarshal(call.Arguments, &a); err != nil {
		return mcptransport.ResultError("bad arguments: " + err.Error()), nil
	}
	// Read a specific tab by id (default: current). Naming a tab also starts its
	// capture (idempotent, without making it current) so multi-tab reads need no
	// current-tab switching — and no select/read race.
	target := a.TargetID
	if target == "" {
		target = c.currentTargetID()
	} else {
		c.captureTarget(ctx, target)
	}

	if a.Action == "clear" {
		c.consolesMu.Lock()
		delete(c.consoles, target)
		c.consolesMu.Unlock()
		return mcptransport.ResultText("console cleared"), nil
	}

	c.consolesMu.Lock()
	lines := append([]string{}, c.consoles[target]...)
	c.consolesMu.Unlock()

	var re *regexp.Regexp
	if a.Grep != "" {
		var err error
		if re, err = regexp.Compile(a.Grep); err != nil {
			return mcptransport.ResultError("bad grep: " + err.Error()), nil
		}
	}
	var out []string
	for _, ln := range lines {
		if a.Level != "" && !strings.Contains(ln, "["+a.Level+"]") {
			continue
		}
		if re != nil && !re.MatchString(ln) {
			continue
		}
		out = append(out, ln)
	}
	if a.Limit > 0 && len(out) > a.Limit {
		out = out[len(out)-a.Limit:]
	}
	if len(out) == 0 {
		return mcptransport.ResultText("(no console messages)"), nil
	}
	return mcptransport.ResultText(c.cap(strings.Join(out, "\n"), 0)), nil
}
