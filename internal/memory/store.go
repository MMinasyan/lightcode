package memory

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

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
	embedder     *Embedder
	projectsRoot string
	home         string
}

func NewStore(embedder *Embedder, projectsRoot, home string) *Store {
	return &Store{embedder: embedder, projectsRoot: projectsRoot, home: home}
}

func (s *Store) SaveMemory(memoriesDir, title, content string) (string, error) {
	fp, err := WriteMemoryFile(memoriesDir, title, content)
	if err != nil {
		return "", err
	}
	vec, err := s.embedder.Embed(content)
	if err != nil {
		os.Remove(fp)
		return "", fmt.Errorf("embed memory: %w", err)
	}
	vecPath := strings.TrimSuffix(fp, ".md") + ".vec"
	if err := WriteVec(vecPath, vec); err != nil {
		os.Remove(fp)
		return "", err
	}
	return fp, nil
}

func (s *Store) SearchMemory(query, projectID string, allProjects bool, limit int) ([]MemoryResult, error) {
	qvec, err := s.embedder.Embed(query)
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
	sections := SplitSummary(summary)
	if len(sections) == 0 {
		return nil
	}
	dir := filepath.Join(s.summariesRoot(), sessionID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	meta := summaryMeta{
		ProjectID:      projectID,
		ProjectName:    projectName,
		CreatedAt:      createdAt,
		CompactionPath: compactionPath,
	}
	metaData, _ := json.Marshal(meta)
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), metaData, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "lightcode: memory index meta: %v\n", err)
	}

	for i, sec := range sections {
		baseName := fmt.Sprintf("%02d-%s", i, slugify(sec.Name))
		mdPath := filepath.Join(dir, baseName+".md")
		if err := os.WriteFile(mdPath, []byte(sec.Content), 0644); err != nil {
			fmt.Fprintf(os.Stderr, "lightcode: memory index write: %v\n", err)
		}

		vec, err := s.embedder.Embed(sec.Content)
		if err != nil {
			continue
		}
		if err := WriteVec(filepath.Join(dir, baseName+".vec"), vec); err != nil {
			fmt.Fprintf(os.Stderr, "lightcode: memory index vec: %v\n", err)
		}
	}
	return nil
}

type summaryMeta struct {
	ProjectID      string `json:"project_id"`
	ProjectName    string `json:"project_name"`
	CreatedAt      string `json:"created_at"`
	CompactionPath string `json:"compaction_path"`
}

func (s *Store) SearchHistory(query, projectID string, allProjects bool, limit int) ([]HistoryResult, error) {
	qvec, err := s.embedder.Embed(query)
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
	}
	var all []vecWithSession

	for _, sess := range sessions {
		if !sess.IsDir() {
			continue
		}
		sessionID := sess.Name()
		sessDir := filepath.Join(root, sessionID)

		if !allProjects {
			meta := readSummaryMeta(filepath.Join(sessDir, "meta.json"))
			if meta.ProjectID != projectID {
				continue
			}
		}

		files, err := os.ReadDir(sessDir)
		if err != nil {
			continue
		}
		for _, f := range files {
			if !strings.HasSuffix(f.Name(), ".vec") {
				continue
			}
			vecPath := filepath.Join(sessDir, f.Name())
			vec, err := ReadVec(vecPath)
			if err != nil {
				continue
			}
			all = append(all, vecWithSession{
				entry:     VecEntry{Path: vecPath, Vec: vec},
				sessionID: sessionID,
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
		meta := readSummaryMeta(filepath.Join(filepath.Dir(e.Path), "meta.json"))
		results = append(results, HistoryResult{
			SectionContent: string(content),
			SessionID:      a.sessionID,
			CreatedAt:      meta.CreatedAt,
			Project:        meta.ProjectName,
			CompactionPath: meta.CompactionPath,
		})
	}
	return results, nil
}

func (s *Store) DeleteSessionSummaries(sessionID string) error {
	dir := filepath.Join(s.summariesRoot(), sessionID)
	return os.RemoveAll(dir)
}

func (s *Store) Reconcile() error {
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
			vec, err := s.embedder.Embed(content)
			if err != nil {
				continue
			}
			WriteVec(vecPath, vec)
		}
	}
	return nil
}

func (s *Store) Close() {
	if s.embedder != nil {
		s.embedder.Close()
	}
}

func readProjectName(metaPath string) string {
	data, err := os.ReadFile(metaPath)
	if err != nil {
		return ""
	}
	var m struct {
		Name string `json:"name"`
	}
	json.Unmarshal(data, &m)
	return m.Name
}

func readSummaryMeta(path string) summaryMeta {
	data, err := os.ReadFile(path)
	if err != nil {
		return summaryMeta{}
	}
	var m summaryMeta
	json.Unmarshal(data, &m)
	return m
}
