package harness

import (
	"math"

	"github.com/MMinasyan/lightcode/model"
)

// This file owns the read-time approximate USD cost estimate. It computes
// nothing durable: no estimate is ever persisted, cached, or written into a
// canonical UsageTotals, register, entry, or maintenance fact. Token
// accounting stays the checked integer arithmetic in usage.go.

// capturedPricingFor resolves the immutable captured pricing of one producer
// model identity inside one Operation capture: the conversation model's
// pricing for the conversation identity, the effective compact model's
// pricing for the compact identity, and unknown for any model a capture never
// priced. The current catalog is never consulted — historical producers are
// always priced by their own captures.
func capturedPricingFor(capture ExecutionCapture, ref model.ModelRef) *ModelPricing {
	if ref == capture.Model {
		return capture.Pricing
	}
	if ref == capture.Compact.Model {
		return capture.Compact.Pricing
	}
	return nil
}

// producerEstimateUSD computes one producer's approximate USD cost:
// (input*input_rate + cached_input*cache_read_rate + output*output_rate)/1e6.
// An absent usage report is unknown even when every rate is zero; only a
// nonzero counter requires its rate, so a present zero rate is a known zero
// and a present all-zero report is a known zero even with absent pricing. No
// cache-write count is ever invented from the captured cache-write rate, and
// a non-finite product is unknown.
func producerEstimateUSD(pricing *ModelPricing, usage *UsageCount) (float64, bool) {
	if usage == nil {
		return 0, false
	}
	var inputRate, cacheReadRate, outputRate *float64
	if pricing != nil {
		inputRate, cacheReadRate, outputRate = pricing.Input, pricing.CacheRead, pricing.Output
	}
	var total float64
	for _, term := range []struct {
		count int64
		rate  *float64
	}{
		{usage.InputTokens, inputRate},
		{usage.CachedInputTokens, cacheReadRate},
		{usage.OutputTokens, outputRate},
	} {
		if term.count == 0 {
			continue
		}
		if term.rate == nil {
			return 0, false
		}
		total += float64(term.count) * *term.rate
	}
	total /= 1e6
	if math.IsNaN(total) || math.IsInf(total, 0) {
		return 0, false
	}
	return total, true
}

// costEstimate accumulates approximate USD estimates across producers.
// Unknown is contagious: once any contributing producer is unknown, the sum
// is unknown and never presented as a partial-known total. A sum that
// overflows float64 to a non-finite value becomes unknown too. A producer
// carrying no estimate at all leaves the accumulator at its known-zero start.
type costEstimate struct {
	sum     float64
	unknown bool
}

func (e *costEstimate) add(value float64, ok bool) {
	if !ok {
		e.unknown = true
		return
	}
	if e.unknown {
		return
	}
	e.sum += value
	if math.IsNaN(e.sum) || math.IsInf(e.sum, 0) {
		e.unknown = true
	}
}

// value returns the estimated USD total as an owned pointer, or nil when the
// estimate is unknown. A session or Operation with no priced-out producer is
// a known zero.
func (e *costEstimate) value() *float64 {
	if e.unknown {
		return nil
	}
	total := e.sum
	return &total
}

// estimateGraphCosts walks the Session graph's committed entries exactly
// once and prices every original owned producer under its owning Operation's
// immutable capture — the validated graph guarantees every operation-owned
// entry its Operation register and capture: assistant entries under the
// conversation pricing, terminal no-output model producers (settlement Model
// with optional Usage) under their corresponding captured pricing, and each
// maintenance fact on a checkpoint or a settlement under the pricing captured
// for the fact's own model. Independently copied fork-prefix entries
// (operationless envelopes) and every informational signal — background
// completion reports included — contribute nothing, so a copied producer never
// charges twice and a copied absent usage never turns a zero-cost fork
// unknown. Tool results, inputs, and hook evidence are not model producers.
//
// It returns one estimate per Operation identity and the Session estimate
// summed over the same single walk, so one coherent SessionReadView needs one
// history scan, not one scan per Operation.
func estimateGraphCosts(graph *sessionGraph) (map[string]*costEstimate, *costEstimate) {
	byOperation := map[string]*costEstimate{}
	session := &costEstimate{}
	captures := make(map[string]ExecutionCapture, len(graph.Operations))
	for _, op := range graph.Operations {
		captures[op.Admission.OperationID] = op.Admission.Execution
	}
	produce := func(operationID string, pricing *ModelPricing, usage *UsageCount) {
		value, ok := producerEstimateUSD(pricing, usage)
		estimate, present := byOperation[operationID]
		if !present {
			estimate = &costEstimate{}
			byOperation[operationID] = estimate
		}
		estimate.add(value, ok)
		session.add(value, ok)
	}
	for i := range graph.Entries {
		entry := &graph.Entries[i]
		operationID := entry.Envelope.OperationID
		if operationID == "" {
			continue // copied fork-prefix entries and operationless signals
		}
		capture := captures[operationID]
		switch {
		case entry.Assistant != nil:
			produce(operationID, capture.Pricing, entry.Assistant.Usage)
		case entry.Settlement != nil:
			if entry.Settlement.Model != nil {
				produce(operationID, capturedPricingFor(capture, *entry.Settlement.Model), entry.Settlement.Usage)
			}
			for _, fact := range entry.Settlement.Facts {
				produce(operationID, capturedPricingFor(capture, fact.Model), fact.Usage)
			}
		case entry.Compaction != nil:
			for _, fact := range entry.Compaction.Facts {
				produce(operationID, capturedPricingFor(capture, fact.Model), fact.Usage)
			}
		}
	}
	return byOperation, session
}
