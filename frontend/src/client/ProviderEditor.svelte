<script>
  import { openDialog } from './dialogs';
  // Provider/model editor over the actual generated operations: detail read,
  // one-edited-member patch, field reset, model upsert/delete, provider
  // create/delete, connect/disconnect, discovery and candidate selection. Patch
  // bodies carry only the edited member; no ReloadConfiguration follows a
  // published mutation.
  import { createEventDispatcher, onMount } from 'svelte';
  import { invokeNative } from './results';
  import { sameStamp } from './reader';
  import { composeMemberPatch, composeObjectPatch, parseExactJSON } from './exactjson';
  export let getClient;
  export let stamp; // () => ReaderStamp
  const dispatch = createEventDispatcher();

  // The explicit editable-member lists with the control kind each member's
  // schema requires; the shared field block renders one row per descriptor.
  const fieldsOf = (kind, ...members) => members.map((member) => [kind, member]);
  const PROVIDER_FIELDS = [
    ...fieldsOf('text', 'name', 'base_url', 'api_key_env', 'max_tokens_field'),
    ['role', 'system_role'],
    ...fieldsOf('bool', 'usage_in_stream', 'discovery', 'hidden'),
    ...fieldsOf('json', 'headers', 'options', 'extra_body', 'protocol_metadata'),
  ];
  const MODEL_FIELDS = [
    ['text', 'name'],
    ...fieldsOf('number', 'context_window', 'max_output_tokens'),
    ['role', 'system_role'],
  ];
  const MODEL_FIELDS_END = [
    ['bool', 'usage_in_stream'],
    ...fieldsOf('json', 'extra_body', 'protocol_metadata'),
    ['bool', 'hidden'],
  ];
  const ROLES = ['system', 'user', 'developer'];
  const MODALITIES = ['text', 'image', 'audio', 'document'];
  const COST_KEYS = ['input', 'output', 'cache_read', 'cache_write'];

  let dialog;

  onMount(() => {
    void loadProviders();
  });
  let providers = [];
  let listError = '';
  let listToken = 0;
  let selectedID = '';
  let detail = null;
  let detailError = '';
  let detailToken = 0;
  let busy = false;
  // Monotonic candidate-panel token: selection change and cancellation invalidate the outstanding request.
  let discoveryToken = 0;

  let connectFor = '';
  let connectKey = '';
  let showCreate = false;
  let createId = '';
  let createName = '';
  let createBaseURL = '';
  let createKeyEnv = '';
  let createKey = '';
  let createCandidates = [];
  let createSelected = {};
  let showAddModel = false;
  let addCandidates = [];
  let addModelID = '';
  let addModelName = '';
  let addModelContext = '';
  let addModelOutput = '';


  const invoke = (label, operation, params, decode) => invokeNative(getClient, label, operation, params, decode);

  async function loadProviders() {
    const token = ++listToken;
    const cap = stamp();
    const classified = await invoke('provider list', 'ListProviders', '{}', JSON.parse);
    if (token !== listToken || !sameStamp(cap, stamp())) return;
    if (classified.kind === 'ok') {
      providers = classified.data.providers;
      listError = '';
    } else {
      listError = classified.message;
    }
  }

  async function selectProvider(id) {
    selectedID = id;
    detail = null;
    detailError = '';
    showAddModel = false;
    discoveryToken += 1;
    await loadDetail(id);
  }

  async function loadDetail(id) {
    const token = ++detailToken;
    const cap = stamp();
    // Read through the exact-text decoder so numeric lexemes survive into controls and writes.
    const classified = await invoke('provider detail', 'GetProviderDetail', JSON.stringify({ query: { provider_id: id } }), parseExactJSON);
    if (token !== detailToken || !sameStamp(cap, stamp()) || id !== selectedID) return;
    if (classified.kind === 'ok') {
      detail = classified.data;
      detailError = '';
    } else {
      detail = null;
      detailError = classified.message;
    }
  }

  // ONE addressed-outcome rule for editor mutations.
  async function reportMutation(classified, label, target) {
    if (classified.kind === 'rejection') {
      dispatch('note', `${label} failed for ${target}: ${classified.message}`);
      return false;
    }
    if (classified.kind !== 'ok') {
      dispatch('note', `${label} outcome unknown for ${target}; nothing was retransmitted.`);
      return false;
    }
    dispatch('changed');
    return true;
  }

  // ONE busy lifecycle for every real write: holds `busy` through the addressed
  // outcome and the uniform successful refresh (provider list + surviving detail).
  async function mutate(operation, params, label, target, onSuccess) {
    if (busy) return;
    busy = true;
    try {
      const classified = await invoke(label, operation, params, JSON.parse);
      if (!(await reportMutation(classified, label, target))) return;
      onSuccess?.();
      await loadProviders();
      if (detail && selectedID) await loadDetail(selectedID);
    } finally {
      busy = false;
    }
  }

  // Row values: checkbox boolean, JSON exact text (absent null), number token, string/role text.
  function memberValue(kind, value) {
    if (kind === 'bool') return !!value;
    if (kind === 'json') return JSON.stringify(value ?? null);
    if (kind === 'number') return value === undefined ? '' : JSON.stringify(value);
    // A role select keeps undefined so its mount behavior (first option) is
    // unchanged; a text input renders '' for an absent member.
    return kind === 'role' ? value : (value ?? '');
  }

  // Edit text: checkbox/text/role JSON-encoded; number and JSON forward the raw token.
  function memberText(kind, event) {
    if (kind === 'bool') return JSON.stringify(event.target.checked);
    if (kind === 'number' || kind === 'json') return event.target.value;
    return JSON.stringify(event.target.value);
  }

  async function patchMember(modelID, member, valueText) {
    if (!detail) return;
    const providerID = detail.provider.id;
    // The two generated operations differ only in query identity and body root key.
    const bodyText = composeMemberPatch(modelID === null ? 'provider' : 'model', composeMemberPatch(member, valueText));
    if (modelID === null) {
      await mutate('UpdateProviderDetail', composeObjectPatch([
        ['query', JSON.stringify({ provider_id: providerID })],
        ['body', bodyText],
      ]), 'Provider edit', providerID);
    } else {
      await mutate('UpdateProviderModel', composeObjectPatch([
        ['query', JSON.stringify({ provider_id: providerID, model_id: modelID })],
        ['body', bodyText],
      ]), 'Model edit', `${providerID}/${modelID}`);
    }
  }

  async function resetMember(modelID, member) {
    if (!detail) return;
    const providerID = detail.provider.id;
    if (modelID === null) {
      await mutate('ResetProviderField', JSON.stringify({
        field: member,
        query: { provider_id: providerID },
      }), 'Provider reset', providerID);
    } else {
      await mutate('ResetProviderModelField', JSON.stringify({
        field: member,
        query: { provider_id: providerID, model_id: modelID },
      }), 'Model reset', `${providerID}/${modelID}`);
    }
  }

  async function removeModel(modelID) {
    if (!detail) return;
    const target = detail.provider.id;
    await mutate('DeleteProviderModel', JSON.stringify({
      query: { provider_id: target, model_id: modelID },
    }), 'Model delete', `${target}/${modelID}`);
  }

  async function toggleModality(model, modality) {
    const current = model.input_modalities || [];
    const next = current.includes(modality) ? current.filter((entry) => entry !== modality) : [...current, modality];
    await patchMember(model.id, 'input_modalities', JSON.stringify(next));
  }

  async function saveCost(model, key, valueText) {
    // Whole-member replacement from the edited price and present siblings only:
    // absent ones are omitted, a cleared price is omitted, present zeros survive.
    const members = [];
    for (const costKey of COST_KEYS) {
      if (costKey === key) {
        if (valueText.trim() !== '') members.push([costKey, valueText]);
        continue;
      }
      const value = model.cost ? model.cost[costKey] : undefined;
      if (value !== undefined) members.push([costKey, JSON.stringify(value)]);
    }
    await patchMember(model.id, 'cost', composeObjectPatch(members));
  }

  function cancelAddModel() {
    discoveryToken += 1;
    showAddModel = false;
  }

  async function startAddModel() {
    if (!detail) return;
    showAddModel = true;
    addCandidates = [];
    addModelID = '';
    addModelName = '';
    addModelContext = '';
    addModelOutput = '';
    const target = detail.provider.id;
    const cap = stamp();
    const token = ++discoveryToken;
    const classified = await invoke('model discovery', 'DiscoverProviderModelCandidates', JSON.stringify({ query: { provider_id: target } }), parseExactJSON);
    if (token !== discoveryToken || !sameStamp(cap, stamp())) return;
    if (classified.kind === 'ok') addCandidates = classified.data;
    else if (classified.kind === 'rejection') dispatch('note', `Model discovery failed: ${classified.message}`);
    else dispatch('note', 'Model discovery outcome unknown; nothing was retransmitted.');
  }

  function pickCandidate(candidate) {
    addModelID = candidate.id;
    addModelName = candidate.name || candidate.id;
    addModelContext = JSON.stringify(candidate.context_window);
    addModelOutput = JSON.stringify(candidate.max_output_tokens);
  }

  // Model upsert is one ordinary metadata patch for the selected candidate or authored id.
  async function addModel() {
    if (!detail || !addModelID.trim()) return;
    const target = detail.provider.id;
    const modelID = addModelID.trim();
    const members = [['name', JSON.stringify(addModelName)]];
    if (addModelContext.trim() !== '') members.push(['context_window', addModelContext]);
    if (addModelOutput.trim() !== '') members.push(['max_output_tokens', addModelOutput]);
    const bodyText = composeMemberPatch('model', composeObjectPatch(members));
    await mutate('UpdateProviderModel', composeObjectPatch([
      ['query', JSON.stringify({ provider_id: target, model_id: modelID })],
      ['body', bodyText],
    ]), 'Model upsert', `${target}/${modelID}`, () => {
      showAddModel = false;
    });
  }

  async function startConnect(providerID) {
    connectFor = providerID;
    connectKey = '';
  }

  async function submitConnect() {
    const target = connectFor;
    if (!target) return;
    const key = connectKey.trim();
    await mutate('ConnectProvider', JSON.stringify({
      query: { provider_id: target },
      body: key === '' ? {} : { api_key: key },
    }), 'Provider connect', target, () => {
      connectFor = '';
      connectKey = '';
    });
  }

  async function disconnect(providerID) {
    await mutate('DisconnectProvider', JSON.stringify({
      query: { provider_id: providerID },
    }), 'Provider disconnect', providerID);
  }

  async function removeProvider(providerID) {
    await mutate('DeleteProviderDetail', JSON.stringify({
      query: { provider_id: providerID },
    }), 'Provider delete', providerID, () => {
      if (selectedID === providerID) {
        selectedID = '';
        detail = null;
        detailToken += 1;
      }
    });
  }

  async function discoverCreateCandidates() {
    if (busy) return;
    busy = true;
    try {
      const cap = stamp();
      const classified = await invoke('provider discovery', 'DiscoverProviderCandidates', JSON.stringify({
        body: {
          base_url: createBaseURL,
          api_key_env: createKeyEnv,
          ...(createKey.trim() === '' ? {} : { api_key: createKey.trim() }),
        },
      }), parseExactJSON);
      if (!sameStamp(cap, stamp())) return;
      if (classified.kind === 'ok') {
        createCandidates = classified.data;
        // Every candidate gets an own selection entry — unavailable included —
        // so a compiled name (constructor, toString, __proto__) is never read
        // from the prototype chain as selected.
        createSelected = Object.fromEntries(createCandidates.map((candidate) => [candidate.id, candidate.usable]));
      } else if (classified.kind === 'rejection') {
        dispatch('note', `Provider discovery failed: ${classified.message}`);
      } else {
        dispatch('note', 'Provider discovery outcome unknown; nothing was retransmitted.');
      }
    } finally {
      busy = false;
    }
  }

  async function create() {
    if (!createId.trim()) return;
    const id = createId.trim();
    const name = createName;
    const baseURL = createBaseURL;
    const keyEnv = createKeyEnv;
    const key = createKey.trim();
    // Object.fromEntries writes own data properties, so a compiled candidate
    // name stays ordinary data instead of hitting the prototype setter.
    const models = Object.fromEntries(
      createCandidates
        .filter((candidate) => createSelected[candidate.id])
        .map((candidate) => [candidate.id, {
          name: candidate.name || candidate.id,
          context_window: candidate.context_window,
          max_output_tokens: candidate.max_output_tokens,
          ...(candidate.cost ? { cost: candidate.cost } : {}),
        }]),
    );
    await mutate('CreateProvider', JSON.stringify({
      body: {
        id,
        // A blank authored env name is omitted; the Runtime allocates the binding.
        provider: {
          name,
          base_url: baseURL,
          ...(keyEnv.trim() === '' ? {} : { api_key_env: keyEnv }),
        },
        models,
        ...(key === '' ? {} : { api_key: key }),
      },
    }), 'Provider create', id, () => {
      showCreate = false;
      createId = '';
      createName = '';
      createBaseURL = '';
      createKeyEnv = '';
      createKey = '';
      createCandidates = [];
      createSelected = {};
    });
  }

  async function reload() {
    if (busy) return;
    busy = true;
    try {
      const classified = await invoke('configuration reload', 'ReloadConfiguration', '{}', JSON.parse);
      if (classified.kind === 'rejection') {
        dispatch('note', `Configuration reload failed: ${classified.message}`);
        return;
      }
      if (classified.kind !== 'ok') {
        dispatch('note', 'Configuration reload outcome unknown; nothing was retransmitted.');
        return;
      }
      dispatch('note', 'Configuration reloaded.');
      dispatch('changed');
      await loadProviders();
      if (selectedID) await loadDetail(selectedID);
    } finally {
      busy = false;
    }
  }
</script>

{#snippet fieldRow(kind, label, aria, value, resetLabel, onReset, onEdit)}
  <label class="row"><span class="label">{label}</span>
    <span class="ctl">
      {#if kind === 'bool'}
        <input type="checkbox" checked={value} aria-label={aria} disabled={busy} on:change={onEdit} />
      {:else if kind === 'json'}
        <textarea rows="2" aria-label={aria} disabled={busy} {value} on:change={onEdit}></textarea>
      {:else if kind === 'role'}
        <select {value} aria-label={aria} disabled={busy} on:change={onEdit}>
          {#each ROLES as role}<option value={role}>{role}</option>{/each}
        </select>
      {:else}
        <input class:num={kind === 'number'} type="text" {value} aria-label={aria} disabled={busy} on:change={onEdit} />
      {/if}
      <button class="reset" disabled={busy} on:click={onReset} aria-label={resetLabel}>↺</button>
    </span>
  </label>
{/snippet}

<dialog class="modal" bind:this={dialog} use:openDialog aria-labelledby="providers-title"
  on:click={(e) => { if (e.target === dialog) dialog.close(); }}
  on:close={() => dispatch('close')}>
  <div class="panel">
    <div id="providers-title" class="hdr"><span>Providers and models</span>
      <button class="panel-close" on:click={() => dialog.close()} aria-label="Close provider editor">×</button>
    </div>
    <div class="layout">
      <div class="sidebar">
        {#if listError}<div class="error">{listError}</div>{/if}
        {#each providers as provider (provider.id)}
          <button class="prow" class:cur={provider.id === selectedID} on:click={() => void selectProvider(provider.id)}
            aria-label={`Select provider ${provider.id}`}>
            <span class="pname">{provider.name}</span>
            <span class="psub">{provider.id}{provider.builtin ? ' · builtin' : ' · custom'} · {provider.connected ? 'connected' : 'disconnected'}</span>
          </button>
        {/each}
        <div class="side-actions">
          <button class="ghost" on:click={() => (showCreate = !showCreate)} aria-label="New provider">+ Provider</button>
          <button class="ghost" disabled={busy} on:click={() => void reload()} aria-label="Reload configuration">Reload</button>
        </div>
      </div>
      <div class="main">
        {#if showCreate}
          <div class="create">
            <div class="sec">New provider</div>
            <label class="row"><span class="label">id</span><input type="text" bind:value={createId} disabled={busy} aria-label="Provider id" /></label>
            <label class="row"><span class="label">name</span><input type="text" bind:value={createName} disabled={busy} aria-label="Provider name" /></label>
            <label class="row"><span class="label">base_url</span><input type="text" bind:value={createBaseURL} disabled={busy} aria-label="Provider base url" /></label>
            <label class="row"><span class="label">api_key_env</span><input type="text" bind:value={createKeyEnv} disabled={busy} aria-label="Provider api key env" /></label>
            <label class="row"><span class="label">api_key</span><input type="password" autocomplete="off" bind:value={createKey} disabled={busy} aria-label="Provider api key" /></label>
            <div class="actions">
              <button class="ghost" disabled={busy || !createBaseURL} on:click={() => void discoverCreateCandidates()} aria-label="Discover provider candidates">Discover</button>
              <button class="primary" disabled={busy || !createId.trim()} on:click={() => void create()} aria-label="Create provider">Create</button>
            </div>
            {#if createCandidates.length > 0}
              <div class="sec">Discovered models</div>
              {#each createCandidates as candidate (candidate.id)}
                <label class="cand"><input type="checkbox" aria-label={`Select candidate ${candidate.id}`} bind:checked={createSelected[candidate.id]} disabled={busy || !candidate.usable} />
                  {candidate.name || candidate.id}{candidate.usable ? '' : ' (unusable)'}</label>
              {/each}
            {/if}
          </div>
        {/if}
        {#if detailError}
          <div class="error">{detailError}</div>
        {:else if !detail}
          <div class="empty">Select a provider.</div>
        {:else}
          <div class="sec">Provider {detail.provider.id} · {detail.provider.builtin ? 'builtin source' : 'custom source'} · key {detail.provider.key_source} · {detail.provider.connected ? 'connected' : 'disconnected'}</div>
          <div class="actions">
            {#if detail.provider.connected}
              <button class="ghost" disabled={busy} on:click={() => void disconnect(detail.provider.id)} aria-label="Disconnect provider">Disconnect</button>
            {:else}
              <button class="ghost" disabled={busy} on:click={() => startConnect(detail.provider.id)} aria-label="Connect provider">Connect</button>
            {/if}
            <button class="ghost danger" disabled={busy} on:click={() => void removeProvider(detail.provider.id)} aria-label="Delete provider">Delete provider</button>
          </div>
          {#if connectFor === detail.provider.id}
            <div class="connect">
              <label class="row"><span class="label">api_key</span><input type="password" autocomplete="off" bind:value={connectKey} disabled={busy} aria-label="Connect api key" /></label>
              <div class="actions">
                <button class="ghost" on:click={() => (connectFor = '')}>Cancel</button>
                <button class="primary" disabled={busy} on:click={() => void submitConnect()} aria-label="Submit connect">Connect</button>
              </div>
            </div>
          {/if}

          {#each PROVIDER_FIELDS as [kind, member] (member)}
            {@render fieldRow(kind, member, `provider ${member}`,
              memberValue(kind, member === 'headers' ? (detail.provider.user_headers || {}) : detail.provider[member]),
              `Reset provider ${member}`,
              () => void resetMember(null, member),
              (e) => void patchMember(null, member, memberText(kind, e)))}
          {/each}

          <div class="sec">Models</div>
          <div class="actions">
            <button class="ghost" disabled={busy} on:click={() => void startAddModel()} aria-label="Add model">+ Model</button>
          </div>
          {#if showAddModel}
            <div class="create">
              {#if addCandidates.length > 0}
                <div class="sec">Candidates</div>
                {#each addCandidates as candidate (candidate.id)}
                  <button class="ghost" disabled={busy} on:click={() => pickCandidate(candidate)} aria-label={`Select candidate ${candidate.id}`}>{candidate.name || candidate.id}</button>
                {/each}
              {/if}
              <label class="row"><span class="label">id</span><input type="text" bind:value={addModelID} disabled={busy} aria-label="Model id" /></label>
              <label class="row"><span class="label">name</span><input type="text" bind:value={addModelName} disabled={busy} aria-label="Model name" /></label>
              <label class="row"><span class="label">context_window</span><input type="text" bind:value={addModelContext} disabled={busy} aria-label="Model context window" /></label>
              <label class="row"><span class="label">max_output_tokens</span><input type="text" bind:value={addModelOutput} disabled={busy} aria-label="Model max output" /></label>
              <div class="actions">
                <button class="ghost" on:click={cancelAddModel}>Cancel</button>
                <button class="primary" disabled={busy || !addModelID.trim()} on:click={() => void addModel()} aria-label="Upsert model">Add model</button>
              </div>
            </div>
          {/if}
          {#each detail.provider.models as model (model.id)}
            <div class="model">
              <div class="model-hdr">
                <span class="mname">{model.name || model.id}</span>
                <span class="msrc">{model.source}{model.incomplete ? ' · incomplete' : ''}{model.hidden ? ' · hidden' : ''}</span>
                <button class="ghost danger" disabled={busy} on:click={() => void removeModel(model.id)} aria-label={`Delete model ${model.id}`}>Delete</button>
              </div>
              {#each MODEL_FIELDS as [kind, member] (member)}
                {@render fieldRow(kind, member, `${model.id} ${member}`,
                  memberValue(kind, model[member]),
                  `Reset ${model.id} ${member}`,
                  () => void resetMember(model.id, member),
                  (e) => void patchMember(model.id, member, memberText(kind, e)))}
              {/each}
              <label class="row"><span class="label">input_modalities</span>
                <span class="ctl">
                  {#each MODALITIES as modality}
                    <label class="chip" class:on={(model.input_modalities || []).includes(modality)}>
                      <input type="checkbox" checked={(model.input_modalities || []).includes(modality)}
                        disabled={busy}
                        on:change={() => void toggleModality(model, modality)} />{modality}</label>
                  {/each}
                  <button class="reset" disabled={busy} on:click={() => void resetMember(model.id, 'input_modalities')} aria-label={`Reset ${model.id} input_modalities`}>↺</button>
                </span>
              </label>
              <label class="row"><span class="label">cost</span>
                <span class="ctl cost">
                  {#each COST_KEYS as key}
                    <input class="num" type="text" value={model.cost && model.cost[key] !== undefined ? JSON.stringify(model.cost[key]) : ''}
                      placeholder={key} aria-label={`${model.id} cost ${key}`}
                      disabled={busy}
                      on:change={(e) => void saveCost(model, key, e.target.value)} />
                  {/each}
                  <button class="reset" disabled={busy} on:click={() => void resetMember(model.id, 'cost')} aria-label={`Reset ${model.id} cost`}>↺</button>
                </span>
              </label>
              {#each MODEL_FIELDS_END as [kind, member] (member)}
                {@render fieldRow(kind, member, `${model.id} ${member}`,
                  memberValue(kind, model[member]),
                  `Reset ${model.id} ${member}`,
                  () => void resetMember(model.id, member),
                  (e) => void patchMember(model.id, member, memberText(kind, e)))}
              {/each}
            </div>
          {/each}
        {/if}
      </div>
    </div>
  </div>
</dialog>

<style>
  .panel { position:absolute; inset:40px 24px 50px 24px; background:var(--bg-elevated); border:1px solid var(--border-strong); box-shadow:var(--shadow-lg); display:flex; flex-direction:column; }
  .hdr { display:flex; justify-content:space-between; align-items:center; padding:6px 12px; font-size:12px; color:var(--text-dim); text-transform:uppercase; letter-spacing:.5px; border-bottom:1px solid var(--border); }
  .layout { flex:1; display:flex; min-height:0; }
  .sidebar { flex:0 0 220px; display:flex; flex-direction:column; gap:2px; border-right:1px solid var(--border); padding:8px; overflow-y:auto; }
  .prow { display:flex; flex-direction:column; gap:2px; padding:6px 8px; background:none; border:1px solid transparent; border-radius:var(--radius); color:var(--text); font-family:var(--font-ui); font-size:12px; cursor:pointer; text-align:left; }
  .prow:hover { background:var(--accent-soft); }
  .prow.cur { border-color:var(--accent); color:var(--accent); }
  .pname { overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
  .psub { color:var(--text-dim); font-size:10px; overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
  .side-actions { display:flex; gap:4px; margin-top:auto; padding-top:8px; }
  .main { flex:1; min-width:0; overflow-y:auto; padding:10px 12px; display:flex; flex-direction:column; gap:6px; }
  .sec { color:var(--text-dim); font-family:var(--font-ui); font-size:11px; text-transform:uppercase; letter-spacing:.5px; border-top:1px solid var(--border); padding-top:8px; }
  .row { display:grid; grid-template-columns:160px 1fr; align-items:center; gap:8px; }
  .label { color:var(--text-dim); font-family:var(--font-ui); font-size:12px; }
  .ctl { display:flex; align-items:center; gap:6px; min-width:0; }
  .ctl input[type=text], .ctl select, .ctl textarea, .row > input, .create input, .connect input { width:100%; box-sizing:border-box; padding:5px 8px; border:1px solid var(--border-button); border-radius:var(--radius); background:var(--bg-input); color:var(--text); font-family:var(--font-mono); font-size:12px; outline:none; }
  .ctl textarea { resize:vertical; }
  .ctl input:focus, .ctl select:focus, .ctl textarea:focus, .create input:focus, .connect input:focus { border-color:var(--accent); }
  .num { text-align:right; }
  .cost { flex-wrap:wrap; }
  .cost .num { width:80px; }
  .reset { background:none; border:1px solid var(--border-button); border-radius:var(--radius); color:var(--text-dim); font-size:12px; cursor:pointer; padding:3px 7px; flex-shrink:0; }
  .reset:hover:not(:disabled) { color:var(--accent); border-color:var(--accent); }
  .reset:disabled { opacity:.35; cursor:default; }
  .actions { display:flex; gap:6px; justify-content:flex-end; }
  .ghost, .primary { padding:4px 10px; border:1px solid var(--border-button); border-radius:var(--radius); background:none; color:var(--text-dim); font-family:var(--font-ui); font-size:11px; cursor:pointer; }
  .ghost:hover:not(:disabled), .primary:hover:not(:disabled) { color:var(--accent); border-color:var(--accent); }
  .ghost.danger:hover:not(:disabled) { color:var(--error); border-color:var(--error); }
  .ghost:disabled, .primary:disabled { opacity:.45; cursor:default; }
  .model { border:1px solid var(--border); border-radius:var(--radius); padding:6px 8px; background:var(--bg); }
  .model-hdr { display:flex; align-items:center; gap:8px; padding-bottom:4px; }
  .mname { flex:1; color:var(--text); font-family:var(--font-mono); font-size:12px; overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
  .msrc { color:var(--text-dim); font-family:var(--font-ui); font-size:10px; text-transform:uppercase; letter-spacing:.5px; }
  .chip { display:inline-flex; align-items:center; gap:4px; padding:2px 7px; border:1px solid var(--border-button); border-radius:10px; color:var(--text-dim); font-family:var(--font-ui); font-size:11px; cursor:pointer; }
  .chip.on { border-color:var(--accent); color:var(--accent); }
  .cand { display:flex; align-items:center; gap:6px; color:var(--text); font-family:var(--font-ui); font-size:12px; padding:2px 0; }
  .create, .connect { border:1px solid var(--border); border-radius:var(--radius); padding:8px; background:var(--bg); display:flex; flex-direction:column; gap:6px; }
  .error { color:var(--error); font-family:var(--font-ui); font-size:12px; white-space:pre-wrap; }
  .empty { color:var(--text-dim); font-family:var(--font-ui); font-size:12px; }
</style>
