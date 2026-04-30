package lsp

import (
	"context"
	"encoding/json"
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

func (c *Client) WorkspaceSymbol(ctx context.Context, query string) (string, error) {
	instances := c.mgr.AllInstances()
	if len(instances) == 0 {
		return "No language servers available.", nil
	}

	params := protocol.WorkspaceSymbolParams{Query: query}
	type key struct {
		name string
		uri  string
		line int
	}
	seen := make(map[key]bool)
	var all []protocol.SymbolInformation

	for _, inst := range instances {
		if err := inst.waitReady(ctx); err != nil {
			continue
		}
		result, err := inst.call(ctx, "workspace/symbol", params)
		if err != nil {
			continue
		}
		var syms []protocol.SymbolInformation
		if err := json.Unmarshal(result, &syms); err != nil {
			continue
		}
		for _, s := range syms {
			k := key{s.Name, s.Location.URI, s.Location.Range.Start.Line}
			if seen[k] {
				continue
			}
			seen[k] = true
			all = append(all, s)
		}
	}

	if len(all) == 0 {
		return "No symbols found.", nil
	}

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
	return strings.TrimRight(b.String(), "\n"), nil
}

func (c *Client) GetDiagnostics(ctx context.Context, paths []string) (string, error) {
	type serverPaths struct {
		inst  *instance
		paths []string
	}
	groups := make(map[string]*serverPaths)

	for _, p := range paths {
		inst := c.mgr.ForFile(p)
		if inst == nil {
			continue
		}
		key := inst.def.Name
		if groups[key] == nil {
			groups[key] = &serverPaths{inst: inst}
		}
		groups[key].paths = append(groups[key].paths, p)
	}

	if len(groups) == 0 {
		return "No language servers available for the modified files.", nil
	}

	var openErrors []string
	for _, g := range groups {
		if err := g.inst.waitReady(ctx); err != nil {
			continue
		}
		for _, p := range g.paths {
			if err := g.inst.openFile(ctx, p); err != nil {
				openErrors = append(openErrors, fmt.Sprintf("%s: %v", p, err))
			}
		}
	}

	select {
	case <-time.After(500 * time.Millisecond):
	case <-ctx.Done():
		return "", ctx.Err()
	}

	var b strings.Builder
	for _, e := range openErrors {
		fmt.Fprintf(&b, "%s (could not check)\n", e)
	}
	totalErrors := 0
	for _, g := range groups {
		for _, p := range g.paths {
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

	if b.Len() == 0 {
		return "No errors found.", nil
	}
	return strings.TrimRight(b.String(), "\n"), nil
}
