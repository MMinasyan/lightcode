<script>
  import { createEventDispatcher, onMount } from 'svelte';
  import { errorText } from '../lib/errors.js';
  import Dropdown from './Dropdown.svelte';
  import KeyValueEditor from './KeyValueEditor.svelte';
  import {
    GetProviderConfig, SetProviderConfig, ResetProviderField,
    SaveModel, DeleteModel, ResetModelField, DiscoverableModels,
  } from '../../wailsjs/go/main/App';

  export let providerId;
  const dispatch = createEventDispatcher();

  let cfg = null;
  let openSection = null;   // 'advanced' | 'models'
  let openModel = null;     // modelId
  let adding = false;
  let candidates = [];
  let loadingCandidates = false;
  let newModel = { id: '', name: '', contextWindow: 0, maxOutputTokens: 0, cost: null };
  let mutating = false;

  const ROLE_OPTIONS = [
    { value: 'system', label: 'system' },
    { value: 'user', label: 'user' },
    { value: 'developer', label: 'developer' },
  ];
  const MODALITIES = ['text', 'image', 'audio', 'document'];

  function fail(e) { dispatch('error', errorText(e)); }

  async function reload() {
    try { cfg = await GetProviderConfig(providerId); }
    catch (e) { fail(e); }
  }
  onMount(reload);

  async function withMutation(fn) {
    if (mutating) return;
    mutating = true;
    try { await fn(); }
    finally { mutating = false; }
  }
  async function saveProvider(patch) {
    await withMutation(async () => {
      try { await SetProviderConfig(providerId, patch); } catch (e) { fail(e); }
      finally { await reload(); dispatch('changed'); }
    });
  }
  async function resetProvider(field) {
    await withMutation(async () => {
      try { await ResetProviderField(providerId, field); } catch (e) { fail(e); }
      finally { await reload(); dispatch('changed'); }
    });
  }
  async function saveModel(modelId, patch) {
    await withMutation(async () => {
      try { await SaveModel(providerId, modelId, patch); } catch (e) { fail(e); }
      finally { await reload(); dispatch('changed'); }
    });
  }
  async function resetModel(modelId, field) {
    await withMutation(async () => {
      try { await ResetModelField(providerId, modelId, field); } catch (e) { fail(e); }
      finally { await reload(); dispatch('changed'); }
    });
  }
  async function deleteModel(modelId) {
    await withMutation(async () => {
      try { await DeleteModel(providerId, modelId); } catch (e) { fail(e); }
      finally { await reload(); dispatch('changed'); }
    });
  }

  function toggleExpand(id) { openModel = openModel === id ? null : id; }
  function toggleSection(s) { openSection = openSection === s ? null : s; }

  function modalitiesOf(m) { return new Set(m.inputModalities || []); }
  function toggleModality(m, mod) {
    const set = modalitiesOf(m);
    if (set.has(mod)) set.delete(mod); else set.add(mod);
    saveModel(m.id, { inputModalities: [...set] });
  }
  function costNumber(v) { const n = Number(v); return Number.isFinite(n) && v !== '' ? n : undefined; }
  function saveCost(m, key, value) {
    const c = m.cost || {};
    const next = { input: c.input, output: c.output, cache_read: c.cache_read, cache_write: c.cache_write };
    next[key] = costNumber(value);
    saveModel(m.id, { cost: next });
  }

  async function startAdd() {
    if (adding) { adding = false; return; }
    adding = true;
    newModel = { id: '', name: '', contextWindow: 0, maxOutputTokens: 0, cost: null };
    candidates = [];
    loadingCandidates = true;
    try { candidates = (await DiscoverableModels(providerId)) || []; }
    catch (e) { fail(e); }
    finally { loadingCandidates = false; }
  }
  function pickCandidate(c) {
    if (!c) return;
    newModel = { id: c.id, name: c.name || c.id, contextWindow: c.contextWindow || 0, maxOutputTokens: c.maxOutputTokens || 0, cost: c.cost || null };
  }
  async function addModel() {
    const id = (newModel.id || '').trim();
    if (!id) { fail('Select a model first'); return; }
    const cw = Math.round(Number(newModel.contextWindow)) || 0;
    if (cw <= 0) { fail('Set a context window'); return; }
    await saveModel(id, {
      name: (newModel.name || '').trim(),
      contextWindow: cw,
      maxOutputTokens: Math.round(Number(newModel.maxOutputTokens)) || 0,
      cost: newModel.cost || undefined,
    });
    newModel = { id: '', name: '', contextWindow: 0, maxOutputTokens: 0, cost: null };
    adding = false;
  }
</script>

<div class="cfg-body">
      {#if !cfg}
        <div class="muted">Loading...</div>
      {:else}
        <!-- Provider -->
        {#if !cfg.builtin}
          <div class="field"><span class="flabel">Name</span>
            <span class="ctl"><input type="text" disabled={mutating} bind:value={cfg.name} on:change={() => saveProvider({ name: cfg.name })} />
              <button class="reset" title="Reset" disabled={mutating} on:click={() => resetProvider('name')}>&#x21ba;</button></span>
          </div>
        {/if}
        <div class="field"><span class="flabel">Base URL</span>
          {#if cfg.builtin}
            <span class="readonly">{cfg.baseURL}</span>
          {:else}
            <span class="ctl"><input type="text" disabled={mutating} bind:value={cfg.baseURL} on:change={() => saveProvider({ baseURL: cfg.baseURL })} />
              <button class="reset" title="Reset" disabled={mutating} on:click={() => resetProvider('base_url')}>&#x21ba;</button></span>
          {/if}
        </div>
        <div class="field"><span class="flabel">API key variable</span>
          <div class="stack">
            <span class="readonly">{cfg.apiKeyEnv || '(keyless)'}</span>
            <span class="hint">Disconnect the provider to change the key variable.</span>
          </div>
        </div>
        <div class="fold" class:open={openSection === 'advanced'}>
          <button class="fold-head" on:click={() => toggleSection('advanced')}>
            <span class="caret" class:open={openSection === 'advanced'}>›</span>
            <span class="fold-label">Advanced</span>
          </button>
          {#if openSection === 'advanced'}
            <div class="fold-body">
              <div class="field"><span class="flabel">Headers</span>
                <KeyValueEditor value={cfg.userHeaders} on:change={(e) => saveProvider({ headers: e.detail })} />
              </div>
              <div class="field"><span class="flabel">System role</span>
                {#if cfg.builtin}
                  <span class="readonly">{cfg.systemRole}</span>
                {:else}
                  <Dropdown value={cfg.systemRole} options={ROLE_OPTIONS} on:change={(e) => saveProvider({ systemRole: e.detail })} />
                {/if}
              </div>
              <div class="field"><span class="flabel">Max tokens field</span>
                {#if cfg.builtin}
                  <span class="readonly">{cfg.maxTokensField}</span>
                {:else}
                  <span class="ctl"><input type="text" disabled={mutating} bind:value={cfg.maxTokensField} on:change={() => saveProvider({ maxTokensField: cfg.maxTokensField })} />
                    <button class="reset" title="Reset" disabled={mutating} on:click={() => resetProvider('max_tokens_field')}>&#x21ba;</button></span>
                {/if}
              </div>
            </div>
          {/if}
        </div>

        <div class="fold" class:open={openSection === 'models'}>
          <button class="fold-head" on:click={() => toggleSection('models')}>
            <span class="caret" class:open={openSection === 'models'}>›</span>
            <span class="fold-label">Models</span>
          </button>
          {#if openSection === 'models'}
            <div class="fold-body">
        {#each cfg.models as m (m.id)}
          <div class="fold" class:open={openModel === m.id}>
            <button class="fold-head" on:click={() => toggleExpand(m.id)}>
              <span class="caret" class:open={openModel === m.id}>›</span>
              <span class="fold-label">
                <span class="fl-name">{m.name || m.id}</span>
                {#if m.name && m.name !== m.id}<span class="fl-id">{m.id}</span>{/if}
              </span>
            </button>
            {#if openModel === m.id}
              <div class="fold-body">
                {#if m.source === 'user'}
                  <div class="field"><span class="flabel">Name</span>
                    <span class="ctl"><input type="text" disabled={mutating} bind:value={m.name} on:change={() => saveModel(m.id, { name: m.name })} />
                      <button class="reset" title="Reset" disabled={mutating} on:click={() => resetModel(m.id, 'name')}>&#x21ba;</button></span>
                  </div>
                {/if}
                <label class="field"><span class="flabel">Context window</span>
                  <span class="ctl"><input class="num" type="number" min="0" disabled={mutating} bind:value={m.contextWindow} on:change={() => saveModel(m.id, { contextWindow: Math.round(Number(m.contextWindow)) || 0 })} />
                    <button class="reset" title="Reset" disabled={mutating} on:click={() => resetModel(m.id, 'context_window')}>&#x21ba;</button></span>
                </label>
                <label class="field"><span class="flabel">Max output</span>
                  <span class="ctl"><input class="num" type="number" min="0" disabled={mutating} bind:value={m.maxOutputTokens} on:change={() => saveModel(m.id, { maxOutputTokens: Math.round(Number(m.maxOutputTokens)) || 0 })} />
                    <button class="reset" title="Reset" disabled={mutating} on:click={() => resetModel(m.id, 'max_output_tokens')}>&#x21ba;</button></span>
                </label>
                {#if m.source === 'user'}
                  <div class="field"><span class="flabel">System role</span>
                    <Dropdown value={m.systemRole} options={ROLE_OPTIONS} on:change={(e) => saveModel(m.id, { systemRole: e.detail })} />
                  </div>
                  <div class="field"><span class="flabel">Input modalities</span>
                    <div class="chips">
                      {#each MODALITIES as mod}
                        <label class="chip" class:on={modalitiesOf(m).has(mod)}>
                          <input type="checkbox" checked={modalitiesOf(m).has(mod)} on:change={() => toggleModality(m, mod)} />{mod}
                        </label>
                      {/each}
                    </div>
                  </div>
                {/if}
                <div class="field"><span class="flabel">Price ($ per 1M tokens)</span>
                  <div class="grid4">
                    <label class="costcell"><span class="costlbl">Input</span><input class="num" type="number" min="0" step="0.01" disabled={mutating} value={m.cost?.input ?? ''} on:change={(e) => saveCost(m, 'input', e.target.value)} /></label>
                    <label class="costcell"><span class="costlbl">Output</span><input class="num" type="number" min="0" step="0.01" disabled={mutating} value={m.cost?.output ?? ''} on:change={(e) => saveCost(m, 'output', e.target.value)} /></label>
                    <label class="costcell"><span class="costlbl">Cache read</span><input class="num" type="number" min="0" step="0.01" disabled={mutating} value={m.cost?.cache_read ?? ''} on:change={(e) => saveCost(m, 'cache_read', e.target.value)} /></label>
                    <label class="costcell"><span class="costlbl">Cache write</span><input class="num" type="number" min="0" step="0.01" disabled={mutating} value={m.cost?.cache_write ?? ''} on:change={(e) => saveCost(m, 'cache_write', e.target.value)} /></label>
                  </div>
                </div>
                {#if m.source === 'user'}
                  <div class="mfoot">
                    <button class="ghost danger" disabled={mutating} on:click={() => deleteModel(m.id)}>Delete model</button>
                  </div>
                {/if}
              </div>
            {/if}
          </div>
        {/each}

        {#if adding}
          <div class="addbox">
            {#if loadingCandidates}
              <div class="muted">Finding available models...</div>
            {:else if candidates.length === 0}
              <div class="muted">No additional models found for this provider.</div>
              <div class="actions"><button class="ghost" disabled={mutating} on:click={() => (adding = false)}>Close</button></div>
            {:else}
              <div class="field"><span class="flabel">Model</span>
                <Dropdown value={newModel.id} placeholder="Select a model..."
                  options={candidates.map((c) => ({ value: c.id, label: c.name && c.name !== c.id ? `${c.name}  (${c.id})` : c.id }))}
                  on:change={(e) => pickCandidate(candidates.find((c) => c.id === e.detail))} />
              </div>
              {#if newModel.id}
                <label class="field"><span class="flabel">Name</span><input type="text" disabled={mutating} bind:value={newModel.name} /></label>
                <label class="field"><span class="flabel">Context window</span><input class="num" type="number" min="0" disabled={mutating} bind:value={newModel.contextWindow} /></label>
                <label class="field"><span class="flabel">Max output</span><input class="num" type="number" min="0" disabled={mutating} bind:value={newModel.maxOutputTokens} /></label>
              {/if}
              <div class="actions"><button class="ghost" disabled={mutating} on:click={() => (adding = false)}>Cancel</button><button class="primary" disabled={mutating || !newModel.id} on:click={addModel}>Add</button></div>
            {/if}
          </div>
        {/if}
              <div class="addrow"><button class="primary" disabled={mutating} on:click={startAdd}>+ Add model</button></div>
            </div>
          {/if}
        </div>
      {/if}
</div>

<style>
  .cfg-body { padding:4px 2px 2px; }
  .muted { color:var(--text-dim); font-size:13px; }

  .field { display:grid; grid-template-columns:150px 1fr; align-items:center; gap:0 12px; margin-bottom:10px; }
  .flabel { font-size:12px; color:var(--text-dim); }
  .hint { font-size:11px; color:var(--text-dim); }
  .readonly { font-family:var(--font-mono); font-size:12px; color:var(--text); padding:7px 10px; border:1px solid var(--border); border-radius:var(--radius); background:var(--bg); }
  .stack { display:flex; flex-direction:column; gap:4px; min-width:0; }
  .ctl { display:flex; gap:6px; align-items:center; }
  .field input, .addbox input { width:100%; min-width:0; box-sizing:border-box; padding:7px 10px; border:1px solid var(--border-button); border-radius:var(--radius); background:var(--bg-input); color:var(--text); font-family:var(--font-ui); font-size:13px; outline:none; }
  .ctl input { flex:1; }
  .field input:focus, .addbox input:focus { border-color:var(--accent); background:var(--bg-input-focus); }
  .field input:disabled, .addbox input:disabled { opacity:.5; cursor:not-allowed; }
  .num { font-family:var(--font-mono); font-size:12px; text-align:right; }
  .num[type=number] { -moz-appearance:textfield; appearance:textfield; }
  .num[type=number]::-webkit-outer-spin-button, .num[type=number]::-webkit-inner-spin-button { -webkit-appearance:none; margin:0; }
  .reset { background:none; border:1px solid var(--border-button); border-radius:var(--radius); color:var(--text-dim); font-size:13px; cursor:pointer; padding:4px 7px; flex-shrink:0; }
  .reset:hover:not(:disabled) { border-color:var(--accent); color:var(--accent); }
  .reset:disabled { opacity:.35; cursor:not-allowed; }

  .grid4 { display:grid; grid-template-columns:repeat(4,1fr); gap:6px; }
  .costcell { display:flex; flex-direction:column; gap:3px; }
  .costlbl { font-size:10px; color:var(--text-dim); }

  /* One consistent foldable card for Advanced and each Model. */
  .fold { border:1px solid var(--border); border-radius:var(--radius); margin-bottom:8px; overflow:hidden; }
  .fold.open { border-color:var(--border-strong); }
  .fold-head { display:flex; align-items:center; gap:8px; width:100%; padding:10px 12px; background:var(--bg); border:0; color:var(--text); font-family:var(--font-ui); font-size:13px; cursor:pointer; text-align:left; }
  .fold-label { flex:1; min-width:0; display:flex; flex-direction:column; }
  .fl-name { overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
  .fl-id { font-size:11px; color:var(--text-dim); overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
  .fold-body { padding:12px; border-top:1px solid var(--border); }
  .caret { color:var(--text-dim); font-size:13px; transition:transform .12s; flex-shrink:0; }
  .caret.open { transform:rotate(90deg); }
  .mfoot { display:flex; align-items:center; justify-content:space-between; margin-top:6px; }

  .addrow { display:flex; margin-top:10px; }
  .primary { padding:5px 11px; border:1px solid var(--border-button); border-radius:var(--radius); background:none; color:var(--text); font-family:var(--font-ui); font-size:12px; cursor:pointer; }
  .primary:hover:not(:disabled) { border-color:var(--accent); color:var(--accent); }
  .primary:disabled { opacity:.45; cursor:not-allowed; }

  .addbox { border:1px solid var(--border); border-radius:var(--radius); padding:12px; margin-bottom:10px; background:var(--bg); }
  .actions { display:flex; gap:8px; justify-content:flex-end; }
  .ghost { padding:5px 11px; border:1px solid var(--border-button); border-radius:var(--radius); background:none; color:var(--text-dim); font-family:var(--font-ui); font-size:12px; cursor:pointer; }
  .ghost:hover:not(:disabled) { border-color:var(--accent); color:var(--text); }
  .ghost.danger:hover:not(:disabled) { border-color:var(--error); color:var(--error); }
  .ghost:disabled { opacity:.45; cursor:not-allowed; }

  .chips { display:flex; flex-wrap:wrap; gap:6px; }
  .chip { display:inline-flex; align-items:center; gap:5px; padding:4px 9px; border:1px solid var(--border-button); border-radius:12px; font-size:12px; color:var(--text-dim); cursor:pointer; }
  .chip.on { border-color:var(--accent); color:var(--accent); }
  .chip input { position:absolute; opacity:0; width:0; height:0; }
</style>
