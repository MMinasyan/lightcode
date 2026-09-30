// Compile-only consumer fixture over the generated protocol SDK.
// It is type-checked by tsconfig.protocol.json together with the generated
// tree and never executed: it proves the generated output exports a usable
// operation and type surface for a real client, without Wails types and
// without handwritten duplicate DTOs.

import {
  deleteProviderDetail,
  deleteProviderModel,
  getEvents,
  getHealth,
  submitSession,
  type ConfigurationRevision,
  type ContentPart,
  type DeletionMutation,
  type Event,
  type ModelUsage,
  type ScopeEvent,
  type SystemRole,
} from './src/generated/protocol';
import { createClient } from './src/generated/protocol/client';

const revision: ConfigurationRevision = {
  instance_id: '0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f',
  generation: '3',
};

// Configuration deletion keeps the uniform mutation envelope: the owning
// revision plus a required, explicitly null-only result.
const deletion: DeletionMutation = {
  configuration_revision: revision,
  result: null,
};
const checkDeletion: DeletionMutation = deletion;

// @ts-expect-error deletion responses require the result member
const missingResult: DeletionMutation = { configuration_revision: revision };

const nonNullResult: DeletionMutation = {
  configuration_revision: revision,
  // @ts-expect-error the deletion result is null only
  result: { removed: true },
};

// One shared model-reference spelling: the "provider/model" string, parsed
// at the first slash so slash-containing model suffixes stay unambiguous.
const usage: ModelUsage = {
  model: 'openrouter/team/model',
  usage: { input_tokens: '12', cached_input_tokens: '0', output_tokens: '34' },
};
const checkUsage: ModelUsage = usage;

const splitUsage: ModelUsage = {
  // @ts-expect-error ModelUsage carries one model reference, not a separate provider member
  provider: 'openrouter',
  model: 'openrouter/team/model',
  usage: { input_tokens: '12', cached_input_tokens: '0', output_tokens: '34' },
};

const textPart: ContentPart = { kind: 'text', text: 'hello' };
const imagePart: ContentPart = { kind: 'image_url', url: 'https://example.invalid/x.png' };
const checkParts: ContentPart[] = [textPart, imagePart];

const finished: Event = {
  kind: 'tool_finished',
  scope: { kind: 'session', session_id: '0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f' },
  call_id: 'call-1',
  status: 'denied',
};
const checkEvent: Event = finished;

// One shared scope-event payload carries both lifecycle kinds; the Event
// union keeps accepting either discriminator literal.
const scopeOpened: ScopeEvent = { kind: 'scope_opened', scope: { kind: 'runtime' } };
const scopeClosed: ScopeEvent = {
  kind: 'scope_closed',
  scope: { kind: 'job', job_id: 'job-1' },
};
const checkScopeOpened: ScopeEvent = scopeOpened;
const checkScopeClosed: Event = scopeClosed;

// @ts-expect-error scope kinds are closed
const unknownScopeKind: ScopeEvent = { kind: 'scope_unknown', scope: { kind: 'runtime' } };

const scopeWithExtraField: ScopeEvent = {
  kind: 'scope_opened',
  scope: { kind: 'runtime' },
  // @ts-expect-error scope events reject unrelated fields
  extra: 1,
};

const role: SystemRole = 'developer';
const checkRole: SystemRole = role;

// The exported operation surface is referenced without executing any
// network request. The client is constructed with the one global bearer
// credential declared by the schema.
const client = createClient({
  baseUrl: 'http://127.0.0.1:0',
  auth: '0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f',
});

const operations = {
  checkDeletion,
  checkEvent,
  checkParts,
  checkRole,
  checkScopeClosed,
  checkScopeOpened,
  checkUsage,
  client,
  deleteProviderDetail,
  deleteProviderModel,
  getEvents,
  getHealth,
  submitSession,
};
void operations;
