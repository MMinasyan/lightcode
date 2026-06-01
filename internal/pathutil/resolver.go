package pathutil

import (
	"os"
	"path/filepath"
	"strings"
)

type ResolvedPath struct {
	OriginalPath  string
	AbsPath       string
	CanonicalPath string
	LeafExists    bool
}

func ResolveFilePath(path string) (ResolvedPath, error) {
	return ResolveFilePathFrom("", path)
}

func ResolveFilePathFrom(root, path string) (ResolvedPath, error) {
	if root != "" && !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return ResolvedPath{}, err
	}
	canonicalPath, leafExists, err := ResolveAbsPath(absPath)
	if err != nil {
		return ResolvedPath{}, err
	}
	return ResolvedPath{
		OriginalPath:  path,
		AbsPath:       absPath,
		CanonicalPath: canonicalPath,
		LeafExists:    leafExists,
	}, nil
}

func ResolveAbsPath(absPath string) (string, bool, error) {
	absPath = filepath.Clean(absPath)
	if _, err := os.Lstat(absPath); err == nil {
		canonicalPath, err := filepath.EvalSymlinks(absPath)
		if err != nil {
			return "", true, err
		}
		return canonicalPath, true, nil
	} else if !os.IsNotExist(err) {
		return "", false, err
	}

	parent := filepath.Dir(absPath)
	tail := []string{filepath.Base(absPath)}
	for {
		if _, err := os.Lstat(parent); err == nil {
			canonicalParent, err := filepath.EvalSymlinks(parent)
			if err != nil {
				return "", false, err
			}
			parts := append([]string{canonicalParent}, tail...)
			return filepath.Join(parts...), false, nil
		} else if !os.IsNotExist(err) {
			return "", false, err
		}

		nextParent := filepath.Dir(parent)
		if nextParent == parent {
			return absPath, false, nil
		}
		newTail := make([]string, len(tail)+1)
		newTail[0] = filepath.Base(parent)
		copy(newTail[1:], tail)
		tail = newTail
		parent = nextParent
	}
}

func ResolvePathPattern(pattern string) string {
	pattern = filepath.Clean(pattern)
	globIdx := strings.IndexAny(pattern, "*?[")
	if globIdx < 0 {
		if resolved, _, err := ResolveAbsPath(pattern); err == nil {
			return resolved
		}
		return pattern
	}

	sepIdx := strings.LastIndex(pattern[:globIdx], string(filepath.Separator))
	if sepIdx < 0 {
		return pattern
	}
	prefix := pattern[:sepIdx]
	tail := pattern[sepIdx:]
	if prefix == "" {
		prefix = string(filepath.Separator)
	}
	if resolved, _, err := ResolveAbsPath(prefix); err == nil {
		return filepath.Clean(resolved + tail)
	}
	return pattern
}
