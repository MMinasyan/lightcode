package catalog

import (
	"errors"
	"fmt"
	"sort"
)

// Catalog lookup errors.
var (
	ErrUnknownProvider = errors.New("catalog: unknown provider")
	ErrUnknownModel    = errors.New("catalog: unknown model")
	ErrIncomplete      = errors.New("catalog: incomplete model")
)

// Lookup returns the provider and model for ref, rejecting incomplete models.
func (c *Catalog) Lookup(ref ModelRef) (*Provider, *Model, error) {
	provider, model, err := c.LookupOrIncomplete(ref)
	if err != nil {
		return nil, nil, err
	}
	if _, ok := model.Incomplete(); ok {
		return nil, nil, fmt.Errorf("%w: %s", ErrIncomplete, ref.String())
	}
	return provider, model, nil
}

// LookupOrIncomplete returns the provider and model for ref, allowing incomplete models.
func (c *Catalog) LookupOrIncomplete(ref ModelRef) (*Provider, *Model, error) {
	if c == nil || c.Providers == nil {
		return nil, nil, fmt.Errorf("%w: %s", ErrUnknownProvider, ref.Provider)
	}
	provider, ok := c.Providers[ref.Provider]
	if !ok || provider == nil {
		return nil, nil, fmt.Errorf("%w: %s", ErrUnknownProvider, ref.Provider)
	}
	model, ok := provider.Models[ref.Model]
	if !ok || model == nil {
		return nil, nil, fmt.Errorf("%w: %s", ErrUnknownModel, ref.String())
	}
	return provider, model, nil
}

// VisibleModels returns non-hidden provider/model refs sorted by prefix string.
func (c *Catalog) VisibleModels() []ModelRef {
	if c == nil {
		return nil
	}
	var refs []ModelRef
	for providerID, provider := range c.Providers {
		if provider == nil || provider.Hidden {
			continue
		}
		for modelID, model := range provider.Models {
			if model == nil || model.Hidden {
				continue
			}
			refs = append(refs, ModelRef{Provider: providerID, Model: modelID})
		}
	}
	sortModelRefs(refs)
	return refs
}

// AllModels returns every provider/model ref sorted by prefix string.
func (c *Catalog) AllModels() []ModelRef {
	if c == nil {
		return nil
	}
	var refs []ModelRef
	for providerID, provider := range c.Providers {
		if provider == nil {
			continue
		}
		for modelID, model := range provider.Models {
			if model == nil {
				continue
			}
			refs = append(refs, ModelRef{Provider: providerID, Model: modelID})
		}
	}
	sortModelRefs(refs)
	return refs
}

func sortModelRefs(refs []ModelRef) {
	sort.Slice(refs, func(i, j int) bool {
		return refs[i].String() < refs[j].String()
	})
}
