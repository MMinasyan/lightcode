<script>
  import { createEventDispatcher } from 'svelte';
  import { errorText } from '../lib/errors.js';
  import { ConnectProvider, DiscoverCustomProvider, AddCustomProvider } from '../../wailsjs/go/main/App';

  export let providers = [];
  const dispatch = createEventDispatcher();

  let view = 'catalog'; // 'catalog' | 'builtin' | 'custom'
  let busy = false;

  $: available = providers.filter((p) => p.builtin && !p.connected);

  let target = null;
  let apiKey = '';
  function startBuiltin(p) { target = p; apiKey = ''; view = 'builtin'; }
  async function connectBuiltin() {
    if (busy) return;
    busy = true;
    try { await ConnectProvider(target.id, apiKey.trim()); dispatch('added'); }
    catch (e) { dispatch('error', errorText(e)); }
    finally { busy = false; }
  }

  let cName = '';
  let cBaseURL = '';
  let cKey = '';
  let candidates = [];
  let selected = {};
  let manualModels = [];
  let manualModel = { id: '', name: '', contextWindow: 0, maxOutputTokens: 0 };
  let discovered = false;

  function slug(s) { return (s || '').toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-+|-+$/g, ''); }
  function customReq() { return { id: slug(cName), name: cName.trim(), baseURL: cBaseURL.trim(), apiKey: cKey.trim(), apiKeyEnv: '' }; }

  async function discover() {
    if (busy || !cName.trim() || !cBaseURL.trim()) return;
    busy = true;
    try {
      candidates = (await DiscoverCustomProvider(customReq())) || [];
      selected = {};
      for (const c of candidates) if (c.usable) selected[c.id] = true;
      discovered = true;
    } catch (e) { dispatch('error', errorText(e)); }
    finally { busy = false; }
  }

  async function addCustom() {
    if (busy) return;
    const selectedModels = candidates.filter((c) => selected[c.id]).map((c) => ({ id: c.id, name: c.name || c.id, contextWindow: c.contextWindow, maxOutputTokens: c.maxOutputTokens, cost: c.cost, hidden: false }));
    const models = [...selectedModels, ...manualModels];
    if (models.length === 0) { dispatch('error', 'Select or add at least one model.'); return; }
    const ids = new Set();
    for (const m of models) {
      if (ids.has(m.id)) { dispatch('error', `Model ${m.id} is already selected.`); return; }
      ids.add(m.id);
    }
    busy = true;
    try { await AddCustomProvider({ ...customReq(), discovery: true, models }); dispatch('added'); }
    catch (e) { dispatch('error', errorText(e)); }
    finally { busy = false; }
  }

  function addManualModel() {
    const id = manualModel.id.trim();
    const contextWindow = Math.round(Number(manualModel.contextWindow));
    const maxOutputTokens = Math.round(Number(manualModel.maxOutputTokens));
    if (!id) { dispatch('error', 'Model id is required.'); return; }
    if (!Number.isFinite(contextWindow) || contextWindow <= 0) { dispatch('error', 'Context window must be greater than zero.'); return; }
    if (manualModels.some((m) => m.id === id) || candidates.some((c) => c.id === id && selected[c.id])) { dispatch('error', `Model ${id} is already selected.`); return; }
    manualModels = [...manualModels, { id, name: manualModel.name.trim() || id, contextWindow, maxOutputTokens: Number.isFinite(maxOutputTokens) && maxOutputTokens > 0 ? maxOutputTokens : 0, hidden: false }];
    manualModel = { id: '', name: '', contextWindow: 0, maxOutputTokens: 0 };
  }
  function removeManualModel(id) { manualModels = manualModels.filter((m) => m.id !== id); }

  function back() { view = 'catalog'; discovered = false; candidates = []; selected = {}; manualModels = []; }
  function close() { dispatch('close'); }
</script>

<div class="layer">
  <button type="button" class="backdrop" tabindex="-1" aria-label="Close" on:click={close}></button>
  <div class="prompt" role="dialog" aria-modal="true" aria-label="Add provider" tabindex="-1">
    <header class="bar">
      <span class="title">{view === 'catalog' ? 'Add provider' : view === 'builtin' ? `Connect ${target?.name}` : 'Custom endpoint'}</span>
      <button class="x" on:click={close} aria-label="Close">&#x2715;</button>
    </header>

    <div class="body">
      {#if view === 'catalog'}
        <div class="list">
          {#each available as p}
            <button class="opt" on:click={() => startBuiltin(p)}><span>{p.name}</span><span class="caret">›</span></button>
          {/each}
          {#if available.length === 0}<div class="muted">All built-in providers are connected.</div>{/if}
          <button class="opt custom" on:click={() => (view = 'custom')}><span>Custom endpoint</span><span class="caret">›</span></button>
        </div>

      {:else if view === 'builtin'}
        <label class="field"><span class="flabel">API key</span>
          <input type="password" autocomplete="off" placeholder="Paste API key (blank for local)" bind:value={apiKey} />
        </label>
        <div class="actions">
          <button class="ghost" on:click={back}>Back</button>
          <button class="primary" disabled={busy} on:click={connectBuiltin}>{busy ? 'Connecting...' : 'Connect'}</button>
        </div>

      {:else}
        <label class="field"><span class="flabel">Name</span><input type="text" placeholder="My local model" bind:value={cName} /></label>
        <label class="field"><span class="flabel">Base URL</span><input type="text" placeholder="http://localhost:11434/v1" bind:value={cBaseURL} /></label>
        <label class="field"><span class="flabel">API key (optional)</span><input type="password" autocomplete="off" placeholder="Blank for local" bind:value={cKey} /></label>

        {#if discovered}
          <div class="sec">Discovered models</div>
          <div class="cands">
            {#each candidates as c}
              <label class="cand" class:disabled={!c.usable}>
                <input type="checkbox" bind:checked={selected[c.id]} disabled={!c.usable} />
                <span class="cand-name">{c.name || c.id}{c.usable ? '' : ' (unusable)'}</span>
              </label>
            {/each}
            {#if candidates.length === 0}<div class="muted">No models found. Add one manually below.</div>{/if}
          </div>
        {/if}

        <div class="sec">Manual model</div>
        <div class="manual-grid">
          <input type="text" placeholder="model id" bind:value={manualModel.id} />
          <input type="text" placeholder="display name" bind:value={manualModel.name} />
          <input type="number" min="1" placeholder="context" bind:value={manualModel.contextWindow} />
          <input type="number" min="0" placeholder="max output" bind:value={manualModel.maxOutputTokens} />
          <button class="ghost" type="button" on:click={addManualModel}>Add model</button>
        </div>
        {#if manualModels.length !== 0}
          <div class="manual-list">
            {#each manualModels as m}
              <div class="manual-row"><span>{m.name || m.id} <small>{m.id}</small></span><button class="x-mini" type="button" on:click={() => removeManualModel(m.id)}>&#x2715;</button></div>
            {/each}
          </div>
        {/if}

        <div class="actions">
          <button class="ghost" on:click={back}>Back</button>
          {#if !discovered}
            <button class="primary" disabled={busy || !cName.trim() || !cBaseURL.trim()} on:click={discover}>{busy ? 'Checking...' : 'Discover models'}</button>
            {#if manualModels.length !== 0}<button class="primary" disabled={busy} on:click={addCustom}>{busy ? 'Adding...' : 'Add provider'}</button>{/if}
          {:else}
            <button class="primary" disabled={busy} on:click={addCustom}>{busy ? 'Adding...' : 'Add provider'}</button>
          {/if}
        </div>
      {/if}
    </div>
  </div>
</div>

<style>
  .layer { position:fixed; inset:0; z-index:320; display:flex; align-items:center; justify-content:center; }
  .backdrop { position:absolute; inset:0; border:0; padding:0; margin:0; background:var(--overlay); cursor:default; }
  .prompt { position:relative; z-index:1; width:420px; max-width:92vw; max-height:80vh; display:flex; flex-direction:column; background:var(--bg-elevated); border:1px solid var(--border-strong); border-radius:var(--radius); box-shadow:var(--shadow-lg); overflow:hidden; }
  .bar { display:flex; align-items:center; justify-content:space-between; padding:12px 16px; border-bottom:1px solid var(--border); flex-shrink:0; }
  .title { font-size:13px; font-weight:600; }
  .x { background:none; border:0; color:var(--text-dim); font-size:13px; cursor:pointer; padding:2px 6px; border-radius:var(--radius); }
  .x:hover { color:var(--text); background:var(--accent-soft); }
  .body { padding:16px; overflow-y:auto; min-height:0; }

  .list { display:flex; flex-direction:column; gap:6px; }
  .opt { display:flex; align-items:center; justify-content:space-between; padding:11px 14px; border:1px solid var(--border); border-radius:var(--radius); background:var(--bg); color:var(--text); font-family:var(--font-ui); font-size:13px; cursor:pointer; text-align:left; }
  .opt:hover { border-color:var(--accent); }
  .opt .caret { color:var(--text-dim); }
  .opt.custom { margin-top:4px; border-style:dashed; }

  .field { display:grid; grid-template-columns:130px 1fr; align-items:center; gap:0 12px; margin-bottom:12px; }
  .flabel { font-size:12px; color:var(--text-dim); }
  .field input, .manual-grid input { width:100%; min-width:0; box-sizing:border-box; padding:8px 10px; border:1px solid var(--border-button); border-radius:var(--radius); background:var(--bg-input); color:var(--text); font-family:var(--font-ui); font-size:13px; outline:none; }
  .field input:focus, .manual-grid input:focus { border-color:var(--accent); background:var(--bg-input-focus); }

  .sec { font-size:11px; text-transform:uppercase; letter-spacing:.5px; color:var(--text-dim); margin:6px 0 8px; padding-top:10px; border-top:1px solid var(--border); }
  .cands { display:flex; flex-direction:column; gap:2px; max-height:200px; overflow-y:auto; margin-bottom:12px; }
  .cand { display:flex; align-items:center; gap:8px; padding:5px 2px; cursor:pointer; font-size:13px; color:var(--text); }
  .cand.disabled { opacity:.45; }
  .cand-name { overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
  .manual-grid { display:grid; grid-template-columns:1fr 1fr 82px 82px auto; gap:6px; align-items:center; margin-bottom:8px; }
  .manual-list { display:flex; flex-direction:column; gap:4px; margin-bottom:12px; }
  .manual-row { display:flex; align-items:center; justify-content:space-between; gap:8px; padding:5px 8px; border:1px solid var(--border); border-radius:var(--radius); font-size:12px; }
  .manual-row small { color:var(--text-dim); }
  .x-mini { background:none; border:0; color:var(--text-dim); cursor:pointer; }
  .x-mini:hover { color:var(--error); }

  .muted { color:var(--text-dim); font-size:13px; padding:8px 2px; }
  .actions { display:flex; gap:8px; justify-content:flex-end; }
  .ghost { padding:6px 12px; border:1px solid var(--border-button); border-radius:var(--radius); background:none; color:var(--text-dim); font-family:var(--font-ui); font-size:13px; cursor:pointer; }
  .ghost:hover { border-color:var(--accent); color:var(--text); }
  .primary { padding:6px 14px; border:1px solid var(--border-button); border-radius:var(--radius); background:none; color:var(--text); font-family:var(--font-ui); font-size:13px; cursor:pointer; }
  .primary:hover:not(:disabled) { border-color:var(--accent); color:var(--accent); }
  .primary:disabled { opacity:.45; cursor:not-allowed; }
</style>
