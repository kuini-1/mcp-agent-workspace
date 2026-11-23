package workspace

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var ErrOutsideWorkspace = errors.New("path is outside the workspace")

// Resolve turns a path from the model into an absolute path inside root.
// It rejects "..", absolute paths that leave root, and symlinks that point
// outside root, including symlinks in parent directories of a file that
// doesn't exist yet.
func Resolve(root, p string) (string, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	realRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		return "", fmt.Errorf("workspace root: %w", err)
	}

	if p == "" {
		p = "."
	}
	var target string
	if filepath.IsAbs(p) {
		target = filepath.Clean(p)
	} else {
		target = filepath.Join(realRoot, p)
	}
	if !within(realRoot, target) && !within(absRoot, target) {
		return "", fmt.Errorf("%w: %s", ErrOutsideWorkspace, p)
	}

	// Follow symlinks on the longest existing prefix of the path.
	existing := target
	var rest []string
	for {
		if _, err := os.Lstat(existing); err == nil {
			break
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			break
		}
		rest = append([]string{filepath.Base(existing)}, rest...)
		existing = parent
	}
	real, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return "", err
	}
	full := filepath.Join(append([]string{real}, rest...)...)
	if !within(realRoot, full) {
		return "", fmt.Errorf("%w: %s resolves to %s", ErrOutsideWorkspace, p, full)
	}
	return full, nil
}

func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
