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
  getHealth,
  getProviderDetail,
  listProviderModels,
  resetProviderField,
  resetProviderModelField,
  setAgentTypeModel,
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

  // The directory-like Agent type name is addressable for model edits.
  const agentEdit = await setAgentTypeModel({ client, query: { agent_type: '..' }, body: { model: 'prov/m' } });
  expect(agentEdit.error).toBeUndefined();
  expect(agentEdit.data?.result.name).toBe('..');
  expect(agentEdit.data?.result.model).toBe('prov/m');

  // A missing identity is the typed refusal over the same authenticated
  // connection.
  const missing = await getProviderDetail({ client, query: { provider_id: 'ghost' } });
  expect(missing.data).toBeUndefined();
  expect(missing.error?.code).toBe('not_found');
});
