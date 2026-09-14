package tool

import (
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"
	"strings"
)

// normalizeIntArg converts one consumed integer argument to an int from its
// exact json.Number lexeme. It accepts mathematically integral numbers that
// fit the consuming Go integer — including 1.0/1e0 spellings and integers
// beyond float64 precision — without float64 rounding, and rejects
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
		clean["offset"] = offset
	} else {
		clean["offset"] = 1
	}
	if _, present := clean["limit"]; present {
		limit, err := normalizeIntArg(clean, "read_file", "limit")
		if err != nil {
			return nil, err
		}
		if limit < 1 {
			limit = defaultLimit
		}
		clean["limit"] = limit
	} else {
		clean["limit"] = defaultLimit
	}
	return clean, nil
}

// NormalizeWriteArgs performs target write_file argument normalization:
// private `_lightcode_` fields are stripped and the schema-required path
// and content are strictly typed — wrong-typed fields are
// argument-validation errors, never silent coercion.
func NormalizeWriteArgs(args map[string]any) (map[string]any, error) {
	clean := stripPrivateArgs(args)
	path, _ := clean["path"].(string)
	if path == "" {
		return nil, fmt.Errorf("write_file: path is required")
	}
	content, present := clean["content"]
	if !present || content == nil {
		return nil, fmt.Errorf("write_file: content is required")
	}
	if _, ok := content.(string); !ok {
		return nil, fmt.Errorf("write_file: content must be a string")
	}
	return clean, nil
}

// NormalizeEditArgs performs target edit_file argument normalization:
// private `_lightcode_` fields are stripped and the schema-required fields
// are strictly typed — wrong-typed fields are argument-validation errors,
// never silent coercion. replace_all keeps its schema default when absent.
func NormalizeEditArgs(args map[string]any) (map[string]any, error) {
	clean := stripPrivateArgs(args)
	path, _ := clean["path"].(string)
	if path == "" {
		return nil, fmt.Errorf("edit_file: path is required")
	}
	for _, key := range []string{"old_string", "new_string"} {
		v, present := clean[key]
		if !present || v == nil {
			return nil, fmt.Errorf("edit_file: %s is required", key)
		}
		if _, ok := v.(string); !ok {
			return nil, fmt.Errorf("edit_file: %s must be a string", key)
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
// strict target normalization.
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
	clean["offset"] = offset
	limit := defaultLimit
	if v, ok := clean["limit"].(float64); ok {
		limit = int(v)
	}
	if limit < 1 {
		limit = defaultLimit
	}
	clean["limit"] = limit
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
