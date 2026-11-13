package filebox

import (
	"context"
	"strings"
	"testing"
)

func mustOpenIn(t *testing.T, dir string) Box {
	t.Helper()
	box, err := Open(dir, "", "")
	if err != nil {
		t.Fatal(err)
	}
	return box
}

// TestJail proves the box is confined to its root and that no host path leaks.
func TestJail(t *testing.T) {
	parent := t.TempDir()
	jailed := parent + "/workspace" // the real host dir must never surface
	box := mustOpenIn(t, jailed)
	ctx := context.Background()

	if err := box.Write(ctx, "/a.txt", []byte("hi")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if b, err := box.Read(ctx, "/a.txt"); err != nil || string(b) != "hi" {
		t.Fatalf("read: %q err=%v", b, err)
	}
	if _, err := box.Stat(ctx, "a.txt"); err != nil { // relative input accepted too
		t.Errorf("stat: %v", err)
	}

	// upward traversal is rejected
	for _, bad := range []string{"../evil.txt", "/../evil.txt", "/a/../../x"} {
		if err := box.Write(ctx, bad, []byte("x")); err == nil {
			t.Errorf("expected escape rejection for %q", bad)
		}
	}

	// errors never leak the host directory (or its parent)
	if _, err := box.Read(ctx, "/nope.txt"); err != nil {
		if strings.Contains(err.Error(), jailed) || strings.Contains(err.Error(), parent) {
			t.Errorf("error leaked host path: %v", err)
		}
	}

	if err := box.Remove(ctx, "/a.txt"); err != nil {
		t.Errorf("remove: %v", err)
	}
}

// TestCanonical checks the virtual-path normalizer: everything is "/"-rooted,
// Windows backslashes are folded to "/", and upward escapes are rejected.
func TestCanonical(t *testing.T) {
	cases := map[string]string{
		"":               "/",
		"/":              "/",
		"a":              "/a",
		"/a/":            "/a",
		"/a//b":          "/a/b",
		"/a/../b":        "/b",
		"a/b/../c":       "/a/c",
		`shots\shot.png`: "/shots/shot.png",
		`\a\b`:           "/a/b",
		`C:\x\y`:         "/C:/x/y", // jailed as a plain name; never a host path
	}
	for in, want := range cases {
		got, err := Canonical(in)
		if err != nil {
			t.Errorf("Canonical(%q) err=%v", in, err)
			continue
		}
		if strings.Contains(got, `\`) {
			t.Errorf("Canonical(%q)=%q contains backslash", in, got)
		}
		if got != want {
			t.Errorf("Canonical(%q)=%q want %q", in, got, want)
		}
	}
	for _, bad := range []string{"..", "../x", "/..", "/a/../../x"} {
		if _, err := Canonical(bad); err == nil {
			t.Errorf("Canonical(%q) should reject", bad)
		}
	}
}

// TestBrowserPath checks the host→browser path mapping for a containerized
// browser that only shares a subdirectory (virtual /downloads <-> container
// /tmp/Downloads), and that paths outside the shared subtree are refused.
func TestBrowserPath(t *testing.T) {
	box, err := Open(t.TempDir(), "/tmp/Downloads", "/downloads")
	if err != nil {
		t.Fatal(err)
	}
	if box.BrowserVRoot() != "/downloads" {
		t.Errorf("BrowserVRoot=%q", box.BrowserVRoot())
	}
	if got, _ := box.BrowserPath("/downloads/sub/a.txt"); got != "/tmp/Downloads/sub/a.txt" {
		t.Errorf("BrowserPath=%q want /tmp/Downloads/sub/a.txt", got)
	}
	if root, _ := box.BrowserPath("/downloads"); root != "/tmp/Downloads" {
		t.Errorf("BrowserPath(vroot)=%q want /tmp/Downloads", root)
	}
	if _, err := box.BrowserPath("/secret.txt"); err == nil {
		t.Error("path outside the shared subtree should be refused")
	}

	// whole-box shared: browser-vroot "/"
	box2, err := Open(t.TempDir(), "/shared", "/")
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := box2.BrowserPath("/x/y"); got != "/shared/x/y" {
		t.Errorf("BrowserPath=%q want /shared/x/y", got)
	}
}
