package memory

import (
	"encoding/binary"
	"math"
	"os"
	"sort"
)

func WriteVec(path string, vec []float32) error {
	buf := make([]byte, len(vec)*4)
	for i, v := range vec {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(v))
	}
	return os.WriteFile(path, buf, 0644)
}

func ReadVec(path string) ([]float32, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	n := len(data) / 4
	vec := make([]float32, n)
	for i := range n {
		vec[i] = math.Float32frombits(binary.LittleEndian.Uint32(data[i*4:]))
	}
	return vec, nil
}

func CosineSimilarity(a, b []float32) float32 {
	if len(a) != len(b) {
		return 0
	}
	var dot float32
	for i := range a {
		dot += a[i] * b[i]
	}
	return dot
}

type VecEntry struct {
	Path string
	Vec  []float32
}

type scoredEntry struct {
	entry VecEntry
	score float32
}

func Search(query []float32, entries []VecEntry, limit int) []VecEntry {
	scored := make([]scoredEntry, len(entries))
	for i, e := range entries {
		scored[i] = scoredEntry{entry: e, score: CosineSimilarity(query, e.Vec)}
	}
	sort.Slice(scored, func(i, j int) bool {
		return scored[i].score > scored[j].score
	})
	if limit > len(scored) {
		limit = len(scored)
	}
	result := make([]VecEntry, limit)
	for i := range limit {
		result[i] = scored[i].entry
	}
	return result
}
