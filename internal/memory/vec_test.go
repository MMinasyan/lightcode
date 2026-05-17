package memory

import (
	"math"
	"path/filepath"
	"reflect"
	"testing"
)

func TestCosineSimilarityDotProduct(t *testing.T) {
	if got := CosineSimilarity([]float32{1, 0}, []float32{1, 0}); math.Abs(float64(got-1)) > 1e-6 {
		t.Fatalf("identity similarity = %v, want 1", got)
	}
	if got := CosineSimilarity([]float32{1, 0}, []float32{0, 1}); got != 0 {
		t.Fatalf("orthogonal similarity = %v, want 0", got)
	}
	if got := CosineSimilarity([]float32{1}, []float32{1, 2}); got != 0 {
		t.Fatalf("mismatched similarity = %v, want 0", got)
	}
}

func TestSearchOrdersByScoreAndLimits(t *testing.T) {
	entries := []VecEntry{{Path: "low", Vec: []float32{0, 1}}, {Path: "high", Vec: []float32{2, 0}}, {Path: "mid", Vec: []float32{1, 0}}}
	got := Search([]float32{1, 0}, entries, 2)
	if len(got) != 2 || got[0].Path != "high" || got[1].Path != "mid" {
		t.Fatalf("Search top2 = %+v", got)
	}
	if got := Search([]float32{1, 0}, entries, 99); len(got) != len(entries) {
		t.Fatalf("Search oversized limit length = %d, want %d", len(got), len(entries))
	}
}

func TestWriteReadVecRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v.vec")
	want := []float32{1.25, -2, 0}
	if err := WriteVec(path, want); err != nil {
		t.Fatalf("WriteVec: %v", err)
	}
	got, err := ReadVec(path)
	if err != nil {
		t.Fatalf("ReadVec: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ReadVec = %v, want %v", got, want)
	}
	if _, err := ReadVec(filepath.Join(t.TempDir(), "missing.vec")); err == nil {
		t.Fatal("ReadVec missing error = nil, want error")
	}
}
