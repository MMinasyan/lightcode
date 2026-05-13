package provider

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// debugWireEnv toggles raw request body and SSE chunk dumps to
// ~/.lightcode/debug/wire/. Off by default; zero cost when unset.
const debugWireEnv = "LIGHTCODE_DEBUG_WIRE"

func debugWireEnabled() bool { return os.Getenv(debugWireEnv) == "1" }

func debugWireDir() string {
	if !debugWireEnabled() {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	dir := filepath.Join(home, ".lightcode", "debug", "wire")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ""
	}
	return dir
}

// debugDumpRequest writes the marshaled request body to a timestamped file
// and returns the matching chunks file path for the same exchange. Returns
// the empty string when wire-debug is off or the dump fails.
func debugDumpRequest(body []byte) string {
	dir := debugWireDir()
	if dir == "" {
		return ""
	}
	id := time.Now().UTC().Format("20060102-150405.000")
	if err := os.WriteFile(filepath.Join(dir, id+"-req.json"), body, 0o600); err != nil {
		return ""
	}
	return filepath.Join(dir, id+"-chunks.jsonl")
}

// debugAppendChunk appends one raw SSE chunk to chunkPath as one JSONL line.
// No-op when chunkPath is empty or the payload is empty.
func debugAppendChunk(chunkPath string, raw json.RawMessage) {
	if chunkPath == "" || len(raw) == 0 {
		return
	}
	f, err := os.OpenFile(chunkPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(raw)
	_, _ = f.Write([]byte{'\n'})
}
