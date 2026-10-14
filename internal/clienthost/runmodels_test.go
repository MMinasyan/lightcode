package clienthost_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/MMinasyan/lightcode/internal/clienthost"
	"github.com/MMinasyan/lightcode/internal/clientops"
	"github.com/MMinasyan/lightcode/protocol"
)

// modelsEndpoint exercises actual startup and generated decoding. After the
// first read it publishes a different body, which must never be sampled.
func modelsEndpoint(t *testing.T, status int, body []byte) clienthost.Options {
	t.Helper()
	instance := strings.Repeat("a", 32)
	credential := strings.Repeat("b", 64)
	var mu sync.Mutex
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		requests = append(requests, r.Method+" "+r.URL.RequestURI())
		if r.Header.Get("Authorization") != "Bearer "+credential {
			t.Error("request lacks discovery authentication")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/health":
			_ = json.NewEncoder(w).Encode(map[string]string{"instance_id": instance, "protocol_version": string(protocol.N1)})
		case "/v1/configuration":
			w.WriteHeader(status)
			_, _ = w.Write(body)
			body = []byte(`{"agents":[],"providers":[],"configuration_revision":{"instance_id":"` + instance + `","generation":"18446744073709551615"},"settings":{}}`)
		default:
			t.Errorf("unexpected utility request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		want := []string{"GET /v1/health", "GET /v1/configuration"}
		if !reflect.DeepEqual(requests, want) {
			t.Errorf("requests = %v, want exactly %v", requests, want)
		}
	})
	dir := t.TempDir()
	record, err := json.Marshal(map[string]string{"endpoint": server.URL, "instance_id": instance, "protocol_version": string(protocol.N1), "credential": credential})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "runtime.json"), record, 0o600); err != nil {
		t.Fatal(err)
	}
	return clienthost.Options{DataDir: dir, Executable: filepath.Join(dir, "must-not-launch")}
}

func modelsConfiguration(t *testing.T, view protocol.ConfigurationView) clienthost.Options {
	t.Helper()
	body, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	return modelsEndpoint(t, http.StatusOK, body)
}

func retainedModelsView() protocol.ConfigurationView {
	return protocol.ConfigurationView{
		ConfigurationRevision: protocol.ConfigurationRevision{InstanceId: strings.Repeat("a", 32), Generation: "9007199254740993"},
		Agents:                []protocol.Agent{{Name: "primary", Model: "modeltest/vis"}},
		Providers: []protocol.Provider{
			{Id: "modeltest", Connected: true, Models: []protocol.ModelView{
				{Id: "vis", ContextWindow: 8192, MaxOutputTokens: 1024, Source: "user"},
				{Id: "inc", Incomplete: true, Source: "user"},
				{Id: "hid", ContextWindow: 16384, Hidden: true, Source: "user"},
			}},
			{Id: "disc", Models: []protocol.ModelView{{Id: "m", ContextWindow: 8192}}},
			{Id: "allinc", Models: []protocol.ModelView{{Id: "m", Incomplete: true}}},
		},
	}
}

func TestRunModelsRetainedOutput(t *testing.T) {
	base := retainedModelsView()
	noPrimary := retainedModelsView()
	noPrimary.Agents = []protocol.Agent{{Name: "secondary", Model: "modeltest/vis"}}
	other := protocol.Provider{Id: "other", Connected: true, Models: []protocol.ModelView{{Id: "m", ContextWindow: 4096}}}
	noPrimary.Providers = append(noPrimary.Providers, other)
	hiddenOnly := protocol.ConfigurationView{Providers: []protocol.Provider{{Id: "hiddenonly", Connected: true, Models: []protocol.ModelView{
		{Id: "hi", Hidden: true, Incomplete: true}, {Id: "h", ContextWindow: 8192, Hidden: true},
	}}}}
	jsonGolden := "[{\"ref\":\"modeltest/vis\",\"provider\":\"modeltest\",\"providerName\":\"modeltest\",\"model\":\"vis\",\"displayName\":\"vis\",\"contextWindow\":8192,\"maxOutputTokens\":1024,\"hidden\":false,\"providerHidden\":false,\"incomplete\":false,\"default\":true,\"source\":\"user\"}]\n"
	for _, tc := range []struct {
		name        string
		view        protocol.ConfigurationView
		args        []string
		stdout      string
		diagnostics string
		usage       string
	}{
		{"default", base, nil, "modeltest/vis\t8192\t1024\tdefault\n", "", ""},
		{"all", base, []string{"--all"}, "modeltest/hid\t16384\t0\thidden\nmodeltest/inc\t0\t0\tincomplete\nmodeltest/vis\t8192\t1024\tdefault\n", "", ""},
		{"filter", noPrimary, []string{"modeltest"}, "modeltest/vis\t8192\t1024\t-\n", "", ""},
		{"unfiltered", noPrimary, nil, "modeltest/vis\t8192\t1024\t-\nother/m\t4096\t0\t-\n", "", ""},
		{"unknown provider", base, []string{"nosuch"}, "", "", `unknown provider "nosuch" (known: modeltest, disc, allinc)`},
		{"disconnected", base, []string{"disc"}, "", "provider \"disc\" is not connected - connect it in Settings first\n", ""},
		{"disconnected all", base, []string{"disc", "--all"}, "", "provider \"disc\" is not connected - connect it in Settings first\n", ""},
		{"all incomplete disconnected", base, []string{"allinc"}, "", "provider \"allinc\" is not connected - connect it in Settings first\n", ""},
		{"provider no visible", hiddenOnly, []string{"hiddenonly"}, "", "no visible models for \"hiddenonly\" - rerun with --all\n", ""},
		{"hidden incomplete markers", hiddenOnly, []string{"hiddenonly", "--all"}, "hiddenonly/h\t8192\t0\thidden\nhiddenonly/hi\t0\t0\thidden,incomplete\n", "", ""},
		{"global no visible", hiddenOnly, nil, "", "no visible models - rerun with --all\n", ""},
		{"global disconnected", protocol.ConfigurationView{}, nil, "", "no models available - connect a provider in Settings first\n", ""},
		{"empty json", protocol.ConfigurationView{}, []string{"--json"}, "[]\n", "no models available - connect a provider in Settings first\n", ""},
		{"json golden", base, []string{"--json"}, jsonGolden, "", ""},
		{"leading flags", base, []string{"-json", "--all=false", "modeltest"}, jsonGolden, "", ""},
		{"trailing flags", base, []string{"modeltest", "-json"}, jsonGolden, "", ""},
		{"end flags retained", base, []string{"--", "modeltest", "--json"}, jsonGolden, "", ""},
		{"trailing unknown is provider", base, []string{"--", "--unknown"}, "", "", `unknown provider "--unknown" (known: modeltest, disc, allinc)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := modelsConfiguration(t, tc.view)
			var out, diagnostics bytes.Buffer
			err := clienthost.RunModels(context.Background(), opts, tc.args, &out, &diagnostics)
			if tc.usage != "" {
				var usage *clienthost.UsageError
				if !errors.As(err, &usage) || usage.ExitCode() != 2 || usage.Error() != tc.usage {
					t.Fatalf("error = %v, want usage %q", err, tc.usage)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if out.String() != tc.stdout || diagnostics.String() != tc.diagnostics {
				t.Fatalf("stdout=%q diagnostics=%q, want stdout=%q diagnostics=%q", out.String(), diagnostics.String(), tc.stdout, tc.diagnostics)
			}
		})
	}
}

func TestRunModelsConfigurationFields(t *testing.T) {
	zero, input, output, cacheWrite := 0.0, 1.25, 2.5, 4.75
	view := protocol.ConfigurationView{
		Agents: []protocol.Agent{{Name: "primary", Model: "hidden/org/m"}},
		Providers: []protocol.Provider{
			{Id: "hidden", Name: "Hidden Provider", Connected: true, Hidden: true, Models: []protocol.ModelView{
				{Id: "org/m", Name: "Named Model", ContextWindow: 9007199254740993, MaxOutputTokens: 0, Source: "discovery",
					Cost: &protocol.Cost{Input: &input, Output: &output, CacheRead: &zero, CacheWrite: &cacheWrite}},
			}},
			{Id: "fallback", Connected: true, Models: []protocol.ModelView{{Id: "z/slash", ContextWindow: 100, Source: "builtin", Cost: &protocol.Cost{Input: &zero}}}},
		},
	}
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"hidden provider filtered", nil, "fallback/z/slash\t100\t0\t-\n"},
		{"all markers", []string{"--all"}, "fallback/z/slash\t100\t0\t-\nhidden/org/m\t9007199254740993\t0\tdefault,hidden\n"},
		{"json costs names and flags", []string{"--all", "--json"}, `[{"ref":"fallback/z/slash","provider":"fallback","providerName":"fallback","model":"z/slash","displayName":"z/slash","contextWindow":100,"maxOutputTokens":0,"cost":{"input":0},"hidden":false,"providerHidden":false,"incomplete":false,"default":false,"source":"builtin"},{"ref":"hidden/org/m","provider":"hidden","providerName":"Hidden Provider","model":"org/m","displayName":"Named Model","contextWindow":9007199254740993,"maxOutputTokens":0,"cost":{"cache_read":0,"cache_write":4.75,"input":1.25,"output":2.5},"hidden":true,"providerHidden":true,"incomplete":false,"default":true,"source":"discovery"}]` + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, diagnostics bytes.Buffer
			if err := clienthost.RunModels(context.Background(), modelsConfiguration(t, view), tc.args, &out, &diagnostics); err != nil {
				t.Fatal(err)
			}
			if out.String() != tc.want || diagnostics.Len() != 0 {
				t.Fatalf("stdout=%q diagnostics=%q, want %q", out.String(), diagnostics.String(), tc.want)
			}
		})
	}
	view.Providers[0].Models[0].Incomplete = true
	var allOut bytes.Buffer
	if err := clienthost.RunModels(context.Background(), modelsConfiguration(t, view), []string{"hidden", "--all"}, &allOut, io.Discard); err != nil {
		t.Fatal(err)
	}
	if allOut.String() != "hidden/org/m\t9007199254740993\t0\tdefault,hidden,incomplete\n" {
		t.Fatalf("combined markers = %q", allOut.String())
	}
	for _, agents := range [][]protocol.Agent{nil, {{Name: "primary"}}, {{Name: "secondary", Model: "fallback/z/slash"}}} {
		view.Agents = agents
		var out bytes.Buffer
		if err := clienthost.RunModels(context.Background(), modelsConfiguration(t, view), nil, &out, io.Discard); err != nil {
			t.Fatal(err)
		}
		if out.String() != "fallback/z/slash\t100\t0\t-\n" {
			t.Fatalf("unset primary output = %q", out.String())
		}
	}
}

// TestRunModelsOpaqueOverflowFieldsRenders proves unused opaque extra_body
// and options numbers beyond Number precision, delivered as their exact raw
// lexemes through the shared invocation, do not disturb the utility's
// retained output.
func TestRunModelsOpaqueOverflowFieldsRenders(t *testing.T) {
	// Raw JSON supplies the overflow lexemes exactly; a generated struct
	// cannot carry them.
	raw := `{"configuration_revision":{"instance_id":"` + strings.Repeat("a", 32) + `","generation":"1"},"settings":{},"agents":[{"name":"primary","model":"prov/m"}],"providers":[{"id":"prov","connected":true,"extra_body":{"n":9007199254740993},"options":{"big":1e400},"models":[{"id":"m","name":"M","context_window":4096,"source":"user","extra_body":{"n":9007199254740993},"options":{"big":1e400}}]}]}`
	opts := modelsEndpoint(t, http.StatusOK, []byte(raw))
	var out, diagnostics bytes.Buffer
	if err := clienthost.RunModels(context.Background(), opts, nil, &out, &diagnostics); err != nil {
		t.Fatalf("RunModels: %v", err)
	}
	if out.String() != "prov/m\t4096\t0\tdefault\n" {
		t.Fatalf("stdout=%q diagnostics=%q, want the retained row despite opaque overflow fields", out.String(), diagnostics.String())
	}
	var jsonOut bytes.Buffer
	if err := clienthost.RunModels(context.Background(), modelsEndpoint(t, http.StatusOK, []byte(raw)), []string{"--json"}, &jsonOut, io.Discard); err != nil {
		t.Fatalf("RunModels --json: %v", err)
	}
	if jsonOut.String() != `[{"ref":"prov/m","provider":"prov","providerName":"prov","model":"m","displayName":"M","contextWindow":4096,"maxOutputTokens":0,"hidden":false,"providerHidden":false,"incomplete":false,"default":true,"source":"user"}]`+"\n" {
		t.Fatalf("json stdout=%q", jsonOut.String())
	}
}

func TestRunModelsUsageBeforeStartup(t *testing.T) {
	// A missing executable would be an ordinary startup error if parsing connected.
	opts := clienthost.Options{DataDir: t.TempDir(), Executable: "/does-not-exist"}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"p1", "p2"}, `unexpected argument "p2"`},
		{[]string{"--unknown"}, "flag provided but not defined: -unknown"},
		{[]string{"--all=invalid"}, "invalid boolean value"},
		{[]string{"p1", "--all=false"}, `unexpected argument "--all=false"`},
	} {
		var out, diagnostics bytes.Buffer
		err := clienthost.RunModels(context.Background(), opts, tc.args, &out, &diagnostics)
		var usage *clienthost.UsageError
		if !errors.As(err, &usage) || usage.ExitCode() != 2 || !strings.Contains(usage.Error(), tc.want) || out.Len() != 0 || diagnostics.Len() != 0 {
			t.Fatalf("args %v: error=%v stdout=%q diagnostics=%q", tc.args, err, out.String(), diagnostics.String())
		}
	}
	entries, err := os.ReadDir(opts.DataDir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("usage changed startup directory: %v, %v", entries, err)
	}
}

func TestRunModelsFailures(t *testing.T) {
	t.Run("configuration error", func(t *testing.T) {
		var out, diagnostics bytes.Buffer
		err := clienthost.RunModels(context.Background(), modelsEndpoint(t, http.StatusNotFound, []byte(`{"code":"not_found","message":"no configuration"}`)), nil, &out, &diagnostics)
		var response *clientops.ResponseError
		if !errors.As(err, &response) || response.Status != http.StatusNotFound || err.Error() != "no configuration" || out.Len() != 0 || diagnostics.Len() != 0 {
			t.Fatalf("error=%v; stdout=%q diagnostics=%q", err, out.String(), diagnostics.String())
		}
	})
	var malformedOut bytes.Buffer
	if err := clienthost.RunModels(context.Background(), modelsEndpoint(t, http.StatusOK, []byte(`{"providers":`)), nil, &malformedOut, io.Discard); err == nil || malformedOut.Len() != 0 {
		t.Fatalf("malformed success: error=%v output=%q", err, malformedOut.String())
	}
	if err := clienthost.RunModels(context.Background(), modelsEndpoint(t, http.StatusNoContent, nil), nil, io.Discard, io.Discard); err == nil {
		t.Fatal("missing configuration body reported success")
	}
	var out bytes.Buffer
	err := clienthost.RunModels(context.Background(), clienthost.Options{DataDir: t.TempDir(), Executable: "/does-not-exist"}, nil, &out, io.Discard)
	var usage *clienthost.UsageError
	if err == nil || errors.As(err, &usage) || !strings.Contains(err.Error(), "start runtime contender") || out.Len() != 0 {
		t.Fatalf("startup error=%v output=%q", err, out.String())
	}
}
