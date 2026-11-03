package clientview_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/MMinasyan/lightcode/internal/clientview"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/protocol"
)

// The shared projection corpus stores every payload as raw JSON text, so an
// outer parse can never round or retype a clock lexeme before the client view
// or the generated union sees it. The TypeScript projection test reads the
// same file. Its typescript_only section carries raw JSON numbers: an unquoted
// JSON number does not become a generated Go string, so decoding those
// deliveries at their generated boundary fails — TestClientViewDecoderBoundary
// records that actual decode error. The TypeScript test asserts its own runtime
// typeof-string rejection. Neither claim is generic schema validation.
const projectionCorpusPath = "../../protocol/testdata/client_projection_cases.json"

type projectionCorpus struct {
	Counters []struct {
		Name, A, B, Result string
		Error, Foreign     bool
	} `json:"counters"`
	Compare        []compareCase        `json:"compare"`
	Accept         []acceptCase         `json:"accept"`
	ItemIDs        []itemIDCase         `json:"item_ids"`
	Merge          []mergeCase          `json:"merge"`
	TypeScriptOnly []typescriptOnlyCase `json:"typescript_only"`
}

func TestIndependentRevisionComparators(t *testing.T) {
	const instance = "0123456789abcdef0123456789abcdef"
	orders := map[string]int{"older": -1, "equal": 0, "newer": 1}
	corpus := loadProjectionCorpus(t)
	testassert.Require(t, len(corpus.Counters) != 0, "the shared corpus has no counter cases")
	for _, test := range corpus.Counters {
		t.Run(test.Name, func(t *testing.T) {
			rightInstance := instance
			if test.Foreign {
				rightInstance = "ffffffffffffffffffffffffffffffff"
			}
			configuration, configErr := clientview.CompareConfigurationRevision(protocol.ConfigurationRevision{InstanceId: instance, Generation: test.A}, protocol.ConfigurationRevision{InstanceId: rightInstance, Generation: test.B})
			warnings, warningErr := clientview.CompareWarningsRevision(protocol.WarningsRevision{InstanceId: instance, Revision: test.A}, protocol.WarningsRevision{InstanceId: rightInstance, Revision: test.B})
			testassert.Require(t, (configErr != nil) == test.Error && (warningErr != nil) == test.Error && (test.Error || configuration == orders[test.Result] && warnings == orders[test.Result]), "config %d/%v warnings %d/%v", configuration, configErr, warnings, warningErr)
		})
	}
}

type compareCase struct {
	Name   string `json:"name"`
	A      string `json:"a"`
	B      string `json:"b"`
	Result string `json:"result"`
	Error  bool   `json:"error"`
}

type acceptCase struct {
	Name          string  `json:"name"`
	Current       *string `json:"current"`
	Next          string  `json:"next"`
	Instance      string  `json:"instance"`
	Configuration *string `json:"configuration"`
	Warnings      *string `json:"warnings"`
	Accept        bool    `json:"accept"`
	Error         bool    `json:"error"`
}

type itemIDCase struct {
	Name  string `json:"name"`
	Item  string `json:"item"`
	ID    string `json:"id"`
	Error bool   `json:"error"`
}

type mergeCase struct {
	Name   string `json:"name"`
	Older  string `json:"older"`
	Newer  string `json:"newer"`
	Result string `json:"result"`
	Error  bool   `json:"error"`
}

type typescriptOnlyCase struct {
	Name     string `json:"name"`
	Function string `json:"function"`
	A        string `json:"a"`
	B        string `json:"b"`
	Next     string `json:"next"`
}

func loadProjectionCorpus(t *testing.T) projectionCorpus {
	t.Helper()
	data, err := os.ReadFile(filepath.FromSlash(projectionCorpusPath))
	testassert.NoError(t, err)
	var corpus projectionCorpus
	testassert.NoError(t, json.Unmarshal(data, &corpus))
	return corpus
}

// TestCompareSessionRevision runs the shared corpus over the component-wise
// partial order: older/equal/newer outcomes and every mixed, malformed,
// out-of-range or foreign case that must stay an error.
func TestCompareSessionRevision(t *testing.T) {
	corpus := loadProjectionCorpus(t)
	testassert.Require(t, len(corpus.Compare) != 0, "the shared projection corpus has no compare cases")
	orders := map[string]int{"older": -1, "equal": 0, "newer": 1}
	for _, testCase := range corpus.Compare {
		t.Run(testCase.Name, func(t *testing.T) {
			a := decodeRevision[protocol.SessionRevision](t, testCase.A)
			b := decodeRevision[protocol.SessionRevision](t, testCase.B)
			got, err := clientview.CompareSessionRevision(a, b)
			if testCase.Error {
				testassert.Require(t, err != nil, "CompareSessionRevision(%s, %s) = %d, nil; want an error", testCase.A, testCase.B, got)
				return
			}
			testassert.Require(t, err == nil, "CompareSessionRevision(%s, %s): %v", testCase.A, testCase.B, err)
			want, ok := orders[testCase.Result]
			testassert.Require(t, ok, "corpus case %q names unknown result %q", testCase.Name, testCase.Result)
			testassert.Require(t, got == want, "CompareSessionRevision(%s, %s) = %d, want %d", testCase.A, testCase.B, got, want)
		})
	}
}

// TestHydrationIndependentClocks runs the shared corpus over acceptance:
// stale, equal and newer Session/configuration/warning clocks, independent
// configuration-only and warning-only positives, optional floors validated
// even without a current hydration, and the malformed/foreign/incomparable
// errors. Every case also proves the call mutates neither current nor next.
func TestHydrationIndependentClocks(t *testing.T) {
	corpus := loadProjectionCorpus(t)
	testassert.Require(t, len(corpus.Accept) != 0, "the shared projection corpus has no accept cases")
	for _, testCase := range corpus.Accept {
		t.Run(testCase.Name, func(t *testing.T) {
			next := decodeRevision[protocol.Hydration](t, testCase.Next)
			var current *protocol.Hydration
			if testCase.Current != nil {
				decoded := decodeRevision[protocol.Hydration](t, *testCase.Current)
				current = &decoded
			}
			var configuration *protocol.ConfigurationRevision
			if testCase.Configuration != nil {
				decoded := decodeRevision[protocol.ConfigurationRevision](t, *testCase.Configuration)
				configuration = &decoded
			}
			var warnings *protocol.WarningsRevision
			if testCase.Warnings != nil {
				decoded := decodeRevision[protocol.WarningsRevision](t, *testCase.Warnings)
				warnings = &decoded
			}
			beforeCurrent, beforeNext := mustJSON(t, current), mustJSON(t, next)
			accepted, err := clientview.AcceptHydration(current, next, testCase.Instance, configuration, warnings)
			if testCase.Error {
				testassert.Require(t, err != nil, "AcceptHydration(%s) = %t, nil; want an error", testCase.Name, accepted)
			} else {
				testassert.Require(t, err == nil, "AcceptHydration(%s): %v", testCase.Name, err)
				testassert.Require(t, accepted == testCase.Accept, "AcceptHydration(%s) = %t, want %t", testCase.Name, accepted, testCase.Accept)
			}
			{
				afterCurrent, afterNext := mustJSON(t, current), mustJSON(t, next)
				testassert.Require(t, afterCurrent == beforeCurrent && afterNext == beforeNext, "AcceptHydration(%s) mutated its inputs", testCase.Name)
			}
		})
	}
}

// TestItemID runs the shared corpus over the five generated conversation item
// variants plus the unknown-kind, missing, empty and non-string identities
// that must stay errors.
func TestItemID(t *testing.T) {
	corpus := loadProjectionCorpus(t)
	testassert.Require(t, len(corpus.ItemIDs) != 0, "the shared projection corpus has no item id cases")
	for _, testCase := range corpus.ItemIDs {
		t.Run(testCase.Name, func(t *testing.T) {
			var item protocol.ConversationItem
			{
				err := json.Unmarshal([]byte(testCase.Item), &item)
				testassert.Require(t, err == nil, "decoding item payload %s: %v", testCase.Item, err)
			}
			got, err := clientview.ItemID(item)
			if testCase.Error {
				testassert.Require(t, err != nil, "ItemID(%s) = %q, nil; want an error", testCase.Item, got)
				return
			}
			testassert.Require(t, err == nil, "ItemID(%s): %v", testCase.Item, err)
			testassert.Require(t, got == testCase.ID, "ItemID(%s) = %q, want %q", testCase.Item, got, testCase.ID)
		})
	}
}

// TestMergeConversation runs the shared corpus over ordered overlaps,
// containment in both directions, equal ranges, disjoint and empty inputs
// (newer values replacing shared identities) plus the duplicate and
// inconsistent/reversed overlaps that must stay invalid delivery. Every case
// also proves the result is a fresh slice and neither input changed.
func TestMergeConversation(t *testing.T) {
	corpus := loadProjectionCorpus(t)
	testassert.Require(t, len(corpus.Merge) != 0, "the shared projection corpus has no merge cases")
	for _, testCase := range corpus.Merge {
		t.Run(testCase.Name, func(t *testing.T) {
			older := decodeItems(t, testCase.Older)
			newer := decodeItems(t, testCase.Newer)
			beforeOlder, beforeNewer := mustJSON(t, older), mustJSON(t, newer)
			got, err := clientview.MergeConversation(older, newer)
			if testCase.Error {
				testassert.Require(t, err != nil, "MergeConversation(%s, %s) = %d items, nil; want an error", testCase.Older, testCase.Newer, len(got))
				return
			}
			testassert.Require(t, err == nil, "MergeConversation(%s): %v", testCase.Name, err)
			want := decodeItems(t, testCase.Result)
			if gotValues, wantValues := canonicalItems(t, got), canonicalItems(t, want); !reflect.DeepEqual(gotValues, wantValues) {
				t.Fatalf("MergeConversation(%s) = %s, want %s", testCase.Name, mustJSON(t, got), testCase.Result)
			}
			{
				afterOlder, afterNewer := mustJSON(t, older), mustJSON(t, newer)
				testassert.Require(t, afterOlder == beforeOlder && afterNewer == beforeNewer, "MergeConversation(%s) mutated its inputs", testCase.Name)
			}
			if len(got) > 0 {
				for _, input := range [][]protocol.ConversationItem{older, newer} {
					testassert.Require(t, len(input) <= 0 || &got[0] != &input[0], "MergeConversation(%s) returned a slice aliasing its input", testCase.Name)
				}
			}
		})
	}
}

// TestClientViewDecoderBoundary records the actual generated decoders'
// behavior on the corpus's two raw numeric deliveries: an unquoted JSON number
// does not become a generated string, it fails the generated decode. The Go
// client view never receives such a value; this is decode evidence at the real
// generated boundary, not schema validation.
func TestClientViewDecoderBoundary(t *testing.T) {
	corpus := loadProjectionCorpus(t)
	testassert.Require(t, len(corpus.TypeScriptOnly) != 0, "the shared projection corpus has no typescript_only cases")
	for _, testCase := range corpus.TypeScriptOnly {
		t.Run(testCase.Name, func(t *testing.T) {
			var err error
			switch testCase.Function {
			case "compare":
				var revision protocol.SessionRevision
				err = json.Unmarshal([]byte(testCase.A), &revision)
			case "accept":
				var hydration protocol.Hydration
				err = json.Unmarshal([]byte(testCase.Next), &hydration)
			default:
				t.Fatalf("typescript_only case %q names unknown function %q", testCase.Name, testCase.Function)
			}
			testassert.Require(t, err != nil, "generated decode of %s accepted a raw JSON number", testCase.Name)
			t.Logf("generated decode error for %s: %v", testCase.Name, err)
		})
	}
}

func decodeRevision[T any](t *testing.T, raw string) T {
	t.Helper()
	var value T
	{
		err := json.Unmarshal([]byte(raw), &value)
		testassert.Require(t, err == nil, "decoding revision payload %s: %v", raw, err)
	}
	return value
}

func decodeItems(t *testing.T, raw string) []protocol.ConversationItem {
	t.Helper()
	var items []protocol.ConversationItem
	{
		err := json.Unmarshal([]byte(raw), &items)
		testassert.Require(t, err == nil, "decoding item array payload %s: %v", raw, err)
	}
	return items
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	testassert.NoError(t, err)
	return string(data)
}

func canonicalItems(t *testing.T, items []protocol.ConversationItem) []any {
	t.Helper()
	out := make([]any, len(items))
	for i, item := range items {
		{
			err := json.Unmarshal([]byte(mustJSON(t, item)), &out[i])
			testassert.Require(t, err == nil, "decoding item %d for comparison: %v", i, err)
		}
	}
	return out
}
