package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/MMinasyan/lightcode/internal/config"
)

// TestStartupDoesNotDeriveMemoryArtifacts is the Step-2 memory user-data
// regression. Sentinel files are seeded deep inside three memory-adjacent path
// families — an isolated project memories tree, the whole summaries/ family, and
// the whole cache/models/bge-small-en-v1.5/ bundle family — each placed in a
// subdirectory so any derived artifact anywhere inside a family parent is caught
// as an added sibling. The complete seeded tree of every family parent is
// snapshotted before startup/shutdown and compared after; no file may be added,
// removed, or changed, so no memory-derived artifact (a vector beside a seeded
// memory, an embedding written under the model cache, a new summary-index bundle)
// can appear. Before memory was removed, startup reconciled the memories root and
// embedded every seeded memory into a sibling .vec, and the embedder wrote files
// under the model cache; removal means startup/shutdown never reconcile or index,
// so nothing derived is created and user files are untouched.
func TestStartupDoesNotDeriveMemoryArtifacts(t *testing.T) {
	home := t.TempDir()
	projectRoot := t.TempDir()
	lightcodeDir := filepath.Join(home, ".lightcode")
	if err := os.MkdirAll(lightcodeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(lightcodeDir, "config.json")
	if err := os.WriteFile(configPath, []byte(`{"providers": {}, "default_model": ""}`), 0o600); err != nil {
		t.Fatal(err)
	}

	projectsRoot := filepath.Join(lightcodeDir, "projects")
	memoriesRoot := filepath.Join(projectsRoot, "sentinelproj", "memories")               // isolated seeded project memories tree (retained)
	summariesRoot := filepath.Join(lightcodeDir, "summaries")                             // full summaries family parent
	cacheModelRoot := filepath.Join(lightcodeDir, "cache", "models", "bge-small-en-v1.5") // full bge bundle family parent

	roots := []string{memoriesRoot, summariesRoot, cacheModelRoot}
	seed := map[string][]byte{
		filepath.Join(memoriesRoot, "sentinel.md"):                    []byte("---\ntitle: Keep\ncreated_at: 2026-01-01T00:00:00Z\n---\n\nbody text to embed\n"),
		filepath.Join(summariesRoot, "sentinelsumm", "sentinel.json"): []byte(`{"keep": true}`),
		filepath.Join(cacheModelRoot, "onnx", "tokenizer.json"):       []byte("{\"vocab\":[]}\n"),
		filepath.Join(cacheModelRoot, "onnx", "model.onnx"):           []byte("keep this bundle untouched\n"),
	}
	for path, content := range seed {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	before := memoryRemovedSnapshotTree(t, roots)

	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	a, err := New(Config{Cfg: cfg, ConfigPath: configPath, ProjectRoot: projectRoot, Home: home})
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.Init(ctx)
	a.ShutdownOwner()

	memoryRemovedDiffTrees(t, before, memoryRemovedSnapshotTree(t, roots), roots)
}

// memoryRemovedSnapshotTree walks each root and maps every regular file's
// absolute path to a hex sha256 of its contents, so any added, removed, or
// changed file is detectable regardless of where inside a tree it appears.
func memoryRemovedSnapshotTree(t *testing.T, roots []string) map[string]string {
	t.Helper()
	snap := make(map[string]string)
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(data)
			snap[path] = hex.EncodeToString(sum[:])
			return nil
		})
		if err != nil {
			t.Fatalf("snapshot %s: %v", root, err)
		}
	}
	return snap
}

// memoryRemovedDiffTrees fails listing every file that differs between the before
// and after snapshots across all seeded trees.
func memoryRemovedDiffTrees(t *testing.T, before, after map[string]string, roots []string) {
	t.Helper()
	var missing, added, changed []string
	for path, sum := range before {
		switch {
		case after[path] == "":
			missing = append(missing, path)
		case after[path] != sum:
			changed = append(changed, path)
		}
	}
	for path := range after {
		if _, ok := before[path]; !ok {
			added = append(added, path)
		}
	}
	sort.Strings(missing)
	sort.Strings(added)
	sort.Strings(changed)
	if len(missing)+len(added)+len(changed) == 0 {
		return
	}
	t.Fatalf("seeded memory-adjacent trees changed on startup/shutdown:\n  added: %v\n  removed: %v\n  changed: %v", added, missing, changed)
}
