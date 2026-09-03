package harness

import (
	"fmt"
	"math"
	"sort"

	"github.com/MMinasyan/lightcode/model"
)

// addInt64 returns a+b under checked signed arithmetic, reporting whether the
// sum is representable.
func addInt64(a, b int64) (int64, bool) {
	if b > 0 && a > math.MaxInt64-b {
		return 0, false
	}
	if b < 0 && a < math.MinInt64-b {
		return 0, false
	}
	return a + b, true
}

// addUsageCount returns a+b under checked arithmetic. Signed counts are
// preserved as landed: overflow returns the storage-failure class and no
// partial result.
func addUsageCount(a, b UsageCount) (UsageCount, error) {
	input, ok := addInt64(a.InputTokens, b.InputTokens)
	if !ok {
		return UsageCount{}, fmt.Errorf("%w: input token count overflows int64", ErrStorage)
	}
	cached, ok := addInt64(a.CachedInputTokens, b.CachedInputTokens)
	if !ok {
		return UsageCount{}, fmt.Errorf("%w: cached input token count overflows int64", ErrStorage)
	}
	output, ok := addInt64(a.OutputTokens, b.OutputTokens)
	if !ok {
		return UsageCount{}, fmt.Errorf("%w: output token count overflows int64", ErrStorage)
	}
	return UsageCount{InputTokens: input, CachedInputTokens: cached, OutputTokens: output}, nil
}

// addUsageTotals merges two totals into one unique, lexicographically sorted
// value (by provider, then model). Overflow on any count returns the
// storage-failure class and no partial result.
func addUsageTotals(a, b UsageTotals) (UsageTotals, error) {
	merged := make(map[model.ModelRef]UsageCount, len(a.ByModel)+len(b.ByModel))
	for _, mu := range a.ByModel {
		merged[mu.Model] = mu.Usage
	}
	for _, mu := range b.ByModel {
		prev, seen := merged[mu.Model]
		if !seen {
			merged[mu.Model] = mu.Usage
			continue
		}
		sum, err := addUsageCount(prev, mu.Usage)
		if err != nil {
			return UsageTotals{}, err
		}
		merged[mu.Model] = sum
	}
	out := UsageTotals{ByModel: make([]ModelUsage, 0, len(merged))}
	for ref, usage := range merged {
		out.ByModel = append(out.ByModel, ModelUsage{Model: ref, Usage: usage})
	}
	sortUsageTotals(out.ByModel)
	return out, nil
}

// sortUsageTotals orders by_model lexicographically by provider, then model,
// in place.
func sortUsageTotals(byModel []ModelUsage) {
	sort.Slice(byModel, func(i, j int) bool {
		if byModel[i].Model.Provider != byModel[j].Model.Provider {
			return byModel[i].Model.Provider < byModel[j].Model.Provider
		}
		return byModel[i].Model.Model < byModel[j].Model.Model
	})
}

// validateUsageTotals enforces the canonical totals shape: unique models
// sorted lexicographically by provider, then model, each identity complete.
func validateUsageTotals(t UsageTotals) error {
	for i := range t.ByModel {
		if t.ByModel[i].Model.Provider == "" || t.ByModel[i].Model.Model == "" {
			return fmt.Errorf("usage totals entry %d has an incomplete model identity", i)
		}
		if i > 0 {
			prev, cur := t.ByModel[i-1].Model, t.ByModel[i].Model
			if prev.Provider == cur.Provider && prev.Model == cur.Model {
				return fmt.Errorf("usage totals carry duplicate model %q", cur.String())
			}
			if prev.Provider > cur.Provider || (prev.Provider == cur.Provider && prev.Model > cur.Model) {
				return fmt.Errorf("usage totals are not sorted by provider then model at entry %d", i)
			}
		}
	}
	return nil
}

// usageTotalsEqual reports whether two canonical totals carry exactly the
// same models and counts.
func usageTotalsEqual(a, b UsageTotals) bool {
	if len(a.ByModel) != len(b.ByModel) {
		return false
	}
	for i := range a.ByModel {
		if a.ByModel[i].Model != b.ByModel[i].Model ||
			a.ByModel[i].Usage != b.ByModel[i].Usage {
			return false
		}
	}
	return true
}
