// Package diffimg renders a git repository's uncommitted changes into PNG
// images via the external `silicon` tool. It shells out to `git` (to collect
// the tracked diff and the untracked files) and `silicon` (to rasterize them
// with syntax highlighting), writing the results into a temp directory the
// caller is expected to clean up.
//
// It never mutates git state — only read-only `git diff` / `git status` /
// `git ls-files` are invoked.
package diffimg

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Image is one rendered PNG together with the caption to show with it.
type Image struct {
	Path    string
	Caption string
}

// binaryExts are file extensions skipped when rendering untracked files —
// rasterizing their bytes as text is pointless.
var binaryExts = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".bmp": true,
	".ico": true, ".woff": true, ".woff2": true, ".ttf": true, ".eot": true,
	".pdf": true, ".zip": true, ".tar": true, ".gz": true,
}

// Render rasterizes the uncommitted changes in dir into PNG images: the tracked
// diff (git diff HEAD) and a listing of the untracked files. It returns the
// images to deliver — empty if the working tree is clean — plus a cleanup func
// (always non-nil) that removes the temp files; call it via defer. Requires
// `git` and `silicon` on PATH.
func Render(ctx context.Context, dir string) (imgs []Image, cleanup func(), err error) {
	noop := func() {}

	if _, err := exec.LookPath("silicon"); err != nil {
		return nil, noop, fmt.Errorf("silicon not found on PATH — install it (macOS: brew install silicon, Linux: cargo install silicon)")
	}
	if strings.TrimSpace(dir) == "" {
		return nil, noop, fmt.Errorf("no directory to diff")
	}
	if out, err := gitOut(ctx, dir, "rev-parse", "--is-inside-work-tree"); err != nil || strings.TrimSpace(out) != "true" {
		return nil, noop, fmt.Errorf("not a git repository: %s", dir)
	}

	status, err := gitOut(ctx, dir, "status", "--short")
	if err != nil {
		return nil, noop, fmt.Errorf("git status: %w", err)
	}
	if strings.TrimSpace(status) == "" {
		return nil, noop, nil // clean working tree — nothing to render
	}

	tmp, err := os.MkdirTemp("", "agentgram-diff-*")
	if err != nil {
		return nil, noop, err
	}
	realCleanup := func() { _ = os.RemoveAll(tmp) }
	// On any error past this point we clean up ourselves and hand back a no-op.
	fail := func(e error) ([]Image, func(), error) {
		realCleanup()
		return nil, noop, e
	}

	// --- tracked diff (git diff HEAD) ---
	diff, err := gitOut(ctx, dir, "diff", "HEAD", "--no-color")
	if err != nil {
		return fail(fmt.Errorf("git diff: %w", err))
	}
	if strings.TrimSpace(diff) != "" {
		src := filepath.Join(tmp, "git_diff.diff")
		if err := os.WriteFile(src, []byte(diff), 0o644); err != nil {
			return fail(err)
		}
		png := filepath.Join(tmp, "diff.png")
		if err := runSilicon(ctx, src, "Diff", png); err != nil {
			return fail(err)
		}
		imgs = append(imgs, Image{Path: png, Caption: "git diff HEAD — uncommitted changes"})
	}

	// --- untracked files ---
	if untracked, err := gitOut(ctx, dir, "ls-files", "--others", "--exclude-standard"); err == nil {
		var buf bytes.Buffer
		for _, f := range strings.Split(strings.TrimSpace(untracked), "\n") {
			if f == "" || binaryExts[strings.ToLower(filepath.Ext(f))] {
				continue
			}
			b, readErr := os.ReadFile(filepath.Join(dir, f))
			if readErr != nil {
				continue
			}
			buf.WriteString("=== " + f + " ===\n")
			buf.Write(b)
			buf.WriteString("\n\n")
		}
		if buf.Len() > 0 {
			src := filepath.Join(tmp, "untracked.txt")
			if err := os.WriteFile(src, buf.Bytes(), 0o644); err != nil {
				return fail(err)
			}
			png := filepath.Join(tmp, "untracked.png")
			if err := runSilicon(ctx, src, "Markdown", png); err != nil {
				return fail(err)
			}
			imgs = append(imgs, Image{Path: png, Caption: "Untracked files"})
		}
	}

	if len(imgs) == 0 {
		realCleanup()
		return nil, noop, nil
	}
	return imgs, realCleanup, nil
}

// gitOut runs a read-only git command in dir and returns its stdout.
func gitOut(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// runSilicon rasterizes src (a source file) into the PNG at out, syntax-
// highlighted for the given language with the GitHub theme.
func runSilicon(ctx context.Context, src, language, out string) error {
	cmd := exec.CommandContext(ctx, "silicon", src, "--language", language, "--theme", "GitHub", "-o", out)
	if combined, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("silicon: %w: %s", err, strings.TrimSpace(string(combined)))
	}
	return nil
}
