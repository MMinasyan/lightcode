package lsp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/MMinasyan/lightcode/internal/lsp/protocol"
)

type Client struct {
	mgr *Manager
}

func NewClient(mgr *Manager) *Client {
	return &Client{mgr: mgr}
}

// WorkspaceSymbol fans one query out over every mapped instance. Each
// readiness, call or decode failure is collected and reported as that
// server's diagnostic; an instance that decoded any reply — including a valid
// empty one — counts as responding. When no instance exists, or no instance
// answered successfully, the collected failures are the returned error. A
// successful reply yields the merged, deduplicated symbols with the
// failed-server diagnostics appended.
func (c *Client) WorkspaceSymbol(ctx context.Context, query string) (string, error) {
	instances := c.mgr.AllInstances()
	if len(instances) == 0 {
		return "", errors.New("No language servers available.")
	}

	params := protocol.WorkspaceSymbolParams{Query: query}
	type key struct {
		name string
		uri  string
		line int
	}
	seen := make(map[key]bool)
	var all []protocol.SymbolInformation
	var failures []string
	replied := 0

	for _, inst := range instances {
		if err := inst.waitReady(ctx); err != nil {
			// An observed caller cancellation returns the symbols collected
			// so far with the cancellation error instead of swallowing it;
			// an unavailable server without cancellation contributes its
			// failure diagnostic.
			if ctx.Err() != nil {
				return formatSymbols(all), ctx.Err()
			}
			failures = append(failures, fmt.Sprintf("%s: %v", inst.def.Name, err))
			continue
		}
		result, err := inst.call(ctx, "workspace/symbol", params)
		if err != nil {
			if ctx.Err() != nil {
				return formatSymbols(all), ctx.Err()
			}
			failures = append(failures, fmt.Sprintf("%s: %v", inst.def.Name, err))
			continue
		}
		var syms []protocol.SymbolInformation
		if err := json.Unmarshal(result, &syms); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", inst.def.Name, err))
			continue
		}
		replied++
		for _, s := range syms {
			k := key{s.Name, s.Location.URI, s.Location.Range.Start.Line}
			if seen[k] {
				continue
			}
			seen[k] = true
			all = append(all, s)
		}
	}

	if replied == 0 {
		return "", errors.New(strings.Join(failures, "; "))
	}

	var b strings.Builder
	if len(all) == 0 {
		// Only the responding servers can speak for their own results: an
		// empty successful subset is not evidence that unavailable servers
		// found nothing.
		b.WriteString("No symbols were reported by the responding language servers.")
	} else {
		b.WriteString(formatSymbols(all))
	}
	for _, failure := range failures {
		b.WriteString("\n")
		b.WriteString(failure)
	}
	return b.String(), nil
}

// formatSymbols is the shared symbol formatting: the 20-symbol cap and the
// retained showing-count line.
func formatSymbols(all []protocol.SymbolInformation) string {
	cap := 20
	total := len(all)
	if len(all) > cap {
		all = all[:cap]
	}

	var b strings.Builder
	for _, s := range all {
		kind := protocol.SymbolKindName[s.Kind]
		if kind == "" {
			kind = "Symbol"
		}
		path := protocol.PathFromURI(s.Location.URI)
		line := s.Location.Range.Start.Line + 1
		fmt.Fprintf(&b, "%s (%s) — %s:%d\n", s.Name, kind, path, line)
	}
	if total > cap {
		fmt.Fprintf(&b, "\nShowing %d of %d total.", cap, total)
	}
	return strings.TrimRight(b.String(), "\n")
}

// GetDiagnosticsCanonical is the one diagnostics read: it groups the paths by
// serving instance, synchronizes every group through the canonical no-follow
// read — a replaced symlink leaf and non-regular or hardlinked files are
// refused, and no substituted content is ever sent — and samples only the
// successfully synchronized paths. Unsupported paths, readiness failures and
// document-open refusals become per-file could-not-check diagnostics: when
// none synchronizes, those collected failures are the ordinary error, returned
// without the sample wait, and only an entirely empty collection (no path at
// all) falls back to the plain unavailability text. Otherwise the result is
// explicitly an asynchronous cache sample that never certifies analysis of the
// latest edit.
func (c *Client) GetDiagnosticsCanonical(ctx context.Context, paths []string) (string, error) {
	type serverPaths struct {
		inst   *instance
		paths  []string
		synced []string
	}
	groups := make(map[string]*serverPaths)
	var refused []string

	for _, p := range paths {
		inst := c.mgr.ForFile(p)
		if inst == nil {
			refused = append(refused, fmt.Sprintf("%s: no language server for this file", p))
			continue
		}
		key := inst.def.Name
		if groups[key] == nil {
			groups[key] = &serverPaths{inst: inst}
		}
		groups[key].paths = append(groups[key].paths, p)
	}

	synced := 0
	for _, g := range groups {
		if err := g.inst.waitReady(ctx); err != nil {
			for _, p := range g.paths {
				refused = append(refused, fmt.Sprintf("%s: %v", p, err))
			}
			continue
		}
		for _, p := range g.paths {
			if err := g.inst.openFileCanonical(ctx, p); err != nil {
				refused = append(refused, fmt.Sprintf("%s: %v", p, err))
				continue
			}
			g.synced = append(g.synced, p)
			synced++
		}
	}

	if synced == 0 {
		// Nothing was synchronized, so there is nothing to sample: the
		// collected per-file failures are the outcome. An observed caller
		// cancellation still returns its error with the accumulated bounded
		// partial content, and an empty collection falls back to the plain
		// unavailability text.
		partial := couldNotCheckContent(refused)
		if err := ctx.Err(); err != nil {
			return partial, err
		}
		if partial == "" {
			return "", errors.New("No language servers available for the modified files.")
		}
		return "", errors.New(partial)
	}

	select {
	case <-time.After(500 * time.Millisecond):
	case <-ctx.Done():
		// The observed caller cancellation returns the could-not-check
		// content accumulated so far with the cancellation error instead of
		// swallowing it.
		return couldNotCheckContent(refused), ctx.Err()
	}

	var b strings.Builder
	totalErrors := 0
	for _, g := range groups {
		for _, p := range g.synced {
			if totalErrors >= 50 {
				break
			}
			uri := protocol.URIFromPath(p)
			diags := g.inst.fileDiagnostics(uri)
			fileErrors := 0
			for _, d := range diags {
				if d.Severity == nil || *d.Severity != protocol.SeverityError {
					continue
				}
				if fileErrors >= 20 || totalErrors >= 50 {
					break
				}
				fmt.Fprintf(&b, "%s:%d: %s\n", p, d.Range.Start.Line+1, d.Message)
				fileErrors++
				totalErrors++
			}
		}
	}
	if totalErrors == 0 {
		b.WriteString("No error diagnostics are currently reported for the synchronized files.\n")
	}
	if len(refused) > 0 {
		b.WriteString(couldNotCheckLines(refused))
	}
	b.WriteString("Diagnostics are asynchronous and may not reflect the latest edit.")
	return b.String(), nil
}

// couldNotCheckLines renders the per-file refused-path diagnostics: one
// "path: error (could not check)" line each.
func couldNotCheckLines(refused []string) string {
	var b strings.Builder
	for _, r := range refused {
		fmt.Fprintf(&b, "%s (could not check)\n", r)
	}
	return b.String()
}

// couldNotCheckContent is the same rendering without the trailing newline,
// the shape error and interrupted outcomes carry.
func couldNotCheckContent(refused []string) string {
	return strings.TrimRight(couldNotCheckLines(refused), "\n")
}
