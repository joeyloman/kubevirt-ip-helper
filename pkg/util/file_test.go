package util

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFileExists(t *testing.T) {
	dir := t.TempDir()

	file := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if !FileExists(file) {
		t.Error("FileExists = false for an existing file")
	}

	sub := filepath.Join(dir, "subdir")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if FileExists(sub) {
		t.Error("FileExists = true for a directory")
	}

	if FileExists(filepath.Join(dir, "missing.txt")) {
		t.Error("FileExists = true for a missing file")
	}

	if FileExists(filepath.Join(dir, "also-missing", "nested.txt")) {
		t.Error("FileExists = true for a missing nested path")
	}
}

func TestFileExistsSymlink(t *testing.T) {
	dir := t.TempDir()

	target := filepath.Join(dir, "target.txt")
	if err := os.WriteFile(target, []byte("data"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	if !FileExists(link) {
		t.Error("FileExists = false for a symlink to an existing file")
	}

	dangling := filepath.Join(dir, "dangling.txt")
	if err := os.Symlink(filepath.Join(dir, "gone.txt"), dangling); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	if FileExists(dangling) {
		t.Error("FileExists = true for a symlink to a missing target")
	}
}

// a path stat reports an error for (a symlink loop, an unreadable
// directory) must report false, not panic on the nil file info: an
// unstatable kubeconfig falls back to the caller's in-cluster config path.
func TestFileExistsSymlinkLoop(t *testing.T) {
	dir := t.TempDir()

	a := filepath.Join(dir, "a")
	b := filepath.Join(dir, "b")
	if err := os.Symlink(b, a); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	if err := os.Symlink(a, b); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	if FileExists(a) {
		t.Error("FileExists = true for a symlink loop")
	}
	if FileExists(b) {
		t.Error("FileExists = true for a symlink loop")
	}
}
