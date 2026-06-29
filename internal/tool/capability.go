package tool

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/MMinasyan/lightcode/internal/pathutil"
)

type CapabilityOptions struct {
	WriteDir string
}

func (o CapabilityOptions) hasWriteDir() bool {
	return strings.TrimSpace(o.WriteDir) != ""
}

func isWriteTool(name string) bool {
	return name == "write_file" || name == "edit_file" || name == "apply_patch"
}

func checkWriteDirTarget(root, toolName, canonicalPath string, opts CapabilityOptions) error {
	if !opts.hasWriteDir() || !isWriteTool(toolName) || canonicalPath == "" {
		return nil
	}
	boundary, err := pathutil.ResolveFilePathFrom(root, opts.WriteDir)
	if err != nil {
		return fmt.Errorf("%s: resolve write_dir %q: %w", toolName, opts.WriteDir, err)
	}
	if pathInsideDir(canonicalPath, boundary.CanonicalPath) {
		return nil
	}
	return fmt.Errorf("%s: path %s is outside write_dir %s", toolName, canonicalPath, boundary.CanonicalPath)
}

func pathInsideDir(path, dir string) bool {
	path = filepath.Clean(path)
	dir = filepath.Clean(dir)
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel))
}
