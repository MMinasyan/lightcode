// Shared nominal values for the frontend's mounted and reader tests: one
// Session/Hydration shape with fresh defaults per call, and the two lossless
// response forms the generated bindings return. Callers spread their own
// overrides shallowly, so deliberate aliases of a supplied object stay.
import type { Hydration, Session, SessionRevision } from '../generated/protocol/types.gen';
import type { desktopclient } from '../../client-bindings/wailsjs/go/models';

export const INSTANCE = '0123456789abcdef0123456789abcdef';
// Descending last_activity then Session ID orders RFC3339 strings correctly;
// the fixtures use comparable timestamps.
export const NEWER = '2026-10-02T10:00:00Z';
export const OLDER = '2026-10-01T05:00:00Z';

export function revision(durable: string, local = '0', instance = INSTANCE): SessionRevision {
  return { instance_id: instance, durable_revision: durable, local_revision: local };
}

export function session(id: string, lastActivity: string, parent?: string): Session {
  return {
    session_id: id,
    workspace: '/ws',
    ...(parent ? { parent_session_id: parent } : {}),
    lifecycle: 'open',
    agent_type: 'primary',
    created_at: '0',
    last_activity: lastActivity,
    session_revision: revision('1'),
  };
}

export function hydration(overrides: Partial<Hydration> = {}): Hydration {
  return {
    session: session('root1', NEWER),
    selected_model: null,
    session_revision: revision('1'),
    configuration_revision: { instance_id: INSTANCE, generation: '1' },
    warnings_revision: { instance_id: INSTANCE, revision: '1' },
    operations: [],
    pending: { steering: [], queued: [] },
    conversation: { items: [] },
    usage: { totals: { by_model: [] }, context: { used_tokens: '0', context_window: 0 } },
    warnings: [],
    background: [],
    ...overrides,
  };
}

// native supplies one scripted local result exactly as the native owner
// returns it, including the explicit invalid-delivery bit; producer validation
// stays at the Go response owner.
export function native(status: number, body: string, invalid: boolean): desktopclient.HTTPResponse {
  return { status, body, invalid };
}

// json stringifies once and delegates: the body bytes are the producer's.
export function json(status: number, value: unknown): desktopclient.HTTPResponse {
  return native(status, JSON.stringify(value), false);
}
