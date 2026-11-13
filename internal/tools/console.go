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

// startConsoleCapture subscribes the given tab's session to console/exception/
// log events, buffering formatted lines under targetID. It replaces any capture
// from a previously selected tab.
func (c *Core) startConsoleCapture(ctx context.Context, page *cdp.Client, targetID string) {
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

	// Drop the previous tab's subscriptions, then install the new ones.
	c.mu.Lock()
	old := c.consoleUnsub
	c.consoleUnsub = unsubs
	c.mu.Unlock()
	for _, u := range old {
		u()
	}

	// Enable after subscribing so Log's replay of past entries is captured.
	_ = page.RawCall(ctx, "Runtime.enable", nil, nil)
	_ = page.RawCall(ctx, "Log.enable", nil, nil)
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
	Description: "Read buffered console messages and uncaught exceptions for the current tab. Narrow with level (log|info|warning|error|debug) or grep (regex); clear empties the buffer.",
	InputSchema: mcptransport.Schema{
		Type: mcptransport.TypeObject,
		Properties: map[string]mcptransport.Schema{
			"action": {Type: mcptransport.TypeString, Description: "list (default) | clear", Enum: []string{"list", "clear"}},
			"level":  {Type: mcptransport.TypeString, Description: "only these levels: debug|log|info|warn|error"},
			"grep":   {Type: mcptransport.TypeString, Description: "only lines matching this regex"},
			"limit":  {Type: mcptransport.TypeNumber, Description: "most recent N matching lines"},
		},
	},
}

func (c *Core) console(ctx context.Context, call *mcptransport.Call) (*mcptransport.ToolResult, error) {
	var a struct {
		Action string `json:"action"`
		Level  string `json:"level"`
		Grep   string `json:"grep"`
		Limit  int    `json:"limit"`
	}
	if err := json.Unmarshal(call.Arguments, &a); err != nil {
		return mcptransport.ResultError("bad arguments: " + err.Error()), nil
	}
	target := c.currentTargetID()

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
