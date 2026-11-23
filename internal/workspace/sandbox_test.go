package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestResolve(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "src"), filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}

	ok := []string{"", ".", "main.go", "src/new/file.go", "alias/x.go", filepath.Join(root, "src")}
	for _, p := range ok {
		if _, err := Resolve(root, p); err != nil {
			t.Errorf("Resolve(%q) unexpected error: %v", p, err)
		}
	}

	bad := []string{"..", "../x", "src/../../x", "/etc/passwd", "escape/secret", "escape/new/file"}
	for _, p := range bad {
		if _, err := Resolve(root, p); !errors.Is(err, ErrOutsideWorkspace) {
			t.Errorf("Resolve(%q) = %v, want ErrOutsideWorkspace", p, err)
		}
	}
}
