// Mounted generated-SDK integration over the real isolated protocol owner.
// This file exercises the generated fetch SDK — never handwritten requests —
// against a real loopback listener: the endpoint and bearer credential are
// read from the temporary mode-0600 discovery record whose path the Go
// protocol test helper exports in LIGHTCODE_PROTOCOL_DISCOVERY. Without that
// environment the row is skipped, so the ordinary frontend suite stays
// self-contained.
import { readFileSync } from 'node:fs';
import { expect, test } from 'vitest';

import {
  connectProvider,
  createProvider,
  createSession,
  deleteProviderModel,
  getConfiguration,
  getHealth,
  getProviderDetail,
  getSessionHistory,
  getSessionHydration,
  getWarnings,
  interruptSession,
  listProviderModels,
  listSessions,
  resetProviderField,
  resetProviderModelField,
  setAgentTypeModel,
  stopSession,
  updateConfigurationSettings,
  updateProviderDetail,
  updateProviderModel,
} from './src/generated/protocol';
import { createClient } from './src/generated/protocol/client';

const discoveryPath = process.env.LIGHTCODE_PROTOCOL_DISCOVERY;

const mounted = test.skipIf(!discoveryPath);

mounted('generated SDK round-trips special identity query values over the mounted owner', async () => {
  const discovery = JSON.parse(readFileSync(discoveryPath, 'utf8'));
  // The Go helper in this same mounted run already proved the record's
  // 0600 mode; these are the record-shape checks it does not duplicate.
  expect(discovery.endpoint).toMatch(/^http:\/\/127\.0\.0\.1:\d+$/);
  expect(discovery.instance_id).toMatch(/^[0-9a-f]{32}$/);
  expect(discovery.protocol_version).toBe('1');
  // The credential is validated without an assertion diff: a mismatch must
  // never print the value this check exists to protect.
  if (typeof discovery.credential !== 'string' || !/^[0-9a-f]{64}$/.test(discovery.credential)) {
    throw new Error('discovery record credential is not 64 lowercase hex');
  }

  const client = createClient({ baseUrl: discovery.endpoint, auth: discovery.credential });
  const health = await getHealth({ client });
  expect(health.error).toBeUndefined();
  expect(health.data).toEqual({ instance_id: discovery.instance_id, protocol_version: '1' });

  // The open warning source crosses the generated TypeScript boundary: the
  // mounted owner's real Runtime-scoped reporter plugin attributes its report
  // to its own registered ID, and the generated record accepts the arbitrary
  // plugin:<ID> string.
  const warnings = await getWarnings({ client });
  expect(warnings.error).toBeUndefined();
  const reporter = warnings.data.warnings.find((warning) => warning.source === 'plugin:mounted_reporter');
  expect(reporter).toEqual({
    source: 'plugin:mounted_reporter',
    kind: 'mounted_notice',
    message: 'mounted plugin warning',
  });

  // The settings round-trip carries each compiled plugin's own document as
  // one opaque string: the int64 lexeme never passes through a JS number,
  // the real tools validator accepts the document, and the read returns the
  // same values. Whitespace is normalized because the owning writer keeps
  // its existing pretty-printed raw-root bytes — value fidelity, not a new
  // byte-formatting contract.
  const canonical = (text) => text.replace(/\s+/g, '');
  const toolsDocument =
    '{"command_timeout":60,"max_output_bytes":4096,"read_line_max_chars":3000,"read_max_lines":100,"opaque":9007199254740993}';
  const initial = await getConfiguration({ client });
  expect(initial.error).toBeUndefined();
  const written = await updateConfigurationSettings({
    client,
    body: { settings: { sessions: initial.data.settings.sessions, plugins: { tools: toolsDocument } } },
  });
  expect(written.error).toBeUndefined();
  const writtenDocument = written.data?.result.plugins.tools;
  expect(writtenDocument).toBeTypeOf('string');
  expect(canonical(writtenDocument)).toBe(canonical(toolsDocument));
  const reread = await getConfiguration({ client });
  expect(reread.error).toBeUndefined();
  const rereadDocument = reread.data?.settings.plugins.tools;
  expect(rereadDocument).toBeTypeOf('string');
  expect(rereadDocument).toContain('9007199254740993');
  expect(canonical(rereadDocument)).toBe(canonical(toolsDocument));

  const window = 4096;
  for (const id of ['.', '..', '?', '#', '%']) {
    const created = await createProvider({
      client,
      body: { id, provider: { base_url: 'https://identity.test/v1' }, models: { m: { context_window: window } } },
    });
    expect(created.error).toBeUndefined();
    expect(created.data?.result.id).toBe(id);

    const detail = await getProviderDetail({ client, query: { provider_id: id } });
    expect(detail.error).toBeUndefined();
    expect(detail.data?.provider.id).toBe(id);

    const edited = await updateProviderDetail({
      client,
      query: { provider_id: id },
      body: { provider: { name: 'renamed' } },
    });
    expect(edited.data?.result.name).toBe('renamed');

    const models = await listProviderModels({ client, query: { provider_id: id } });
    expect(models.data?.models).toHaveLength(1);
    expect(models.data?.models[0]?.id).toBe('m');

    const hidden = await updateProviderModel({
      client,
      query: { provider_id: id, model_id: 'm' },
      body: { model: { hidden: true } },
    });
    expect(hidden.data?.result.hidden).toBe(true);

    const reset = await resetProviderModelField({
      client,
      path: { field: 'name' },
      query: { provider_id: id, model_id: 'm' },
    });
    expect(reset.data?.result.id).toBe('m');

    const connected = await connectProvider({ client, query: { provider_id: id }, body: {} });
    expect(connected.data?.result.id).toBe(id);

    const fieldReset = await resetProviderField({
      client,
      path: { field: 'name' },
      query: { provider_id: id },
    });
    expect(fieldReset.data?.result.id).toBe(id);
  }

  // A slash-containing model ID stays one query value.
  const slashed = await createProvider({
    client,
    body: {
      id: 'slashmodel',
      provider: { base_url: 'https://identity.test/v1' },
      models: { 'org/model': { context_window: window } },
    },
  });
  expect(slashed.error).toBeUndefined();
  const slashEdited = await updateProviderModel({
    client,
    query: { provider_id: 'slashmodel', model_id: 'org/model' },
    body: { model: { name: 'Slashed' } },
  });
  expect(slashEdited.data?.result.id).toBe('org/model');

  // The post-state rule crosses the TypeScript boundary: a standalone user
  // model's delete is the required null result, while a bundled model's
  // override delete reveals the base as the non-null post-state.
  const standaloneDelete = await deleteProviderModel({
    client,
    query: { provider_id: 'slashmodel', model_id: 'org/model' },
  });
  expect(standaloneDelete.error).toBeUndefined();
  expect(standaloneDelete.data?.result).toBeNull();
  const configuration = await getConfiguration({ client });
  expect(configuration.error).toBeUndefined();
  const openrouter = configuration.data?.providers.find((p) => p.id === 'openrouter');
  const bundledModel = openrouter?.models.find((m) => m.source === 'bundled');
  expect(bundledModel).toBeDefined();
  const overridden = await updateProviderModel({
    client,
    query: { provider_id: 'openrouter', model_id: bundledModel.id },
    body: { model: { name: 'Overridden' } },
  });
  expect(overridden.data?.result?.name).toBe('Overridden');
  const revealed = await deleteProviderModel({
    client,
    query: { provider_id: 'openrouter', model_id: bundledModel.id },
  });
  expect(revealed.error).toBeUndefined();
  expect(revealed.data?.result?.id).toBe(bundledModel.id);
  expect(revealed.data?.result?.name).not.toBe('Overridden');
  expect(revealed.data?.result?.source).toBe('bundled');

  // A supplied key on an explicitly named external variable refuses before
  // any side effect, and the refused create registers nothing.
  const refused = await createProvider({
    client,
    body: {
      id: 'externalk',
      provider: { base_url: 'https://external.test/v1', api_key_env: 'PATH' },
      models: { m: { context_window: window } },
      api_key: 'mounted-refused-secret',
    },
  });
  expect(refused.error?.code).toBe('configuration');
  const refusedDetail = await getProviderDetail({ client, query: { provider_id: 'externalk' } });
  expect(refusedDetail.error?.code).toBe('not_found');

  // The directory-like Agent type name is addressable for model edits.
  const agentEdit = await setAgentTypeModel({ client, query: { agent_type: '..' }, body: { model: 'prov/m' } });
  expect(agentEdit.error).toBeUndefined();
  expect(agentEdit.data?.result.name).toBe('..');
  expect(agentEdit.data?.result.model).toBe('prov/m');

  // The Session hydration carries the required nullable next-admission
  // selection: a resolved Agent type names its configured model, and an
  // unresolvable type is null over the generated TypeScript boundary.
  const configured = await createSession({ client, body: { workspace: '/tmp/mounted-ts', agent_type: 'solo' } });
  expect(configured.error).toBeUndefined();
  const configuredHydration = await getSessionHydration({ client, path: { id: configured.data.session_id } });
  expect(configuredHydration.error).toBeUndefined();
  expect(configuredHydration.data?.selected_model).toBe('prov/m');
  const unresolved = await createSession({ client, body: { workspace: '/tmp/mounted-ts', agent_type: 'ghost' } });
  expect(unresolved.error).toBeUndefined();
  const unresolvedHydration = await getSessionHydration({ client, path: { id: unresolved.data.session_id } });
  expect(unresolvedHydration.error).toBeUndefined();
  expect(unresolvedHydration.data?.selected_model).toBeNull();

  // Navigation reads the list headers and the anchored history under their
  // own revisions, and the argument-free controls need no request body.
  const listed = await listSessions({ client, query: { workspace: '/tmp/mounted-ts', lifecycle: 'open' } });
  expect(listed.error).toBeUndefined();
  expect(listed.data.some((row) => row.session_id === configured.data.session_id)).toBe(true);
  const history = await getSessionHistory({ client, path: { id: configured.data.session_id } });
  expect(history.error).toBeUndefined();
  expect(history.data?.items).toEqual([]);
  expect(history.data?.session_revision.instance_id).toBe(discovery.instance_id);
  const stopped = await stopSession({ client, path: { id: unresolved.data.session_id } });
  expect(stopped.error).toBeUndefined();
  const interrupted = await interruptSession({ client, path: { id: unresolved.data.session_id } });
  expect(interrupted.error).toBeUndefined();

  // A missing identity is the typed refusal over the same authenticated
  // connection.
  const missing = await getProviderDetail({ client, query: { provider_id: 'ghost' } });
  expect(missing.data).toBeUndefined();
  expect(missing.error?.code).toBe('not_found');
});
