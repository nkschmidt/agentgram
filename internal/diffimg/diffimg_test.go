package diffimg

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestRenderProducesImages sets up a throwaway git repo with a committed file,
// an unstaged modification, and an untracked file, then checks that Render
// produces two PNGs (diff + untracked). Skipped when git or silicon is absent.
func TestRenderProducesImages(t *testing.T) {
	requireBins(t, "git", "silicon")

	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	git("init", "-q")
	write("main.go", "package main\n\nfunc main() {}\n")
	git("add", "-A")
	git("commit", "-qm", "init")

	write("main.go", "package main\n\nfunc main() { println(\"hi\") }\n") // tracked change
	write("notes.txt", "some untracked notes\n")                          // untracked

	imgs, cleanup, err := Render(context.Background(), dir)
	defer cleanup()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(imgs) != 2 {
		t.Fatalf("want 2 images (diff + untracked), got %d: %+v", len(imgs), imgs)
	}
	for _, img := range imgs {
		if info, err := os.Stat(img.Path); err != nil || info.Size() == 0 {
			t.Fatalf("image %q missing or empty: %v", img.Path, err)
		}
	}
}

// TestRenderCleanTree returns no images and a real (non-nil) cleanup for a repo
// with no uncommitted changes.
func TestRenderCleanTree(t *testing.T) {
	requireBins(t, "git", "silicon")

	dir := t.TempDir()
	cmd := exec.Command("git", "-C", dir, "init", "-q")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}

	imgs, cleanup, err := Render(context.Background(), dir)
	defer cleanup()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(imgs) != 0 {
		t.Fatalf("want 0 images for clean tree, got %d", len(imgs))
	}
}

func requireBins(t *testing.T, bins ...string) {
	t.Helper()
	for _, b := range bins {
		if _, err := exec.LookPath(b); err != nil {
			t.Skipf("%s not on PATH", b)
		}
	}
}
