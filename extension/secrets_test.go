package extension

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSecretNameResolvesJSONPointerValues(t *testing.T) {
	section := json.RawMessage(`{"api":{"key":"FIRST_KEY"},"keys":["ZERO_KEY","SECOND_KEY"],"slash/key":"SLASH_KEY","tilde~key":"TILDE_KEY","tilde~2key":"TILDE_TWO_KEY","trailing~":"TRAILING_KEY","numeric":{"01":"ZERO_ONE_KEY","+1":"PLUS_KEY","-1":"MINUS_KEY","1":"ONE_KEY"},"empty":"","number":5}`)
	for _, row := range []struct {
		name   string
		field  string
		want   string
		wantOK bool
	}{
		{"nested object", "/api/key", "FIRST_KEY", true},
		{"canonical array index", "/keys/1", "SECOND_KEY", true},
		{"zero array index", "/keys/0", "ZERO_KEY", true},
		{"leading-zero index", "/keys/01", "", false},
		{"signed index", "/keys/+1", "", false},
		{"negative index", "/keys/-1", "", false},
		{"dash index", "/keys/-", "", false},
		{"space index", "/keys/ 1", "", false},
		{"fractional index", "/keys/1.0", "", false},
		{"out-of-range index", "/keys/2", "", false},
		{"numeric object key stays exact", "/numeric/01", "ZERO_ONE_KEY", true},
		{"signed object key stays exact", "/numeric/+1", "PLUS_KEY", true},
		{"negative object key stays exact", "/numeric/-1", "MINUS_KEY", true},
		{"numeric object key one", "/numeric/1", "ONE_KEY", true},
		{"escaped slash", "/slash~1key", "SLASH_KEY", true},
		{"escaped tilde", "/tilde~0key", "TILDE_KEY", true},
		{"escaped tilde then literal two", "/tilde~02key", "TILDE_TWO_KEY", true},
		{"invalid tilde escape", "/tilde~2key", "", false},
		{"trailing tilde escape", "/trailing~", "", false},
		{"escaped trailing tilde", "/trailing~0", "TRAILING_KEY", true},
		{"missing member", "/api/other", "", false},
		{"non-string value", "/number", "", false},
		{"empty value", "/empty", "", false},
		{"traversal through a string", "/api/key/deeper", "", false},
		{"empty pointer", "", "", false},
		{"malformed section", "/api", "", false},
	} {
		t.Run(row.name, func(t *testing.T) {
			input := section
			if row.name == "malformed section" {
				input = json.RawMessage(`{`)
			}
			got, ok := SecretName(input, row.field)
			if got != row.want || ok != row.wantOK {
				t.Fatalf("SecretName(%q) = (%q, %v), want (%q, %v)", row.field, got, ok, row.want, row.wantOK)
			}
		})
	}
}

func TestSecretRefPointerGrammarAtCompilation(t *testing.T) {
	valid := testNative("valid", LifetimeHarness, nil, nil, nil)
	valid.Declaration.Secrets = []SecretRef{
		{Field: "/unresolved", Lifetime: SecretActivation},
		{Field: "/array/0", Lifetime: SecretOperation},
		{Field: "/object/-1", Lifetime: SecretActivation},
	}
	if _, err := Compile([]Native{valid}); err != nil {
		t.Fatalf("Compile valid unresolved SecretRef fields: %v", err)
	}
	for _, field := range []string{"/bad~2", "/bad~", "no-slash", ""} {
		native := testNative("malformed", LifetimeHarness, nil, nil, nil)
		native.Declaration.Secrets = []SecretRef{{Field: field, Lifetime: SecretActivation}}
		if _, err := Compile([]Native{native}); err == nil || !strings.Contains(err.Error(), "not a JSON pointer") {
			t.Fatalf("Compile SecretRef field %q = %v, want the pointer grammar refusal", field, err)
		}
	}
}

func TestCredentialsHostContractBinding(t *testing.T) {
	host, ok := HostCapability(ContractCredentials)
	if !ok || host.ContractID != ContractCredentials || host.ContractVersion != ContractVersion || host.Kind != KindService {
		t.Fatalf("host credentials capability = (%+v, %v), want the fixed service contract", host, ok)
	}
	if len(host.Methods) != 1 || host.Methods[0].Name != "get" || host.Methods[0].Mode != ModeServiceIO {
		t.Fatalf("host credentials methods = %+v, want the one service_io get method", host.Methods)
	}
	bound := testNative("bound", LifetimeHarness, []Capability{ServiceCapability("caller", "test.caller", PureMethod("call", `{}`, `{}`))}, []Binding{{Name: "credentials", Provider: HostProvider, CapabilityID: ContractCredentials, Definition: host}}, nil)
	compiled, err := Compile([]Native{bound})
	if err != nil {
		t.Fatalf("Compile with the credentials host binding: %v", err)
	}
	if binding := compiled.Components(LifetimeHarness)[0].Requires[0]; !binding.Host {
		t.Fatalf("credentials binding = %+v, want the host provider", binding)
	}

	mismatch := testNative("bound", LifetimeHarness, []Capability{ServiceCapability("caller", "test.caller", PureMethod("call", `{}`, `{}`))}, []Binding{{Name: "credentials", Provider: HostProvider, CapabilityID: ContractCredentials, Definition: CapabilityDefinition{ContractID: ContractCredentials, ContractVersion: ContractVersion, Kind: KindService, Methods: []Method{PureMethod("get", `{}`, `{}`)}}}}, nil)
	if _, err := Compile([]Native{mismatch}); err == nil || !strings.Contains(err.Error(), "expected host contract differs") {
		t.Fatalf("Compile with a forged credentials definition = %v, want the host contract mismatch", err)
	}
}
