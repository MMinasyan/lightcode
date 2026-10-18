// The native invocation contract assertion's own rows: the allowed envelopes
// the mounted clients compose, and the nearest forbidden siblings — unknown or
// prototype-named operations, forbidden/missing/null/type members, undeclared
// query members and non-object query/body values — each rejected.
import { describe, expect, it } from 'vitest';

import { assertNativeInvocations } from './nativecontract';

function call(operation: string, params: string): readonly unknown[] {
  return ['1', operation, params];
}

function allows(operation: string, params: string): void {
  expect(() => assertNativeInvocations([call(operation, params)])).not.toThrow();
}

function rejects(operation: string, params: string): void {
  expect(() => assertNativeInvocations([call(operation, params)])).toThrow();
}

describe('native invocation contract', () => {
  it('accepts the envelopes the mounted clients actually compose', () => {
    allows('GetHealth', '');
    allows('GetHealth', '{}');
    allows('GetConfiguration', '{}');
    allows('ListSessions', '{"query":{"workspace":"/ws","lifecycle":"open"}}');
    allows('ListModels', '{"query":{"all":false}}');
    allows('GetSessionHydration', '{"session_id":"root1"}');
    allows('GetSessionHistory', '{"session_id":"root1"}');
    allows('GetSessionHistory', '{"session_id":"root1","query":{}}');
    allows('GetSessionHistory', '{"session_id":"root1","query":{"cursor":"abc"}}');
    allows('SubmitSession', '{"session_id":"root1","body":{"operation_id":"op1"}}');
    allows('CreateSession', '{"body":{"workspace":"/ws","agent_type":"primary"}}');
    allows('ResetProviderField', '{"field":"name","query":{"provider_id":"prov1"}}');
    allows('ResetProviderModelField', '{"field":"hidden","query":{"provider_id":"prov1","model_id":"m1"}}');
    allows('UpdateProviderModel', '{"query":{"provider_id":"prov1","model_id":"m1"},"body":{"name":"M"}}');
    // Opaque bodies stay opaque: exact lexemes and unknown members inside a
    // body are never re-interpreted here.
    allows('UpdateProviderDetail', '{"query":{"provider_id":"prov1"},"body":{"extra_body":{"n":9007199254740993}}}');
  });

  it('rejects an operation the contract does not declare, including prototype names', () => {
    rejects('Nope', '{}');
    rejects('GetEvents', '{}');
    rejects('', '{}');
    rejects('toString', '{}');
    rejects('constructor', '{}');
    rejects('__proto__', '{}');
    rejects('hasOwnProperty', '{}');
  });

  it('rejects forbidden, undeclared, missing, null and mistyped envelope members', () => {
    rejects('GetHealth', '{"session_id":"root1"}');
    rejects('GetHealth', '{"bogus":1}');
    rejects('GetHealth', '{"toString":"root1"}');
    rejects('DeleteSession', '{}');
    rejects('DeleteSession', '{"session_id":null}');
    rejects('DeleteSession', '{"session_id":5}');
    rejects('DeleteSession', '{"session_id":{}}');
    rejects('ResetProviderField', '{"query":{"provider_id":"prov1"}}');
    rejects('ResetProviderField', '{"field":5,"query":{"provider_id":"prov1"}}');
    rejects('ResetProviderField', '{"field":"name"}');
    rejects('CreateSession', '{}');
    rejects('CreateSession', '{"body":null}');
    rejects('CreateSession', '{"body":[]}');
    rejects('CreateSession', '{"body":"x"}');
    rejects('CreateSession', '{"body":5}');
    rejects('SubmitSession', '{}');
    rejects('SubmitSession', '{"body":{"operation_id":"op1"}}');
  });

  it('rejects malformed params text and non-object envelopes', () => {
    rejects('GetHealth', '{');
    rejects('GetHealth', '[]');
    rejects('GetHealth', '"x"');
    rejects('GetHealth', 'null');
    rejects('GetHealth', '5');
    rejects('GetHealth', '{} {}');
  });

  it('rejects undeclared, missing and mistyped query members', () => {
    rejects('ListSessions', '{}');
    rejects('ListSessions', '{"query":null}');
    rejects('ListSessions', '{"query":[]}');
    rejects('ListSessions', '{"query":5}');
    rejects('ListSessions', '{"query":{"workspace":"/ws"}}');
    rejects('ListSessions', '{"query":{"workspace":null,"lifecycle":"open"}}');
    rejects('ListSessions', '{"query":{"workspace":5,"lifecycle":"open"}}');
    rejects('ListSessions', '{"query":{"workspace":"/ws","lifecycle":"open","bogus":1}}');
    rejects('ListSessions', '{"query":{"workspace":"/ws","lifecycle":"open","toString":"x"}}');
    rejects('ListModels', '{"query":{"all":"yes"}}');
    rejects('ListModels', '{"query":{"all":null}}');
    rejects('GetSessionHistory', '{"session_id":"root1","query":null}');
    rejects('GetSessionHistory', '{"session_id":"root1","query":[]}');
    rejects('GetSessionHistory', '{"session_id":"root1","query":{"cursor":null}}');
    rejects('GetSessionHistory', '{"session_id":"root1","query":{"cursor":5}}');
    rejects('GetSessionHistory', '{"session_id":"root1","query":{"bogus":1}}');
    // The envelope's session_id belongs only to the operations that declare a
    // session path; the retained query's own session_id is a query member.
    rejects('GetRetainedCodeSnapshots', '{"session_id":"root1","query":{"workspace":"/ws"}}');
    rejects('GetRetainedCodeSnapshots', '{"query":{"workspace":"/ws"}}');
    allows('GetRetainedCodeSnapshots', '{"query":{"workspace":"/ws","session_id":"0123456789abcdef0123456789abcdef"}}');
  });

  it('checks every recorded call, so one violating envelope fails the whole history', () => {
    expect(() =>
      assertNativeInvocations([
        call('GetHealth', '{}'),
        call('SubmitSession', '{"body":{"operation_id":"op1"}}'),
      ]),
    ).toThrow(/session_id is required/);
  });
});
