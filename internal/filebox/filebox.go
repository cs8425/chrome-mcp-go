// Package filebox provides a jailed file store: every path is confined to a
// root directory, so tools can read and write files (screenshot saveAs,
// upload/download, storage_state) with no risk of reaching the host filesystem.
//
// Virtual paths: tool callers and the model only ever see paths rooted at "/",
// which stands for the jail root. The real host directory behind the root is
// NEVER returned in a result or error — the agent cannot learn where the
// workspace actually lives (e.g. /home/user/agent-root or C:\agent-workspace);
// to it the workspace is simply "/".
//
// The local backend wraps os.Root (Go 1.24+), which rejects symlink escapes.
// The Box interface is backend-agnostic so the same tool code can later run
// against an SFTP (or other remote) backend without touching callers.
package filebox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// ErrNoRoot is returned when a tool needs the file store but the server was
// started without --root, so no directory has been jailed.
var ErrNoRoot = errors.New("no file root configured (start the server with --root <dir>)")

// ErrEscape is returned for paths that try to climb above the sandbox root.
var ErrEscape = errors.New("path escapes the sandbox root")

// Box is a file store confined to a root directory. All paths are VIRTUAL:
// rooted at "/" (the jail root); nothing outside it can be referenced.
type Box interface {
	Write(ctx context.Context, vpath string, data []byte) error
	Read(ctx context.Context, vpath string) ([]byte, error)
	// Create opens a file for writing (streaming, for large downloads).
	Create(ctx context.Context, vpath string) (io.WriteCloser, error)
	Stat(ctx context.Context, vpath string) (fs.FileInfo, error)
	Remove(ctx context.Context, vpath string) error
	// Abs resolves a virtual path to its host-absolute path, for internal use
	// by callers that must hand a real path to the browser (DOM.setFileInputFiles,
	// download path). The result must NEVER be returned to the model.
	Abs(vpath string) (string, error)
	// BrowserPath returns the path AS THE BROWSER PROCESS should see it. When
	// Chrome runs in a container/sandbox the same files sit at a different mount
	// (host /home/x/dl is the container's /downloads); this maps a virtual path
	// to that browser-side absolute path for CDP. With no browser root set it
	// falls back to the host Abs path.
	BrowserPath(vpath string) (string, error)
	// BrowserVRoot returns the virtual directory of the box the browser can
	// actually see (the shared mount): "/" if the whole box is shared, or e.g.
	// "/downloads". Downloads land here; uploads must be under it.
	BrowserVRoot() string
}

// Canonical normalizes a caller-supplied path to canonical virtual form
// ("/", "/a", "/a/b"). It accepts either "/"-rooted or relative input and
// backslashes, and rejects any ".." that would climb above the root.
func Canonical(p string) (string, error) {
	p = strings.ReplaceAll(p, "\\", "/")
	if p == "" {
		return "/", nil
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	var stack []string
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "", ".":
			// skip
		case "..":
			if len(stack) == 0 {
				return "", ErrEscape
			}
			stack = stack[:len(stack)-1]
		default:
			stack = append(stack, seg)
		}
	}
	return "/" + strings.Join(stack, "/"), nil
}

// rel maps a canonical virtual path to the relative form os.Root expects.
func rel(v string) string {
	r := strings.TrimPrefix(v, "/")
	if r == "" {
		return "."
	}
	return r
}

// local is the os.Root-backed Box. dir is the host path; it is never surfaced.
type local struct {
	root         *os.Root
	dir          string // host absolute root (filebox's own read/write)
	browserDir   string // path the browser sees for browserVRoot (e.g. container /tmp/Downloads); "" = same as dir
	browserVRoot string // virtual dir browserDir maps to (default "/")
}

// Open jails dir (creating it if missing) and returns a Box rooted there. The
// root is resolved to an ABSOLUTE path so Abs() always yields an absolute host
// path — required when handing a path to the browser (DOM.setFileInputFiles,
// downloadPath); a relative path there crashes the renderer with
// RESULT_CODE_KILLED_BAD_MESSAGE.
func Open(dir, browserDir, browserVRoot string) (Box, error) {
	if dir == "" {
		return nil, ErrNoRoot
	}
	if browserVRoot == "" {
		browserVRoot = "/"
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, err
	}
	r, err := os.OpenRoot(abs)
	if err != nil {
		return nil, err
	}
	return &local{root: r, dir: abs, browserDir: browserDir, browserVRoot: browserVRoot}, nil
}

// safe turns a backend error into one that reports only the (safe) virtual path
// plus an error category, so no host path can leak to the model.
func safe(v string, err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("%s: not found", v)
	case errors.Is(err, fs.ErrPermission):
		return fmt.Errorf("%s: permission denied", v)
	default:
		return fmt.Errorf("%s: file operation failed", v)
	}
}

func (b *local) Write(ctx context.Context, vpath string, data []byte) error {
	v, err := Canonical(vpath)
	if err != nil {
		return err
	}
	return safe(v, b.root.WriteFile(rel(v), data, 0o644))
}

func (b *local) Read(ctx context.Context, vpath string) ([]byte, error) {
	v, err := Canonical(vpath)
	if err != nil {
		return nil, err
	}
	data, err := b.root.ReadFile(rel(v))
	if err != nil {
		return nil, safe(v, err)
	}
	return data, nil
}

func (b *local) Create(ctx context.Context, vpath string) (io.WriteCloser, error) {
	v, err := Canonical(vpath)
	if err != nil {
		return nil, err
	}
	f, err := b.root.Create(rel(v))
	if err != nil {
		return nil, safe(v, err)
	}
	return f, nil
}

func (b *local) Stat(ctx context.Context, vpath string) (fs.FileInfo, error) {
	v, err := Canonical(vpath)
	if err != nil {
		return nil, err
	}
	fi, err := b.root.Stat(rel(v))
	if err != nil {
		return nil, safe(v, err)
	}
	return fi, nil
}

func (b *local) Remove(ctx context.Context, vpath string) error {
	v, err := Canonical(vpath)
	if err != nil {
		return err
	}
	return safe(v, b.root.Remove(rel(v)))
}

func (b *local) Abs(vpath string) (string, error) {
	v, err := Canonical(vpath)
	if err != nil {
		return "", err
	}
	return filepath.Join(b.dir, filepath.FromSlash(rel(v))), nil
}

func (b *local) BrowserPath(vpath string) (string, error) {
	v, err := Canonical(vpath)
	if err != nil {
		return "", err
	}
	if b.browserDir == "" {
		// Same filesystem: the browser sees the box at its host-absolute path.
		return filepath.Join(b.dir, filepath.FromSlash(rel(v))), nil
	}
	// Shared region: b.browserDir is the browser path of virtual b.browserVRoot.
	vr := b.browserVRoot
	prefix := vr
	if prefix != "/" {
		prefix += "/"
	}
	if v != vr && !strings.HasPrefix(v, prefix) {
		return "", fmt.Errorf("%s: not accessible to the browser (only %s is shared)", v, vr)
	}
	rest := strings.TrimPrefix(v, vr)
	return path.Join(b.browserDir, rest), nil
}

func (b *local) BrowserVRoot() string { return b.browserVRoot }
