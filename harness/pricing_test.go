package harness

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
)

// Captured pricing codec oracles: the capture carries the effective model's
// rates as durable capture data, and the updated settlement relation keeps a
// no-output model producer decodable.

func fptr(v float64) *float64 { return &v }

// TestExecutionCapturePricingCodec pins the durable pricing members: absent
// pricing encodes no member and decodes nil; present pricing round-trips with
// every present rate — a present zero stays present and distinct from absent;
// the closed key set, null rejection, non-number rejection, and finite-rate
// validation run on both the encode and the decode path.
func TestExecutionCapturePricingCodec(t *testing.T) {
	encode := func(t *testing.T, v ExecutionCapture) map[string]json.RawMessage {
		t.Helper()
		raw, err := encodeExecutionCapture(v)
		testassert.NoError(t, err)
		members, err := decodePayloadObject(raw)
		testassert.NoError(t, err)
		return members
	}

	// Absent pricing: no member on either capture.
	plain := encode(t, testCapture())
	if _, present := plain["pricing"]; present {
		t.Fatalf("capture with unknown pricing encoded a pricing member: %s", plain["pricing"])
	}
	plainCompact, err := decodePayloadObject(plain["compact"])
	testassert.NoError(t, err)
	if _, present := plainCompact["pricing"]; present {
		t.Fatalf("compact capture with unknown pricing encoded a pricing member: %s", plainCompact["pricing"])
	}

	// Present pricing with a present zero rate and an absent rate.
	v := testCapture()
	v.Pricing = &ModelPricing{Input: fptr(0), Output: fptr(2.5)}
	v.Compact.Pricing = &ModelPricing{CacheRead: fptr(0.25)}
	members := encode(t, v)
	pricingObj, err := decodePayloadObject(members["pricing"])
	testassert.NoError(t, err)
	testassert.Equal(t, string(pricingObj["input"]), "0")
	if _, present := pricingObj["cache_read"]; present {
		t.Fatalf("absent rate encoded as a member: %s", pricingObj["cache_read"])
	}
	decoded, err := decodeExecutionCapture(members)
	testassert.NoError(t, err)
	if !decoded.Pricing.Equal(v.Pricing) || !decoded.Compact.Pricing.Equal(v.Compact.Pricing) {
		t.Fatalf("round-tripped pricing = %+v / %+v, want %+v / %+v", decoded.Pricing, decoded.Compact.Pricing, v.Pricing, v.Compact.Pricing)
	}
	// A present zero and an absent rate stay distinct after the round trip.
	testassert.Require(t, decoded.Pricing.Input != nil && *decoded.Pricing.Input == 0 && decoded.Pricing.CacheRead == nil,
		"round-tripped pricing collapsed the present zero or the absent rate: %+v", decoded.Pricing)

	// An empty pricing object is present pricing whose rates are all absent.
	v.Pricing = &ModelPricing{}
	members = encode(t, v)
	decoded, err = decodeExecutionCapture(members)
	testassert.NoError(t, err)
	testassert.Require(t, decoded.Pricing != nil && decoded.Pricing.Input == nil, "empty pricing round-tripped to %+v", decoded.Pricing)

	for _, mutation := range []struct {
		name  string
		mutat func(map[string]json.RawMessage)
	}{
		{"null pricing", func(m map[string]json.RawMessage) { m["pricing"] = json.RawMessage("null") }},
		{"string pricing", func(m map[string]json.RawMessage) { m["pricing"] = json.RawMessage(`"cheap"`) }},
		{"null rate", func(m map[string]json.RawMessage) {
			obj := wireObject(t, m["pricing"])
			obj["input"] = json.RawMessage("null")
			m["pricing"] = mustRawJSON(obj)
		}},
		{"string rate", func(m map[string]json.RawMessage) {
			obj := wireObject(t, m["pricing"])
			obj["output"] = json.RawMessage(`"2.5"`)
			m["pricing"] = mustRawJSON(obj)
		}},
		{"unknown rate member", func(m map[string]json.RawMessage) {
			obj := wireObject(t, m["pricing"])
			obj["discount"] = json.RawMessage(`1`)
			m["pricing"] = mustRawJSON(obj)
		}},
		{"retired cache_write rate", func(m map[string]json.RawMessage) {
			obj := wireObject(t, m["pricing"])
			obj["cache_write"] = json.RawMessage(`0.25`)
			m["pricing"] = mustRawJSON(obj)
		}},
		{"compact null pricing", func(m map[string]json.RawMessage) {
			obj := wireObject(t, m["compact"])
			obj["pricing"] = json.RawMessage("null")
			m["compact"] = mustRawJSON(obj)
		}},
		{"compact unknown pricing member", func(m map[string]json.RawMessage) {
			obj := wireObject(t, m["compact"])
			inner := wireObject(t, obj["pricing"])
			inner["input_per_million"] = json.RawMessage(`1`)
			obj["pricing"] = mustRawJSON(inner)
			m["compact"] = mustRawJSON(obj)
		}},
		{"compact retired cache_write rate", func(m map[string]json.RawMessage) {
			obj := wireObject(t, m["compact"])
			inner := wireObject(t, obj["pricing"])
			inner["cache_write"] = json.RawMessage(`0.25`)
			obj["pricing"] = mustRawJSON(inner)
			m["compact"] = mustRawJSON(obj)
		}},
	} {
		m := encode(t, v)
		mutation.mutat(m)
		got, err := decodeExecutionCapture(m)
		testassert.Require(t, err != nil, "capture with %s decoded to %+v, want rejection", mutation.name, got)
	}

	// Non-finite Go rates are refused before persistence, so no invalid JSON
	// can ever carry them.
	for _, badRate := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		bad := testCapture()
		bad.Pricing = &ModelPricing{Input: fptr(badRate)}
		_, err := encodeExecutionCapture(bad)
		testassert.Require(t, err != nil, "capture with conversation rate %v encoded, want rejection", badRate)
		bad = testCapture()
		bad.Compact.Pricing = &ModelPricing{CacheRead: fptr(badRate)}
		_, err = encodeExecutionCapture(bad)
		testassert.Require(t, err != nil, "capture with compact rate %v encoded, want rejection", badRate)
	}
}

func mustRawJSON(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return raw
}

// TestOperationSettlementModelWithoutUsageRoundTrip pins the updated codec
// relation: usage requires its model identity, while a model identity alone
// encodes, decodes, and round-trips unchanged as the witness of a no-output
// terminal model producer.
func TestOperationSettlementModelWithoutUsageRoundTrip(t *testing.T) {
	v := validSettlementEntry()
	v.Status = OperationFailure
	v.Detail = "stream failed"
	ref := testModelRef()
	v.Model = &ref
	raw, err := encodeOperationSettlementEntry(v)
	testassert.NoError(t, err)
	if _, present := wireObject(t, raw)["usage"]; present {
		t.Fatalf("model-only settlement encoded a usage member: %s", raw)
	}
	decoded, err := decodeOperationSettlementEntry(Entry{
		SessionID: v.SessionID, ID: v.EntryID, OperationID: v.OperationID,
		Kind: EntryOperationSettlement, Payload: raw,
	})
	testassert.NoError(t, err)
	testassert.Require(t, decoded.Model != nil && *decoded.Model == ref && decoded.Usage == nil,
		"round-tripped model-only settlement = %+v, want the model identity with no usage member", decoded)
}
