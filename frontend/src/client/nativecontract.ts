// The test-only native invocation contract assertion. It reads the recorded
// native Call arguments — the same {generation, operation, params} tuples the
// scripted host received — and checks each one against the schema-derived
// native invocation metadata. It is an assertion helper, not a runtime
// validator: the shipped client never imports it, it never rewrites or
// re-serializes the dispatched text, and enums and identities stay with the
// Runtime's own semantic validation.
import metadata from '../generated/native-invocations.json';

type Mode = 'forbidden' | 'optional' | 'required';
type QueryPrimitive = 'string' | 'boolean';

interface QueryMember {
  required: boolean;
  type: QueryPrimitive;
}

interface OperationContract {
  session_id: Mode;
  field: Mode;
  query: Mode;
  body: Mode;
  query_members: Record<string, QueryMember>;
}

const contracts = metadata as Record<string, OperationContract>;
const ENVELOPE_MEMBERS = ['session_id', 'field', 'query', 'body'] as const;

// own reports one object's own-property membership: an inherited name such as
// toString, constructor or __proto__ can never impersonate a declared
// operation or satisfy a required member.
function own(target: object, key: string): boolean {
  return Object.prototype.hasOwnProperty.call(target, key);
}

function isPlainObject(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

// assertNativeInvocations checks every recorded mounted native invocation
// against the schema-derived contract. A violation throws with the offending
// operation and member, so the owning test's teardown fails loudly instead of
// hiding behind an expected application outcome.
export function assertNativeInvocations(calls: ReadonlyArray<readonly unknown[]>): void {
  for (const call of calls) {
    const operation = call[1];
    if (typeof operation !== 'string' || !own(contracts, operation)) {
      throw new Error(`native invocation ${String(operation)} is not a declared native operation`);
    }
    const contract = contracts[operation];
    const envelope = envelopeOf(operation, call[2]);
    for (const name of Object.keys(envelope)) {
      if (!(ENVELOPE_MEMBERS as readonly string[]).includes(name)) {
        throw new Error(`native invocation ${operation}: envelope member ${name} is not declared`);
      }
      if (contract[name as (typeof ENVELOPE_MEMBERS)[number]] === 'forbidden') {
        throw new Error(`native invocation ${operation}: envelope member ${name} is forbidden for this operation`);
      }
    }
    for (const name of ENVELOPE_MEMBERS) {
      if (!own(envelope, name)) {
        if (contract[name] === 'required') {
          throw new Error(`native invocation ${operation}: envelope member ${name} is required`);
        }
        continue;
      }
      const value = envelope[name];
      if (value === null) {
        throw new Error(`native invocation ${operation}: envelope member ${name} must not be null`);
      }
      if (name === 'session_id' || name === 'field') {
        if (typeof value !== 'string') {
          throw new Error(`native invocation ${operation}: envelope member ${name} must be a string`);
        }
        continue;
      }
      if (name === 'query') {
        checkQuery(operation, contract, value);
        continue;
      }
      if (!isPlainObject(value)) {
        throw new Error(`native invocation ${operation}: envelope member body must be one JSON object`);
      }
    }
  }
}

// envelopeOf decodes one recorded params text for inspection only; the text
// the host received stays untouched. The empty text is the empty envelope,
// exactly like the Go binder's missing-params case.
function envelopeOf(operation: string, params: unknown): Record<string, unknown> {
  if (typeof params !== 'string') {
    throw new Error(`native invocation ${operation}: params text is not a string`);
  }
  if (params === '') {
    return {};
  }
  let parsed: unknown;
  try {
    parsed = JSON.parse(params);
  } catch {
    throw new Error(`native invocation ${operation}: params text is not JSON`);
  }
  if (!isPlainObject(parsed)) {
    throw new Error(`native invocation ${operation}: envelope must be one JSON object`);
  }
  return parsed;
}

// checkQuery checks one declared query object: every provided member must be
// declared with the matching primitive type and every declared required
// member must be present.
function checkQuery(operation: string, contract: OperationContract, value: unknown): void {
  if (!isPlainObject(value)) {
    throw new Error(`native invocation ${operation}: envelope member query must be one JSON object`);
  }
  const query = value;
  for (const name of Object.keys(query)) {
    if (!own(contract.query_members, name)) {
      throw new Error(`native invocation ${operation}: query member ${name} is not declared`);
    }
    const member = contract.query_members[name];
    if (typeof query[name] !== member.type) {
      throw new Error(`native invocation ${operation}: query member ${name} must be a ${member.type}`);
    }
  }
  for (const name of Object.keys(contract.query_members)) {
    if (contract.query_members[name].required && !own(query, name)) {
      throw new Error(`native invocation ${operation}: query member ${name} is required`);
    }
  }
}
