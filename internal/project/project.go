// Package project owns ~/.lightcode/projects/<project-id>/ — a project
// registry keyed by filesystem path. Each project dir holds meta.json
// plus a sessions/ subdir which is the root passed to the snapshot
// Store. Projects are created lazily on the first session in a cwd and
// persist forever.
package project

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
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
		var p Project
		if err := readJSON(filepath.Join(root, e.Name(), "meta.json"), &p); err != nil {
			continue
		}
		out = append(out, p)
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

// FindByPath returns the first project whose Path == absPath, or (nil, nil).
func FindByPath(root, absPath string) (*Project, error) {
	projects, err := List(root)
	if err != nil {
		return nil, err
	}
	for i := range projects {
		if projects[i].Path == absPath {
			p := projects[i]
			return &p, nil
		}
	}
	return nil, nil
}

// EnsureForPath returns the existing project record for absPath or
// creates a new one. The new project's sessions/ subdir is also created.
func EnsureForPath(root, absPath string) (*Project, error) {
	if existing, err := FindByPath(root, absPath); err != nil {
		return nil, err
	} else if existing != nil {
		return existing, nil
	}
	id, err := newProjectID()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(filepath.Join(dir, "sessions"), 0o700); err != nil {
		return nil, fmt.Errorf("project: create %s: %w", dir, err)
	}
	now := time.Now()
	p := Project{
		ID:           id,
		Name:         filepath.Base(absPath),
		Path:         absPath,
		CreatedAt:    now.UTC().Format(time.RFC3339),
		LastActivity: now.Unix(),
	}
	if err := writeJSON(filepath.Join(dir, "meta.json"), p); err != nil {
		return nil, err
	}
	return &p, nil
}

// TouchActivity bumps LastActivity on the project's meta.json to now.
func TouchActivity(root, projectID string) error {
	if projectID == "" {
		return nil
	}
	metaPath := filepath.Join(root, projectID, "meta.json")
	var p Project
	if err := readJSON(metaPath, &p); err != nil {
		return err
	}
	p.LastActivity = time.Now().Unix()
	return writeJSON(metaPath, p)
}

func newProjectID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("project: random: %w", err)
	}
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(buf[0:4]),
		hex.EncodeToString(buf[4:6]),
		hex.EncodeToString(buf[6:8]),
		hex.EncodeToString(buf[8:10]),
		hex.EncodeToString(buf[10:16]),
	), nil
}
