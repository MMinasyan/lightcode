// Package project owns ~/.lightcode/projects/<project-id>/ — a project
// registry keyed by filesystem path. Each project dir holds meta.json
// plus a sessions/ subdir which is the root passed to the snapshot
// Store. Projects are created lazily on the first session in a cwd and
// persist forever.
package project

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/MMinasyan/lightcode/internal/atomicfs"
)

// Project is persisted to projects/<id>/meta.json.
type Project struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Path         string `json:"path"`
	CreatedAt    string `json:"created_at"`
	LastActivity int64  `json:"last_activity"`
}

// Resolver owns the projects root and is shared across app + store. It
// knows the current process's cwd (projectRoot) and can lazily resolve
// or create a project record for it.
type Resolver struct {
	root        string
	projectRoot string
}

// NewResolver returns a Resolver rooted at ~/.lightcode/projects for
// the given cwd. The projects root is created on demand.
func NewResolver(home, projectRoot string) (*Resolver, error) {
	root := filepath.Join(home, ".lightcode", "projects")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("project: create %s: %w", root, err)
	}
	for _, dir := range []string{root, filepath.Dir(root), filepath.Dir(filepath.Dir(root))} {
		if err := atomicfs.SyncDir(dir); err != nil {
			return nil, fmt.Errorf("project: sync %s: %w", dir, err)
		}
	}
	abs, err := filepath.Abs(projectRoot)
	if err != nil {
		return nil, fmt.Errorf("project: resolve cwd: %w", err)
	}
	return &Resolver{root: root, projectRoot: abs}, nil
}

// Root returns the projects root dir.
func (r *Resolver) Root() string { return r.root }

// ProjectRoot returns the current cwd this resolver was built for.
func (r *Resolver) ProjectRoot() string { return r.projectRoot }

// Current returns the project record for the resolver's cwd, or (nil, nil)
// if none has been created yet.
func (r *Resolver) Current() (*Project, error) {
	return FindByPath(r.root, r.projectRoot)
}

// Ensure returns the project record for the resolver's cwd, creating
// the directory + meta.json if it does not yet exist.
func (r *Resolver) Ensure() (*Project, error) {
	return EnsureForPath(r.root, r.projectRoot)
}

// SessionsRoot returns the sessions dir for the given project id
// (caller already holds a project record).
func (r *Resolver) SessionsRoot(projectID string) string {
	return filepath.Join(r.root, projectID, "sessions")
}

// List scans root and returns every project found. Order is unspecified;
// callers that want chronological order sort by LastActivity themselves.
func List(root string) ([]Project, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("project: list: %w", err)
	}
	var out []Project
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// Skip reserved sidecar dirs (.locks, .staging, .deleting) that are
		// not project records.
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		p, err := readProject(root, e.Name())
		if err != nil {
			continue
		}
		out = append(out, *p)
	}
	return out, nil
}

// ListSortedByActivity returns projects sorted by LastActivity desc.
func ListSortedByActivity(root string) ([]Project, error) {
	out, err := List(root)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].LastActivity > out[j].LastActivity
	})
	return out, nil
}

// FindByPath returns the project record for absPath, or (nil, nil) if none
// has been created. The project id is a deterministic function of the
// normalized path, so this reads exactly that project's meta.json.
func FindByPath(root, absPath string) (*Project, error) {
	clean, err := normalizePath(absPath)
	if err != nil {
		return nil, err
	}
	p, err := readProject(root, projectID(clean))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	// The normalized path is the expected value here, so a record that
	// belongs to another path is rejected with an error — never (nil, nil),
	// which callers read as "no such project" and would answer by minting a
	// second project over the corrupt record.
	if p.Path != clean {
		return nil, fmt.Errorf("project: %s: stored path %q does not match %q", p.ID, p.Path, clean)
	}
	return p, nil
}

// EnsureForPath returns the existing project record for absPath or creates
// one. The project id is the deterministic id of the normalized path, so a
// path maps to exactly one project. Creation is serialized under the
// per-path identity lock, so two concurrent creators converge on one record
// with one CreatedAt rather than racing to overwrite each other.
func EnsureForPath(root, absPath string) (*Project, error) {
	clean, err := normalizePath(absPath)
	if err != nil {
		return nil, err
	}
	id := projectID(clean)
	dir := filepath.Join(root, id)
	metaPath := filepath.Join(dir, "meta.json")

	var result Project
	err = atomicfs.WithLock(identityLockPath(root, clean), func() error {
		existing, err := readProject(root, id)
		if err == nil {
			// The normalized path is the expected value here, so a record
			// that belongs to another path is rejected with an error rather
			// than minting a second project over the corrupt record.
			if existing.Path != clean {
				return fmt.Errorf("project: %s: stored path %q does not match %q", id, existing.Path, clean)
			}
			result = *existing
			if err := atomicfs.SyncDir(dir); err != nil {
				return fmt.Errorf("project: sync %s: %w", dir, err)
			}
			if err := atomicfs.SyncDir(root); err != nil {
				return fmt.Errorf("project: sync %s: %w", root, err)
			}
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := os.MkdirAll(filepath.Join(dir, "sessions"), 0o700); err != nil {
			return fmt.Errorf("project: create %s: %w", dir, err)
		}
		now := time.Now()
		p := Project{
			ID:           id,
			Name:         filepath.Base(clean),
			Path:         clean,
			CreatedAt:    now.UTC().Format(time.RFC3339),
			LastActivity: now.Unix(),
		}
		if err := writeJSON(metaPath, p); err != nil {
			return err
		}
		if err := atomicfs.SyncDir(dir); err != nil {
			return fmt.Errorf("project: sync %s: %w", dir, err)
		}
		if err := atomicfs.SyncDir(root); err != nil {
			return fmt.Errorf("project: sync %s: %w", root, err)
		}
		result = p
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// TouchActivity advances LastActivity on the project's meta.json to now.
// The whole read-modify-write runs under the project meta lock and never
// moves activity backward, so concurrent touches serialize to a monotonic
// value instead of losing an update.
func TouchActivity(root, projectID string) error {
	if projectID == "" {
		return nil
	}
	metaPath := metaPathFor(root, projectID)
	return atomicfs.WithLock(metaLockPath(root, projectID), func() error {
		p, err := readProject(root, projectID)
		if err != nil {
			return err
		}
		now := time.Now().Unix()
		if now <= p.LastActivity {
			return nil
		}
		p.LastActivity = now
		return writeJSON(metaPath, p)
	})
}

// normalizePath is the canonical project-path identity: the cleaned
// absolute path, with no symlink or case rewriting.
func normalizePath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("project: resolve path: %w", err)
	}
	return filepath.Clean(abs), nil
}

func pathHash(cleanAbs string) string {
	sum := sha256.Sum256([]byte(cleanAbs))
	return hex.EncodeToString(sum[:])
}

// projectID is p- plus the full lowercase SHA-256 hex of the normalized
// path bytes. It is fully deterministic; there is no random or legacy id.
func projectID(cleanAbs string) string {
	return "p-" + pathHash(cleanAbs)
}

// readProject reads a project's meta.json and assigns the id the caller
// resolved the directory under. The persisted record's own id is
// unvalidated and can name a different project, so the directory's id wins:
// every reader that already holds the id reports the record under it,
// keeping a corrupt record from surfacing under a foreign id.
func readProject(root, id string) (*Project, error) {
	var p Project
	if err := readJSON(metaPathFor(root, id), &p); err != nil {
		return nil, err
	}
	p.ID = id
	return &p, nil
}

func metaPathFor(root, id string) string {
	return filepath.Join(root, id, "meta.json")
}

func identityLockPath(root, cleanAbs string) string {
	return filepath.Join(root, ".locks", "identity", pathHash(cleanAbs)+".lock")
}

func metaLockPath(root, id string) string {
	return filepath.Join(root, id, ".locks", "meta.lock")
}
