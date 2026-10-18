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

// WorkspaceSymbol fans one query out over every mapped instance. An instance
// that decoded any reply — including a valid empty one — counts as responding;
// a readiness, call or decode failure simply omits that instance. With no
// mapped instance at all, and equally when no existing instance answered, the
// entry returns its plain unavailability error. A successful reply yields the
// merged, deduplicated symbols (or, for a valid empty reply, the no-symbols
// text), with unavailable or failed servers omitted.
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
	replied := 0

	for _, inst := range instances {
		if err := inst.waitReady(ctx); err != nil {
			// An observed caller cancellation returns the symbols collected
			// so far with the cancellation error instead of swallowing it;
			// without cancellation the failed server is simply omitted.
			if ctx.Err() != nil {
				return formatSymbols(all), ctx.Err()
			}
			continue
		}
		result, err := inst.call(ctx, "workspace/symbol", params)
		if err != nil {
			if ctx.Err() != nil {
				return formatSymbols(all), ctx.Err()
			}
			continue
		}
		var syms []protocol.SymbolInformation
		if err := json.Unmarshal(result, &syms); err != nil {
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
		return "", errors.New("No language servers available for the symbol query.")
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
// refused, and no substituted content is ever sent — and renders every grouped
// path's captured diagnostic cache. Unsupported paths, readiness failures and
// document-open refusals become per-file could-not-check diagnostics; when
// none synchronizes, those collected failures are the ordinary error, returned
// without the sample wait, and only an entirely empty collection (no path at
// all) falls back to the plain unavailability text. A group with no
// synchronized path has nothing a live server can speak for, and a group whose
// current attempt is no longer live when the sample is taken cannot supply
// success either — its restart-cleared cache is not evidence; both contribute
// their synchronized paths as could-not-check failures, and when no group
// remains eligible the collected failures are the outcome.
func (c *Client) GetDiagnosticsCanonical(ctx context.Context, paths []string) (string, error) {
	type serverPaths struct {
		inst   *instance
		paths  []string
		synced []string
		sample [][]protocol.Diagnostic
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
		return refusedOutcome(ctx, refused)
	}

	select {
	case <-time.After(500 * time.Millisecond):
	case <-ctx.Done():
		// The observed caller cancellation returns the could-not-check
		// content accumulated so far with the cancellation error instead of
		// swallowing it.
		return couldNotCheckContent(refused), ctx.Err()
	}

	// Only a group with at least one synchronized path can supply a sample,
	// and only while its current attempt is still up: a group with nothing
	// synchronized has nothing the live server can speak for, and a crashed,
	// retired or restarting instance has no live attempt behind its cache.
	// Either way its synced paths join the could-not-check collection, and
	// when nothing eligible remains the collected failures are the outcome.
	// Each eligible group's existing paths are captured once, in order, and
	// only those owned snapshots are rendered — including a refused path's
	// cached diagnostics when its group also has a live synchronized path.
	liveGroups := make([]*serverPaths, 0, len(groups))
	for _, g := range groups {
		if len(g.synced) == 0 {
			continue
		}
		sample, ok := g.inst.captureDiagnostics(g.paths)
		if !ok {
			for _, p := range g.synced {
				refused = append(refused, fmt.Sprintf("%s: %s is unavailable", p, g.inst.def.Name))
			}
			continue
		}
		g.sample = sample
		liveGroups = append(liveGroups, g)
	}
	if len(liveGroups) == 0 {
		return refusedOutcome(ctx, refused)
	}

	var b strings.Builder
	if len(refused) > 0 {
		b.WriteString(couldNotCheckLines(refused))
	}
	totalErrors := 0
	for _, g := range liveGroups {
		for i, p := range g.paths {
			if totalErrors >= 50 {
				break
			}
			fileErrors := 0
			for _, d := range g.sample[i] {
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

	if b.Len() == 0 {
		return "No errors found.", nil
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// refusedOutcome is the collected could-not-check outcome shared by the
// nothing-synchronized and nothing-live-to-sample gates: an observed caller
// cancellation returns the accumulated partial content with its error, an
// entirely empty collection falls back to the plain unavailability text, and
// anything else is the collected failure as the ordinary error.
func refusedOutcome(ctx context.Context, refused []string) (string, error) {
	partial := couldNotCheckContent(refused)
	if err := ctx.Err(); err != nil {
		return partial, err
	}
	if partial == "" {
		return "", errors.New("No language servers available for the modified files.")
	}
	return "", errors.New(partial)
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
