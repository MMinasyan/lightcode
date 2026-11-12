package protocol_test

import (
	"testing"

	"github.com/Syntheresis/melta/protocol"
)

func TestDecodeValueExactRequestMembersButOpaquePayloads(t *testing.T) {
	var created protocol.CreateSessionRequest
	if err := protocol.DecodeValue([]byte(`{"workspace":"/ws"}`), &created); err != nil {
		t.Fatalf("exact request: %v", err)
	}
	if err := protocol.DecodeValue([]byte(`{"workspace":"/ws","Workspace":"/other"}`), &created); err == nil {
		t.Fatal("case-variant request member was accepted")
	}
	if err := protocol.DecodeValue([]byte(`{"Workspace":"/other"}`), &created); err == nil {
		t.Fatal("case-variant-only request member was accepted")
	}

	var invocation protocol.ExtensionInvocationRequest
	body := []byte(`{"extension_id":"fixture","capability_id":"fixture.service","contract_id":"fixture.service/v1","contract_version":"1","method":"raw","target":{"kind":"harness"},"arguments":{"Code":"guest","RESULT":9007199254740993},"request_id":"req"}`)
	if err := protocol.DecodeValue(body, &invocation); err != nil {
		t.Fatalf("opaque arguments: %v", err)
	}
	if invocation.Arguments == nil || string(*invocation.Arguments) != `{"Code":"guest","RESULT":9007199254740993}` {
		t.Fatalf("opaque arguments = %+v", invocation.Arguments)
	}
}
