package clienthost

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/MMinasyan/lightcode/internal/clientops"
	"github.com/MMinasyan/lightcode/protocol"
)

// UsageError is an invalid utility argument, distinct from startup or transport failure.
type UsageError struct{ Message string }

func (e *UsageError) Error() string { return e.Message }
func (e *UsageError) ExitCode() int { return 2 }

// modelRow preserves the models utility's output vocabulary, not a wire DTO.
type modelRow struct {
	Ref             string               `json:"ref"`
	Provider        string               `json:"provider"`
	ProviderName    string               `json:"providerName"`
	Model           string               `json:"model"`
	DisplayName     string               `json:"displayName"`
	ContextWindow   int                  `json:"contextWindow"`
	MaxOutputTokens int                  `json:"maxOutputTokens"`
	Cost            *protocol.Cost       `json:"cost,omitempty"`
	Hidden          bool                 `json:"hidden"`
	ProviderHidden  bool                 `json:"providerHidden"`
	Incomplete      bool                 `json:"incomplete"`
	Default         bool                 `json:"default"`
	Source          protocol.ModelSource `json:"source"`
}

// RunModels lists models from one published configuration without owning
// Runtime work. Argument parsing belongs to the caller; provider validation
// uses the same configuration read.
func RunModels(ctx context.Context, opts Options, provider string, all, asJSON bool, out, diagnostics io.Writer) error {
	c, err := Connect(ctx, opts)
	if err != nil {
		return err
	}
	raw, err := clientops.Call(ctx, c, "GetConfiguration", nil)
	if err != nil {
		return err
	}
	// UseNumber keeps opaque numeric lexemes exact while typed fields keep
	// their generated type checks; only the output consumer decodes here.
	var view protocol.ConfigurationView
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&view); err != nil {
		return fmt.Errorf("decode configuration response: %w", err)
	}
	primaryModel := ""
	for _, agent := range view.Agents {
		if agent.Name == "primary" {
			primaryModel = string(agent.Model)
			break
		}
	}
	rows := []modelRow{}
	var known []string
	var match *protocol.Provider
	anyConnected := false
	for i := range view.Providers {
		p := &view.Providers[i]
		known = append(known, p.Id)
		if p.Id == provider {
			match = p
		}
		anyConnected = anyConnected || p.Connected
		if !p.Connected || (provider != "" && p.Id != provider) {
			continue
		}
		providerName := p.Name
		if providerName == "" {
			providerName = p.Id
		}
		for _, m := range p.Models {
			hidden := p.Hidden || m.Hidden
			if !all && (hidden || m.Incomplete) {
				continue
			}
			displayName := m.Name
			if displayName == "" {
				displayName = m.Id
			}
			ref := p.Id + "/" + m.Id
			rows = append(rows, modelRow{
				Ref: ref, Provider: p.Id, ProviderName: providerName, Model: m.Id, DisplayName: displayName,
				ContextWindow: m.ContextWindow, MaxOutputTokens: m.MaxOutputTokens, Cost: m.Cost,
				Hidden: hidden, ProviderHidden: p.Hidden, Incomplete: m.Incomplete, Default: ref == primaryModel, Source: m.Source,
			})
		}
	}
	hint := ""
	if provider != "" {
		if match == nil {
			return &UsageError{Message: fmt.Sprintf("unknown provider %q (known: %s)", provider, strings.Join(known, ", "))}
		}
		switch {
		case !match.Connected:
			hint = fmt.Sprintf("provider %q is not connected - connect it in Settings first", provider)
		case len(rows) == 0:
			hint = fmt.Sprintf("no visible models for %q - rerun with --all", provider)
		}
	} else if len(rows) == 0 {
		if anyConnected {
			hint = "no visible models - rerun with --all"
		} else {
			hint = "no models available - connect a provider in Settings first"
		}
	}
	if hint != "" {
		if _, err := fmt.Fprintln(diagnostics, hint); err != nil {
			return err
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Ref < rows[j].Ref })
	if asJSON {
		return json.NewEncoder(out).Encode(rows)
	}
	for _, row := range rows {
		var markers []string
		if row.Default {
			markers = append(markers, "default")
		}
		if row.Hidden {
			markers = append(markers, "hidden")
		}
		if row.Incomplete {
			markers = append(markers, "incomplete")
		}
		marker := "-"
		if len(markers) > 0 {
			marker = strings.Join(markers, ",")
		}
		if _, err := fmt.Fprintf(out, "%s\t%d\t%d\t%s\n", row.Ref, row.ContextWindow, row.MaxOutputTokens, marker); err != nil {
			return err
		}
	}
	return nil
}
