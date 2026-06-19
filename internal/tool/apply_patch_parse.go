package tool

import (
	"errors"
	"fmt"
	"strings"
)

const (
	applyPatchBeginMarker  = "*** Begin Patch"
	applyPatchEndMarker    = "*** End Patch"
	applyPatchAddPrefix    = "*** Add File: "
	applyPatchUpdatePrefix = "*** Update File: "
	applyPatchDeletePrefix = "*** Delete File: "
	applyPatchMovePrefix   = "*** Move to: "
	applyPatchHunkPrefix   = "@@"
)

var (
	errApplyPatchEmpty        = errors.New("apply_patch: input is empty")
	errApplyPatchNoBegin      = errors.New("apply_patch: input must start with \"*** Begin Patch\"")
	errApplyPatchNoEnd        = errors.New("apply_patch: input must end with \"*** End Patch\"")
	errApplyPatchNoOps        = errors.New("apply_patch: patch contains no operations")
	errApplyPatchDupPath      = errors.New("apply_patch: duplicate path in patch")
	errApplyPatchUnknownHead  = errors.New("apply_patch: unknown section header")
	errApplyPatchAddBody      = errors.New("apply_patch: Add File body lines must start with \"+\"")
	errApplyPatchHunkLine     = errors.New("apply_patch: hunk line must start with \" \", \"-\", or \"+\"")
	errApplyPatchHunkExpected = errors.New("apply_patch: expected a section header")
)

type opKind int

const (
	opAdd opKind = iota
	opUpdate
	opDelete
)

type lineKind int

const (
	lineContext lineKind = iota
	lineAdd
	lineRemove
)

type hunkLine struct {
	kind lineKind
	text string
}

type hunk struct {
	anchor string
	lines  []hunkLine
}

type fileOp struct {
	kind     opKind
	path     string
	movePath string
	hunks    []hunk
}

type patch struct {
	ops []fileOp
}

func parsePatch(input string) (*patch, error) {
	if strings.TrimSpace(input) == "" {
		return nil, errApplyPatchEmpty
	}

	// Decision 17: split on \n only; \r rides inside lines as raw bytes.
	lines := strings.Split(input, "\n")
	// Trim a single trailing empty produced by a final newline so the
	// envelope's last line lands on *** End Patch exactly.
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) < 2 {
		return nil, errApplyPatchNoEnd
	}
	if lines[0] != applyPatchBeginMarker {
		return nil, errApplyPatchNoBegin
	}
	if lines[len(lines)-1] != applyPatchEndMarker {
		return nil, errApplyPatchNoEnd
	}
	body := lines[1 : len(lines)-1]

	p := &patch{}
	seen := map[string]bool{}
	i := 0
	for i < len(body) {
		line := body[i]
		switch {
		case strings.HasPrefix(line, applyPatchAddPrefix):
			path := strings.TrimSpace(strings.TrimPrefix(line, applyPatchAddPrefix))
			if path == "" {
				return nil, fmt.Errorf("apply_patch: Add File: path is empty")
			}
			if seen[path] {
				return nil, fmt.Errorf("%w: %s", errApplyPatchDupPath, path)
			}
			op := fileOp{kind: opAdd, path: path}
			i++
			for i < len(body) {
				b := body[i]
				if strings.HasPrefix(b, "*** ") {
					break
				}
				if !strings.HasPrefix(b, "+") {
					return nil, fmt.Errorf("%w: %q", errApplyPatchAddBody, b)
				}
				op.hunks = append(op.hunks, hunk{lines: []hunkLine{{kind: lineAdd, text: b[1:]}}})
				i++
			}
			if len(op.hunks) == 0 {
				return nil, fmt.Errorf("apply_patch: Add File %q has no content", path)
			}
			seen[path] = true
			p.ops = append(p.ops, op)
		case strings.HasPrefix(line, applyPatchUpdatePrefix):
			path := strings.TrimSpace(strings.TrimPrefix(line, applyPatchUpdatePrefix))
			if path == "" {
				return nil, fmt.Errorf("apply_patch: Update File: path is empty")
			}
			if seen[path] {
				return nil, fmt.Errorf("%w: %s", errApplyPatchDupPath, path)
			}
			op := fileOp{kind: opUpdate, path: path}
			i++
			if i < len(body) {
				next := body[i]
				if strings.HasPrefix(next, applyPatchMovePrefix) {
					movePath := strings.TrimSpace(strings.TrimPrefix(next, applyPatchMovePrefix))
					if movePath == "" {
						return nil, fmt.Errorf("apply_patch: Move to: path is empty")
					}
					if seen[movePath] {
						return nil, fmt.Errorf("%w: %s", errApplyPatchDupPath, movePath)
					}
					op.movePath = movePath
					seen[movePath] = true
					i++
				}
			}
			for i < len(body) {
				b := body[i]
				if strings.HasPrefix(b, "*** ") {
					break
				}
				if !strings.HasPrefix(b, applyPatchHunkPrefix) {
					return nil, fmt.Errorf("apply_patch: expected @@, got %q", b)
				}
				anchor := strings.TrimSpace(strings.TrimPrefix(b, applyPatchHunkPrefix))
				h := hunk{anchor: anchor}
				i++
				if i < len(body) && (strings.HasPrefix(body[i], "*** ") || strings.HasPrefix(body[i], applyPatchHunkPrefix)) {
					return nil, fmt.Errorf("apply_patch: hunk has no body lines")
				}
				for i < len(body) {
					b := body[i]
					if strings.HasPrefix(b, "*** ") || strings.HasPrefix(b, applyPatchHunkPrefix) {
						break
					}
					if len(b) == 0 {
						return nil, fmt.Errorf("%w: empty line", errApplyPatchHunkLine)
					}
					switch b[0] {
					case ' ':
						h.lines = append(h.lines, hunkLine{kind: lineContext, text: b[1:]})
					case '-':
						h.lines = append(h.lines, hunkLine{kind: lineRemove, text: b[1:]})
					case '+':
						h.lines = append(h.lines, hunkLine{kind: lineAdd, text: b[1:]})
					default:
						return nil, fmt.Errorf("%w: %q", errApplyPatchHunkLine, b)
					}
					i++
				}
				if len(h.lines) == 0 {
					return nil, fmt.Errorf("apply_patch: hunk has no body lines")
				}
				op.hunks = append(op.hunks, h)
			}
			if len(op.hunks) == 0 {
				return nil, fmt.Errorf("apply_patch: Update File %q has no hunks", path)
			}
			seen[path] = true
			p.ops = append(p.ops, op)
		case strings.HasPrefix(line, applyPatchDeletePrefix):
			path := strings.TrimSpace(strings.TrimPrefix(line, applyPatchDeletePrefix))
			if path == "" {
				return nil, fmt.Errorf("apply_patch: Delete File: path is empty")
			}
			if seen[path] {
				return nil, fmt.Errorf("%w: %s", errApplyPatchDupPath, path)
			}
			seen[path] = true
			p.ops = append(p.ops, fileOp{kind: opDelete, path: path})
			i++
		default:
			if strings.HasPrefix(line, "*** ") {
				return nil, fmt.Errorf("%w: %q", errApplyPatchUnknownHead, line)
			}
			return nil, fmt.Errorf("%w: %q", errApplyPatchHunkExpected, line)
		}
	}

	if len(p.ops) == 0 {
		return nil, errApplyPatchNoOps
	}
	return p, nil
}
