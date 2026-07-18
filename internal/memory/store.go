package memory

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/MMinasyan/lightcode/internal/atomicfs"
	"github.com/MMinasyan/lightcode/internal/snapshot"
)

var errEmbedderUnavailable = errors.New("memory embedder unavailable")

type memoryEmbedder interface {
	Embed(text string) ([]float32, error)
	Close()
}

type MemoryResult struct {
	Title     string
	Content   string
	CreatedAt string
	Project   string
	FilePath  string
}

type HistoryResult struct {
	SectionContent string
	SessionID      string
	CreatedAt      string
	Project        string
	CompactionPath string
}

type Store struct {
	mu           sync.Mutex
	embedder     memoryEmbedder
	projectsRoot string
	home         string
}

func NewStore(embedder *Embedder, projectsRoot, home string) *Store {
	if embedder == nil {
		return NewStoreWithEmbedder(nil, projectsRoot, home)
	}
	return NewStoreWithEmbedder(embedder, projectsRoot, home)
}

func NewStoreWithEmbedder(embedder memoryEmbedder, projectsRoot, home string) *Store {
	return &Store{embedder: embedder, projectsRoot: projectsRoot, home: home}
}

func (s *Store) SaveMemory(memoriesDir, title, content string) (string, error) {
	if s == nil {
		return "", errEmbedderUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	embedder, err := s.requireEmbedder()
	if err != nil {
		return "", err
	}
	fp, err := WriteMemoryFile(memoriesDir, title, content)
	if err != nil {
		return "", err
	}
	vec, err := embedder.Embed(content)
	if err != nil {
		_ = os.Remove(fp)
		return "", fmt.Errorf("embed memory: %w", err)
	}
	vecPath := strings.TrimSuffix(fp, ".md") + ".vec"
	if err := WriteVec(vecPath, vec); err != nil {
		_ = os.Remove(fp)
		return "", err
	}
	return fp, nil
}

func (s *Store) SearchMemory(query, projectID string, allProjects bool, limit int) ([]MemoryResult, error) {
	if s == nil {
		return nil, errEmbedderUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	embedder, err := s.requireEmbedder()
	if err != nil {
		return nil, err
	}
	qvec, err := embedder.Embed(query)
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}

	var entries []VecEntry

	projects, err := os.ReadDir(s.projectsRoot)
	if err != nil {
		return nil, nil
	}
	for _, p := range projects {
		if !p.IsDir() {
			continue
		}
		if !allProjects && p.Name() != projectID {
			continue
		}
		memDir := filepath.Join(s.projectsRoot, p.Name(), "memories")
		files, err := os.ReadDir(memDir)
		if err != nil {
			continue
		}
		for _, f := range files {
			if !strings.HasSuffix(f.Name(), ".vec") {
				continue
			}
			vecPath := filepath.Join(memDir, f.Name())
			vec, err := ReadVec(vecPath)
			if err != nil {
				continue
			}
			entries = append(entries, VecEntry{Path: vecPath, Vec: vec})
		}
	}

	if len(entries) == 0 {
		return nil, nil
	}

	top := Search(qvec, entries, limit)

	var results []MemoryResult
	for _, e := range top {
		mdPath := strings.TrimSuffix(e.Path, ".vec") + ".md"
		title, content, createdAt, err := ReadMemoryFile(mdPath)
		if err != nil {
			continue
		}
		projectID := filepath.Base(filepath.Dir(filepath.Dir(mdPath)))
		projectName := readProjectName(filepath.Join(s.projectsRoot, projectID, "meta.json"))
		results = append(results, MemoryResult{
			Title:     title,
			Content:   content,
			CreatedAt: createdAt,
			Project:   projectName,
			FilePath:  mdPath,
		})
	}
	return results, nil
}

func (s *Store) summariesRoot() string {
	return filepath.Join(s.home, ".lightcode", "summaries")
}

func (s *Store) IndexSummary(sessionID, projectID, projectName, summary, createdAt, compactionPath string) error {
	if s == nil {
		return errEmbedderUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sections := SplitSummary(summary)
	if len(sections) == 0 {
		return nil
	}
	embedder, err := s.requireEmbedder()
	if err != nil {
		return err
	}

	type indexedSection struct {
		section Section
		vec     []float32
	}
	indexed := make([]indexedSection, 0, len(sections))
	for _, sec := range sections {
		vec, err := embedder.Embed(sec.Content)
		if err != nil {
			return fmt.Errorf("embed summary section: %w", err)
		}
		indexed = append(indexed, indexedSection{section: sec, vec: vec})
	}

	// Each indexing writes an immutable generation directory. index.json is
	// published atomically last and is the commit marker: it names the current
	// generation. A crash before index.json leaves an orphan generation that
	// readers ignore, and re-indexing supersedes the old generation by writing
	// a new one and repointing index.json.
	generation, err := newGeneration()
	if err != nil {
		return err
	}
	sessionDir := filepath.Join(s.summariesRoot(), sessionID)
	genDir := filepath.Join(sessionDir, generation)
	if err := os.MkdirAll(genDir, 0755); err != nil {
		return err
	}

	for i, item := range indexed {
		baseName := fmt.Sprintf("%02d-%s", i, slugify(item.section.Name))
		if err := atomicfs.Write(filepath.Join(genDir, baseName+".md"), []byte(item.section.Content), 0644); err != nil {
			return fmt.Errorf("write summary section: %w", err)
		}
		if err := WriteVec(filepath.Join(genDir, baseName+".vec"), item.vec); err != nil {
			return fmt.Errorf("write summary vec: %w", err)
		}
	}

	idx := summaryIndex{
		Generation:     generation,
		ProjectID:      projectID,
		ProjectName:    projectName,
		CreatedAt:      createdAt,
		CompactionPath: compactionPath,
	}
	data, err := json.Marshal(idx)
	if err != nil {
		return err
	}
	if err := atomicfs.Write(filepath.Join(sessionDir, "index.json"), data, 0644); err != nil {
		return fmt.Errorf("write summary index: %w", err)
	}
	return nil
}

// summaryIndex is the committed pointer for one session's summary. It names
// the current generation directory and carries the summary metadata.
type summaryIndex struct {
	Generation     string `json:"generation"`
	ProjectID      string `json:"project_id"`
	ProjectName    string `json:"project_name"`
	CreatedAt      string `json:"created_at"`
	CompactionPath string `json:"compaction_path"`
}

func newGeneration() (string, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("memory: generation id: %w", err)
	}
	return "gen-" + hex.EncodeToString(buf[:]), nil
}

func (s *Store) SearchHistory(query, projectID string, allProjects bool, limit int) ([]HistoryResult, error) {
	if s == nil {
		return nil, errEmbedderUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	embedder, err := s.requireEmbedder()
	if err != nil {
		return nil, err
	}
	qvec, err := embedder.Embed(query)
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}

	root := s.summariesRoot()
	sessions, err := os.ReadDir(root)
	if err != nil {
		return nil, nil
	}

	type vecWithSession struct {
		entry     VecEntry
		sessionID string
		idx       summaryIndex
	}
	var all []vecWithSession

	for _, sess := range sessions {
		if !sess.IsDir() {
			continue
		}
		sessionID := sess.Name()
		sessDir := filepath.Join(root, sessionID)

		idx, ok := readSummaryIndex(filepath.Join(sessDir, "index.json"))
		if !ok {
			continue
		}
		if !allProjects && idx.ProjectID != projectID {
			continue
		}

		genDir := filepath.Join(sessDir, idx.Generation)
		files, err := os.ReadDir(genDir)
		if err != nil {
			continue
		}
		for _, f := range files {
			if !strings.HasSuffix(f.Name(), ".vec") {
				continue
			}
			vecPath := filepath.Join(genDir, f.Name())
			vec, err := ReadVec(vecPath)
			if err != nil {
				continue
			}
			all = append(all, vecWithSession{
				entry:     VecEntry{Path: vecPath, Vec: vec},
				sessionID: sessionID,
				idx:       idx,
			})
		}
	}

	if len(all) == 0 {
		return nil, nil
	}

	entries := make([]VecEntry, len(all))
	for i, a := range all {
		entries[i] = a.entry
	}
	top := Search(qvec, entries, limit)

	byPath := make(map[string]vecWithSession, len(all))
	for _, a := range all {
		byPath[a.entry.Path] = a
	}

	var results []HistoryResult
	for _, e := range top {
		a := byPath[e.Path]
		mdPath := strings.TrimSuffix(e.Path, ".vec") + ".md"
		content, _ := os.ReadFile(mdPath)
		results = append(results, HistoryResult{
			SectionContent: string(content),
			SessionID:      a.sessionID,
			CreatedAt:      a.idx.CreatedAt,
			Project:        a.idx.ProjectName,
			CompactionPath: a.idx.CompactionPath,
		})
	}
	return results, nil
}

func (s *Store) DeleteSessionSummaries(sessionID string) error {
	if s == nil {
		return nil
	}
	if err := snapshot.ValidateSessionID(sessionID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	dir := filepath.Join(s.summariesRoot(), sessionID)
	return os.RemoveAll(dir)
}

func (s *Store) Reconcile() error {
	if s == nil {
		return errEmbedderUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	embedder, err := s.requireEmbedder()
	if err != nil {
		return err
	}
	projects, err := os.ReadDir(s.projectsRoot)
	if err != nil {
		return nil
	}
	for _, p := range projects {
		if !p.IsDir() {
			continue
		}
		memDir := filepath.Join(s.projectsRoot, p.Name(), "memories")
		files, err := os.ReadDir(memDir)
		if err != nil {
			continue
		}
		for _, f := range files {
			if !strings.HasSuffix(f.Name(), ".md") {
				continue
			}
			mdPath := filepath.Join(memDir, f.Name())
			vecPath := strings.TrimSuffix(mdPath, ".md") + ".vec"

			mdInfo, err := os.Stat(mdPath)
			if err != nil {
				continue
			}
			vecInfo, err := os.Stat(vecPath)
			if err == nil && !vecInfo.ModTime().Before(mdInfo.ModTime()) {
				continue
			}

			_, content, _, err := ReadMemoryFile(mdPath)
			if err != nil {
				continue
			}
			vec, err := embedder.Embed(content)
			if err != nil {
				if errors.Is(err, errEmbedderUnavailable) {
					return err
				}
				continue
			}
			_ = WriteVec(vecPath, vec)
		}
	}
	return nil
}

// Close releases store-local resources. The embedder is borrowed, not owned, so
// it is never closed here — closing one store must not disable the shared
// embedder that other stores still use. Its owner closes it once.
func (s *Store) Close() {}

func (s *Store) requireEmbedder() (memoryEmbedder, error) {
	if s == nil || s.embedder == nil {
		return nil, errEmbedderUnavailable
	}
	return s.embedder, nil
}

func readProjectName(metaPath string) string {
	data, err := os.ReadFile(metaPath)
	if err != nil {
		return ""
	}
	var m struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(data, &m)
	return m.Name
}

func readSummaryIndex(path string) (summaryIndex, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return summaryIndex{}, false
	}
	var idx summaryIndex
	if err := json.Unmarshal(data, &idx); err != nil || idx.Generation == "" {
		return summaryIndex{}, false
	}
	return idx, true
}
