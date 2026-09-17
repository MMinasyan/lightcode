package tool

import (
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"
)

// normalizeIntArg validates one consumed integer argument from its exact
// json.Number lexeme to a Go int. It accepts mathematically integral numbers
// that fit the consuming Go integer — including 1.0/1e0 spellings and
// integers beyond float64 precision — without float64 rounding, and rejects
// fractions, wrong types and overflow before the value reaches duration or
// index arithmetic. Absent and null arguments are handled by callers, which
// apply the retained defaults.
func normalizeIntArg(args map[string]any, tool, key string) (int, error) {
	n, ok := args[key].(json.Number)
	if !ok {
		return 0, fmt.Errorf("%s: %s must be an integer", tool, key)
	}
	return jsonNumberToInt(string(n), tool, key)
}

// canonicalInt is the canonical-integer json.Number lexeme of one validated
// integer: the one shared normalization representation for consumed
// integers. Strict and lenient normalization both emit it, marshaling
// preserves the exact lexeme, and preparation parses it back to int at its
// point of use.
func canonicalInt(i int) json.Number { return json.Number(strconv.Itoa(i)) }

// jsonNumberToInt parses the exact JSON number lexeme without float64
// rounding: plain integers via strconv, every other spelling via exact
// rational arithmetic so no fraction — however deep — can round to an
// integer.
func jsonNumberToInt(s, tool, key string) (int, error) {
	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		if i > maxInt || i < minInt {
			return 0, fmt.Errorf("%s: %s is too large", tool, key)
		}
		return int(i), nil
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return 0, fmt.Errorf("%s: %s must be an integer", tool, key)
	}
	if !r.IsInt() {
		return 0, fmt.Errorf("%s: %s must be a whole number", tool, key)
	}
	num := r.Num()
	if !num.IsInt64() {
		return 0, fmt.Errorf("%s: %s is too large", tool, key)
	}
	v := num.Int64()
	if v > maxInt || v < minInt {
		return 0, fmt.Errorf("%s: %s is too large", tool, key)
	}
	return int(v), nil
}

// maxInt/minInt are the platform int bounds as int64 for range checks.
const (
	maxInt = int64(^uint(0) >> 1)
	minInt = -maxInt - 1
)

// stripPrivateArgs copies args without model-supplied private `_lightcode_`
// fields, so no private receipt or canonical path can survive into
// executable authority. Unrelated schema-accepted fields are retained.
func stripPrivateArgs(args map[string]any) map[string]any {
	clean := make(map[string]any, len(args))
	for k, v := range args {
		if strings.HasPrefix(k, "_lightcode_") {
			continue
		}
		clean[k] = v
	}
	return clean
}

// NormalizeReadArgs performs target read_file argument normalization: it
// strips private `_lightcode_` fields, checks the JSON argument schema with
// strict consumed-field types and applies the retained defaults/clamps for
// offset and limit. It never touches the filesystem and preserves unrelated
// schema-accepted fields.
func NormalizeReadArgs(args map[string]any, defaultLimit int) (map[string]any, error) {
	clean := stripPrivateArgs(args)
	path, _ := clean["path"].(string)
	if path == "" {
		return nil, fmt.Errorf("read_file: path is required")
	}
	if _, present := clean["offset"]; present {
		offset, err := normalizeIntArg(clean, "read_file", "offset")
		if err != nil {
			return nil, err
		}
		if offset < 1 {
			offset = 1
		}
		clean["offset"] = canonicalInt(offset)
	} else {
		clean["offset"] = canonicalInt(1)
	}
	if _, present := clean["limit"]; present {
		limit, err := normalizeIntArg(clean, "read_file", "limit")
		if err != nil {
			return nil, err
		}
		if limit < 1 {
			limit = defaultLimit
		}
		clean["limit"] = canonicalInt(limit)
	} else {
		clean["limit"] = canonicalInt(defaultLimit)
	}
	return clean, nil
}

// NormalizeRunCommandArgs performs target run_command argument normalization:
// private `_lightcode_` fields are stripped, the required command must be a
// nonempty string, and a supplied background member of any JSON value —
// including null — is rejected because the target has no background path.
// The optional timeout is a strict consumed integer bounded by the
// seconds-to-duration conversion; an absent or sub-one value keeps the
// configured default, while a present null is rejected. The normalized form
// carries the effective timeout as its canonical-integer lexeme.
func NormalizeRunCommandArgs(args map[string]any, defaultTimeout int) (map[string]any, error) {
	clean := stripPrivateArgs(args)
	command, _ := clean["command"].(string)
	if command == "" {
		return nil, fmt.Errorf("run_command: command is required")
	}
	if _, present := clean["background"]; present {
		return nil, fmt.Errorf("run_command: background execution is not supported")
	}
	timeout := defaultTimeout
	if _, present := clean["timeout"]; present {
		v, err := normalizeIntArg(clean, "run_command", "timeout")
		if err != nil {
			return nil, err
		}
		if int64(v) > int64(math.MaxInt64/time.Second) {
			return nil, fmt.Errorf("run_command: timeout is too large")
		}
		if v >= 1 {
			timeout = v
		}
	}
	clean["timeout"] = canonicalInt(timeout)
	return clean, nil
}

// NormalizeSleepArgs performs target sleep argument normalization: private
// `_lightcode_` fields are stripped and a present seconds must be a strict
// integer before the retained 1..300 clamp applies; an absent seconds keeps
// the retained default of 1. The normalized form carries the clamped value
// as its canonical-integer lexeme.
func NormalizeSleepArgs(args map[string]any) (map[string]any, error) {
	clean := stripPrivateArgs(args)
	if _, present := clean["seconds"]; present {
		v, err := normalizeIntArg(clean, "sleep", "seconds")
		if err != nil {
			return nil, err
		}
		clean["seconds"] = canonicalInt(normalizeSleepSeconds(v))
	} else {
		clean["seconds"] = canonicalInt(1)
	}
	return clean, nil
}

// NormalizeWriteArgs performs target write_file argument normalization:
// private `_lightcode_` fields are stripped and the schema-required path
// and content must be strings — an absent, null or wrong-typed required
// field is the same argument-validation error, never silent coercion.
func NormalizeWriteArgs(args map[string]any) (map[string]any, error) {
	clean := stripPrivateArgs(args)
	path, _ := clean["path"].(string)
	if path == "" {
		return nil, fmt.Errorf("write_file: path is required")
	}
	if _, ok := clean["content"].(string); !ok {
		return nil, fmt.Errorf("write_file: content is required")
	}
	return clean, nil
}

// NormalizeEditArgs performs target edit_file argument normalization:
// private `_lightcode_` fields are stripped and the schema-required fields
// must be strings — an absent, null or wrong-typed required field is the
// same argument-validation error, never silent coercion. replace_all keeps
// its schema default when absent.
func NormalizeEditArgs(args map[string]any) (map[string]any, error) {
	clean := stripPrivateArgs(args)
	path, _ := clean["path"].(string)
	if path == "" {
		return nil, fmt.Errorf("edit_file: path is required")
	}
	for _, key := range []string{"old_string", "new_string"} {
		if _, ok := clean[key].(string); !ok {
			return nil, fmt.Errorf("edit_file: %s is required", key)
		}
	}
	if v, present := clean["replace_all"]; present {
		if _, ok := v.(bool); !ok {
			return nil, fmt.Errorf("edit_file: replace_all must be a boolean")
		}
	}
	return clean, nil
}

// NormalizePatchArgs performs target apply_patch argument normalization:
// private `_lightcode_` fields are stripped and the patch payload text is
// preserved verbatim. The input member must be a string when present;
// a wrong type is an argument-validation error, never a later patch-syntax
// error. V4A parsing belongs to canonical preparation.
func NormalizePatchArgs(args map[string]any) (map[string]any, error) {
	clean := stripPrivateArgs(args)
	if v, present := clean["input"]; present {
		if _, ok := v.(string); !ok {
			return nil, fmt.Errorf("apply_patch: input must be a string")
		}
	}
	return clean, nil
}

// normalizeReadArgsLegacy is the retained lenient legacy read_file
// argument parsing: float64 offsets/limits truncate and nonpositive values
// take the retained clamps; wrong-typed values keep their retained
// defaults. It feeds the same shared preparation/execution bodies as the
// strict target normalization and emits the same canonical-integer
// json.Number representation for consumed integers.
func normalizeReadArgsLegacy(args map[string]any, defaultLimit int) (map[string]any, error) {
	clean := stripPrivateArgs(args)
	path, _ := clean["path"].(string)
	if path == "" {
		return nil, fmt.Errorf("read_file: path is required")
	}
	offset := 1
	if v, ok := clean["offset"].(float64); ok {
		offset = int(v)
	}
	if offset < 1 {
		offset = 1
	}
	clean["offset"] = canonicalInt(offset)
	limit := defaultLimit
	if v, ok := clean["limit"].(float64); ok {
		limit = int(v)
	}
	if limit < 1 {
		limit = defaultLimit
	}
	clean["limit"] = canonicalInt(limit)
	return clean, nil
}

// normalizePathArgsLegacy is the retained lenient legacy argument parsing
// shared by the path-argument tools: private fields are stripped and the
// string path argument is required; every other member keeps its tolerant
// default.
func normalizePathArgsLegacy(args map[string]any, toolName string) (map[string]any, error) {
	clean := stripPrivateArgs(args)
	path, _ := clean["path"].(string)
	if path == "" {
		return nil, fmt.Errorf("%s: path is required", toolName)
	}
	return clean, nil
}
