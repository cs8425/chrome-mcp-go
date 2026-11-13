package tools

// The files pack: upload (DOM.setFileInputFiles) and download (browser download
// capture into the sandbox file box). All paths are VIRTUAL — the filebox jail
// rooted at "/" — so the model never sees host paths. box.Abs() is used only
// internally to hand the browser a real path.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"chrome-mcp/internal/filebox"
	"mcptransport"
)

const maxDownloads = 100

// downloadRec is one captured browser download.
type downloadRec struct {
	Guid  string `json:"guid"`
	URL   string `json:"url"`
	Name  string `json:"name"`
	VPath string `json:"path"` // virtual path in the file box
	State string `json:"state"`
	Total int64  `json:"totalBytes,omitempty"`
	Got   int64  `json:"receivedBytes"`
}

// ---------- upload ----------

var uploadTool = &mcptransport.Tool{
	Name:        "upload",
	Description: "Set file(s) on a file input element. Files are virtual paths inside the sandbox file box (root \"/\"); they must already exist there (write them first via saveAs or the fs tools).",
	InputSchema: mcptransport.Schema{
		Type: mcptransport.TypeObject,
		Properties: map[string]mcptransport.Schema{
			"selector": {Type: mcptransport.TypeString, Description: "CSS selector of the file input (or an element that opens the file chooser)"},
			"files":    {Type: mcptransport.TypeArray, Description: "virtual paths of files to upload, e.g. [\"/photo.png\"]", Items: &mcptransport.Schema{Type: mcptransport.TypeString}},
		},
		Required: []string{"selector", "files"},
	},
}

func (c *Core) upload(ctx context.Context, call *mcptransport.Call) (*mcptransport.ToolResult, error) {
	var a struct {
		Selector string   `json:"selector"`
		Files    []string `json:"files"`
	}
	if err := json.Unmarshal(call.Arguments, &a); err != nil {
		return mcptransport.ResultError("bad arguments: " + err.Error()), nil
	}
	if a.Selector == "" || len(a.Files) == 0 {
		return mcptransport.ResultError("selector and files are required"), nil
	}
	box := c.fileBox()
	if box == nil {
		return mcptransport.ResultError(filebox.ErrNoRoot.Error()), nil
	}
	// Verify each virtual file exists in the box; resolve host paths for CDP.
	hostFiles := make([]string, 0, len(a.Files))
	names := make([]string, 0, len(a.Files))
	for _, f := range a.Files {
		if _, err := box.Stat(ctx, f); err != nil {
			return mcptransport.ResultError("upload: " + err.Error()), nil
		}
		host, err := box.BrowserPath(f)
		if err != nil {
			return mcptransport.ResultError("upload: " + err.Error()), nil
		}
		hostFiles = append(hostFiles, host)
		vp, _ := filebox.Canonical(f)
		names = append(names, vp)
	}
	page, id, err := c.resolveNode(ctx, a.Selector)
	if err != nil {
		return mcptransport.ResultError("upload: resolve input: " + err.Error()), nil
	}
	if err := page.RawCall(ctx, "DOM.setFileInputFiles", map[string]any{"nodeId": id, "files": hostFiles}, nil); err != nil {
		return mcptransport.ResultError("upload: " + err.Error()), nil
	}
	return mcptransport.ResultText(fmt.Sprintf("uploaded %d file(s) to %s: %s", len(hostFiles), a.Selector, strings.Join(names, ", "))), nil
}

// ---------- download ----------

var downloadTool = &mcptransport.Tool{
	Name:        "download",
	Description: "Capture browser downloads into the sandbox file box. action start enables capture (behavior=allow + events) before you trigger a download; wait blocks until a matching download finishes and returns its virtual path; list shows captured downloads; clear empties the buffer.",
	InputSchema: mcptransport.Schema{
		Type: mcptransport.TypeObject,
		Properties: map[string]mcptransport.Schema{
			"action":  {Type: mcptransport.TypeString, Description: "start | wait | list | clear", Enum: []string{"start", "wait", "list", "clear"}},
			"guid":    {Type: mcptransport.TypeString, Description: "wait: wait for this specific download guid"},
			"match":   {Type: mcptransport.TypeString, Description: "wait: wait for a download whose filename/url contains this substring"},
			"timeout": {Type: mcptransport.TypeNumber, Description: "wait: max seconds to wait (default 30)"},
		},
	},
}

func (c *Core) download(ctx context.Context, call *mcptransport.Call) (*mcptransport.ToolResult, error) {
	var a struct {
		Action  string `json:"action"`
		Guid    string `json:"guid"`
		Match   string `json:"match"`
		Timeout int    `json:"timeout"`
	}
	if err := json.Unmarshal(call.Arguments, &a); err != nil {
		return mcptransport.ResultError("bad arguments: " + err.Error()), nil
	}
	switch a.Action {
	case "start":
		if err := c.enableDownloads(ctx); err != nil {
			return mcptransport.ResultError("download start: " + err.Error()), nil
		}
		return mcptransport.ResultText("downloads capture started (behavior=allow; files land in the file box)"), nil
	case "clear":
		c.downloadsMu.Lock()
		c.downloads = nil
		c.downloadsMu.Unlock()
		return mcptransport.ResultText("downloads cleared"), nil
	case "wait":
		return c.downloadWait(ctx, a.Guid, a.Match, a.Timeout)
	default: // list
		return c.downloadList(), nil
	}
}

// enableDownloads allows downloads into the file box, enables events, and
// (re)subscribes to the browser download events.
func (c *Core) enableDownloads(ctx context.Context) error {
	box := c.fileBox()
	if box == nil {
		return filebox.ErrNoRoot
	}
	vroot := box.BrowserVRoot()
	dir, err := box.BrowserPath(vroot)
	if err != nil {
		return err
	}

	var unsub []func()
	unsub = append(unsub, c.browser.Load().Subscribe("Browser.downloadWillBegin", func(params json.RawMessage) {
		var e struct {
			Guid              string `json:"guid"`
			URL               string `json:"url"`
			SuggestedFilename string `json:"suggestedFilename"`
		}
		if json.Unmarshal(params, &e) != nil {
			return
		}
		name := e.SuggestedFilename
		vpath := vroot + "/" + name
		if cv, cerr := filebox.Canonical(vpath); cerr == nil {
			vpath = cv
		}
		rec := &downloadRec{Guid: e.Guid, URL: e.URL, Name: name, State: "inProgress", VPath: vpath}
		c.downloadsMu.Lock()
		c.downloads = append(c.downloads, rec)
		if n := len(c.downloads); n > maxDownloads {
			c.downloads = c.downloads[n-maxDownloads:]
		}
		c.downloadsMu.Unlock()
	}))
	unsub = append(unsub, c.browser.Load().Subscribe("Browser.downloadProgress", func(params json.RawMessage) {
		var e struct {
			Guid          string `json:"guid"`
			TotalBytes    int64  `json:"totalBytes"`
			ReceivedBytes int64  `json:"receivedBytes"`
			State         string `json:"state"`
		}
		if json.Unmarshal(params, &e) != nil {
			return
		}
		c.downloadsMu.Lock()
		for _, d := range c.downloads {
			if d.Guid == e.Guid {
				d.State = e.State
				d.Total = e.TotalBytes
				d.Got = e.ReceivedBytes
				break
			}
		}
		c.downloadsMu.Unlock()
	}))

	c.downloadsMu.Lock()
	old := c.downloadUnsub
	c.downloadUnsub = unsub
	c.downloadsMu.Unlock()
	for _, u := range old {
		u()
	}

	return c.browser.Load().RawCall(ctx, "Browser.setDownloadBehavior", map[string]any{
		"behavior":      "allow",
		"downloadPath":  dir,
		"eventsEnabled": true,
	}, nil)
}

func (c *Core) downloadList() *mcptransport.ToolResult {
	c.downloadsMu.Lock()
	recs := append([]*downloadRec{}, c.downloads...)
	c.downloadsMu.Unlock()
	if len(recs) == 0 {
		return mcptransport.ResultText("(no downloads captured; call action=start, then trigger a download)")
	}
	lines := make([]string, 0, len(recs))
	for _, d := range recs {
		lines = append(lines, fmt.Sprintf("%s  %s  [%s]  %d/%d B", d.Guid, d.VPath, d.State, d.Got, d.Total))
	}
	return mcptransport.ResultText(c.cap(strings.Join(lines, "\n"), 0))
}

func (c *Core) downloadWait(ctx context.Context, guid, match string, timeout int) (*mcptransport.ToolResult, error) {
	if timeout <= 0 {
		timeout = 30
	}
	wctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	for {
		c.downloadsMu.Lock()
		var found *downloadRec
		for i := len(c.downloads) - 1; i >= 0; i-- {
			d := c.downloads[i]
			if guid != "" && d.Guid != guid {
				continue
			}
			if match != "" && !strings.Contains(d.Name, match) && !strings.Contains(d.URL, match) {
				continue
			}
			if d.State == "completed" || d.State == "canceled" {
				cp := *d
				found = &cp
				break
			}
		}
		c.downloadsMu.Unlock()
		if found != nil {
			if found.State == "completed" {
				return mcptransport.ResultText(fmt.Sprintf("downloaded %d bytes -> %s (guid %s)", found.Got, found.VPath, found.Guid)), nil
			}
			return mcptransport.ResultError(fmt.Sprintf("download %s (guid %s) was canceled", found.VPath, found.Guid)), nil
		}
		select {
		case <-wctx.Done():
			return mcptransport.ResultError(fmt.Sprintf("download wait timed out after %ds", timeout)), nil
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// RegisterFiles wires the files pack (upload, download) into a registry.
func RegisterFiles(reg *mcptransport.Registry, core *Core) {
	must(reg.Register(uploadTool, core.live(core.upload)))
	must(reg.Register(downloadTool, core.live(core.download)))
}
