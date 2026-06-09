<script>
  import { createEventDispatcher, onMount } from 'svelte';
  import { errorText } from '../lib/errors.js';
  import { groupByProvider } from '../lib/format.js';
  import { settings } from '../lib/settings.js';
  import ModelPicker from './ModelPicker.svelte';
  import AddProvider from './AddProvider.svelte';
  import ProviderConfig from './ProviderConfig.svelte';
  import {
    ProviderList, AllModelList, GetRuntimeConfig, SetRuntimeConfig,
    SetModelHidden, SetProviderHidden, SetDefaultModel,
    ConnectProvider, DisconnectProvider, RemoveProvider,
  } from '../../wailsjs/go/main/App';

  export let initialSection = 'providers';
  const dispatch = createEventDispatcher();

  const TABS = [
    { id: 'appearance', label: 'Appearance' },
    { id: 'providers', label: 'Providers' },
    { id: 'models', label: 'Models' },
    { id: 'agent', label: 'Agent' },
  ];
  let active = TABS.some((t) => t.id === initialSection) ? initialSection : 'providers';

  let providers = [];
  let models = [];
  let runtime = null;
  let loading = true;
  let showAdd = false;
  let connectTarget = null;
  let connectKey = '';
  let connectBusy = false;
  let savingRuntime = false;
  let runtimeSaveQueue = Promise.resolve();
  let runtimeSavesPending = 0;

  function fail(e) { dispatch('error', errorText(e)); }

  async function loadAll() {
    try {
      const [p, m] = await Promise.all([ProviderList(), AllModelList()]);
      providers = p || [];
      models = m || [];
      if (!runtime) runtime = await GetRuntimeConfig();
    } catch (e) { fail(e); }
  }

  onMount(async () => { loading = true; await loadAll(); loading = false; });

  let openProvider = null;
  function toggleFold(id) { openProvider = openProvider === id ? null : id; }

  $: managedProviders = providers.filter((p) => p.connected || !p.builtin);
  $: usableModels = models.filter((m) => !m.incomplete);
  $: modelGroups = groupByProvider(usableModels);
  $: configuredDefault = models.find((m) => m.default);
  $: currentDefault = (usableModels.find((m) => m.default) || {}).ref || '';
  $: unavailableDefault = configuredDefault && configuredDefault.incomplete ? configuredDefault.ref : '';

  async function disconnect(p) { try { await DisconnectProvider(p.id); } catch (e) { fail(e); } finally { await loadAll(); } }
  function openConnect(p) { connectTarget = p; connectKey = ''; }
  async function connectProvider() {
    if (!connectTarget || connectBusy) return;
    connectBusy = true;
    try { await ConnectProvider(connectTarget.id, connectKey.trim()); connectTarget = null; connectKey = ''; }
    catch (e) { fail(e); }
    finally { connectBusy = false; await loadAll(); }
  }
  async function remove(p) { try { await RemoveProvider(p.id); } catch (e) { fail(e); } finally { await loadAll(); } }
  async function toggleModel(entry) { try { await SetModelHidden(entry.ref, !entry.hidden); } catch (e) { fail(e); } finally { await loadAll(); } }
  async function toggleProvider(group) { try { await SetProviderHidden(group.provider, !group.providerHidden); } catch (e) { fail(e); } finally { await loadAll(); } }
  async function pickDefault(ref) { if (!ref) return; try { await SetDefaultModel(ref); } catch (e) { fail(e); } finally { await loadAll(); } }

  function cloneRuntimeConfig(value) {
    return JSON.parse(JSON.stringify(value));
  }
  function clampInt(value, min, max, fallback) {
    let n = Math.round(Number(value));
    if (!Number.isFinite(n)) n = fallback;
    return Math.max(min, Math.min(max, n));
  }
  async function saveRuntime() {
    if (!runtime) return;
    const snapshot = cloneRuntimeConfig(runtime);
    runtimeSavesPending += 1;
    savingRuntime = true;
    runtimeSaveQueue = runtimeSaveQueue.catch(() => {}).then(async () => {
      try { await SetRuntimeConfig(snapshot); }
      catch (e) { fail(e); }
      finally { runtime = await GetRuntimeConfig(); }
    }).finally(() => {
      runtimeSavesPending -= 1;
      savingRuntime = runtimeSavesPending > 0;
    });
    await runtimeSaveQueue;
  }
  async function setRuntimeInt(section, key, min, max, fallback, e) {
    runtime[section][key] = clampInt(e.target.value, min, max, fallback);
    await saveRuntime();
  }
  async function setThreshold(e) {
    runtime.compaction.threshold_pct = clampInt(e.target.value, 10, 99, 90) / 100;
    await saveRuntime();
  }
  async function setRuntimeModel(section, key, ref) {
    runtime[section][key] = ref;
    await saveRuntime();
  }

  async function onProviderAdded() { showAdd = false; await loadAll(); }
  function close() { dispatch('close'); }
</script>

<div class="layer">
  <button type="button" class="backdrop" tabindex="-1" aria-label="Close settings" on:click={close}></button>
  <div class="panel" role="dialog" aria-modal="true" aria-label="Settings" tabindex="-1">
    <nav class="sidebar">
      <div class="brand">Settings</div>
      {#each TABS as t}
        <button class="navitem" class:active={active === t.id} on:click={() => (active = t.id)}>{t.label}</button>
      {/each}
    </nav>

    <section class="main">
      <header class="bar">
        <span class="bar-title">{TABS.find((t) => t.id === active).label}</span>
        <button class="x" on:click={close} aria-label="Close">&#x2715;</button>
      </header>

      <div class="content">
        {#if loading}
          <div class="muted">Loading...</div>

        {:else if active === 'providers'}
          {#if managedProviders.length === 0}
            <div class="empty">No providers connected</div>
          {:else}
            <div class="cards">
              {#each managedProviders as p}
                <div class="pcard" class:open={openProvider === p.id}>
                  <div class="card">
                    <button class="card-main" on:click={() => toggleFold(p.id)}>
                      <span class="caret" class:open={openProvider === p.id}>›</span>
                      <span>
                        <span class="card-name">{p.name}</span>
                        <span class="card-sub">{p.connected ? `${p.usableModels} model${p.usableModels === 1 ? '' : 's'}` : 'Disconnected'}{p.builtin ? '' : ' (custom)'}</span>
                      </span>
                    </button>
                    <div class="card-actions">
                      {#if !p.connected && p.apiKeyEnv}<button class="ghost" on:click={() => openConnect(p)}>Connect</button>{/if}
                      {#if p.disconnectable}<button class="ghost" on:click={() => disconnect(p)}>Disconnect</button>{/if}
                      {#if p.removable}<button class="ghost danger" on:click={() => remove(p)}>Remove</button>{/if}
                    </div>
                  </div>
                  {#if openProvider === p.id}
                    <div class="pfold">
                      <ProviderConfig providerId={p.id} on:changed={loadAll} on:error={(e) => fail(e.detail)} />
                    </div>
                  {/if}
                </div>
              {/each}
            </div>
          {/if}
          <div class="addrow">
            <button class="primary" on:click={() => (showAdd = true)}>+ Add provider</button>
          </div>

        {:else if active === 'models'}
          {#if modelGroups.length === 0}
            <div class="empty">No models yet</div>
          {:else}
            {#each modelGroups as group}
              <div class="mgroup">
                <div class="mgroup-hdr">
                  <span class="mgroup-name">{group.providerName}</span>
                  <label class="switch">
                    <input type="checkbox" checked={!group.providerHidden} on:change={() => toggleProvider(group)} />
                    <span class="track"></span>
                  </label>
                </div>
                {#each group.models as entry}
                  <div class="mrow" class:dim={group.providerHidden}>
                    <span class="mname">{entry.displayName || entry.model}</span>
                    <label class="switch">
                      <input type="checkbox" checked={!entry.hidden} disabled={group.providerHidden} on:change={() => toggleModel(entry)} />
                      <span class="track"></span>
                    </label>
                  </div>
                {/each}
              </div>
            {/each}
          {/if}

        {:else if active === 'agent'}
          <div class="field">
            <span class="flabel">Default model</span>
            <div class="stack">
              <ModelPicker value={currentDefault} models={usableModels} placeholder="Select a model" on:change={(e) => pickDefault(e.detail)} />
              {#if unavailableDefault}<span class="hint warn">Configured default is unavailable: {unavailableDefault}</span>{/if}
            </div>
          </div>

          {#if runtime}
            <div class="sec">Sessions</div>
            <label class="field"><span class="flabel">Archive after (days)</span><input type="number" min="1" max="365" disabled={savingRuntime} value={runtime.sessions.archive_after_days} on:change={(e) => setRuntimeInt('sessions', 'archive_after_days', 1, 365, 30, e)} /></label>
            <label class="field"><span class="flabel">Delete after archive (days)</span><input type="number" min="1" max="365" disabled={savingRuntime} value={runtime.sessions.delete_after_archive_days} on:change={(e) => setRuntimeInt('sessions', 'delete_after_archive_days', 1, 365, 90, e)} /></label>

            <div class="sec">Compaction</div>
            <label class="field"><span class="flabel">Threshold (%)</span><input type="number" min="10" max="99" disabled={savingRuntime} value={Math.round(runtime.compaction.threshold_pct * 100)} on:change={setThreshold} /></label>
            <div class="field"><span class="flabel">Summarizer model</span><ModelPicker value={runtime.compaction.summarizer_model} models={usableModels} defaultLabel="Same as default" on:change={(e) => setRuntimeModel('compaction', 'summarizer_model', e.detail)} /></div>

            <div class="sec">Subagents</div>
            <label class="field"><span class="flabel">Max concurrent</span><input type="number" min="1" max="20" disabled={savingRuntime} value={runtime.subagents.max_concurrent} on:change={(e) => setRuntimeInt('subagents', 'max_concurrent', 1, 20, 4, e)} /></label>
            <div class="field"><span class="flabel">Subagent model</span><ModelPicker value={runtime.subagents.model} models={usableModels} defaultLabel="Same as default" on:change={(e) => setRuntimeModel('subagents', 'model', e.detail)} /></div>

            <div class="sec">Tools</div>
            <label class="field"><span class="flabel">Max output (bytes)</span><input type="number" min="1024" max="1048576" disabled={savingRuntime} value={runtime.tools.max_output_bytes} on:change={(e) => setRuntimeInt('tools', 'max_output_bytes', 1024, 1048576, 65536, e)} /></label>
            <label class="field"><span class="flabel">Read max lines</span><input type="number" min="10" max="10000" disabled={savingRuntime} value={runtime.tools.read_max_lines} on:change={(e) => setRuntimeInt('tools', 'read_max_lines', 10, 10000, 500, e)} /></label>
            <label class="field"><span class="flabel">Read line max chars</span><input type="number" min="100" max="100000" disabled={savingRuntime} value={runtime.tools.read_line_max_chars} on:change={(e) => setRuntimeInt('tools', 'read_line_max_chars', 100, 100000, 2000, e)} /></label>
            <label class="field"><span class="flabel">Command timeout (s)</span><input type="number" min="5" max="600" disabled={savingRuntime} value={runtime.tools.command_timeout} on:change={(e) => setRuntimeInt('tools', 'command_timeout', 5, 600, 180, e)} /></label>
            <label class="field"><span class="flabel">Max background processes</span><input type="number" min="1" max="50" disabled={savingRuntime} value={runtime.tools.max_background_processes} on:change={(e) => setRuntimeInt('tools', 'max_background_processes', 1, 50, 10, e)} /></label>
          {/if}

        {:else if active === 'appearance'}
          <div class="mrow">
            <span class="mname">Wrap code blocks</span>
            <label class="switch">
              <input type="checkbox" checked={$settings.wrapCode} on:change={(e) => settings.update((s) => ({ ...s, wrapCode: e.target.checked }))} />
              <span class="track"></span>
            </label>
          </div>
          <label class="field"><span class="flabel">Font scale (%)</span>
            <input type="number" min="50" max="200" step="5" value={$settings.fontScale}
              on:change={(e) => settings.update((s) => ({ ...s, fontScale: Math.max(50, Math.min(200, Number(e.target.value) || 100)) }))} />
          </label>
        {/if}
      </div>
    </section>
  </div>
</div>

{#if showAdd}
  <AddProvider {providers} on:added={onProviderAdded} on:close={() => (showAdd = false)} on:error={(e) => fail(e.detail)} />
{/if}

{#if connectTarget}
  <div class="layer top">
    <button type="button" class="backdrop" tabindex="-1" aria-label="Close connect provider" on:click={() => (connectTarget = null)}></button>
    <div class="connect-prompt" role="dialog" aria-modal="true" aria-label="Connect provider">
      <header class="bar"><span class="bar-title">Connect {connectTarget.name}</span></header>
      <div class="connect-body">
        <label class="field"><span class="flabel">API key</span><input type="password" autocomplete="off" placeholder={connectTarget.apiKeyEnv} bind:value={connectKey} /></label>
        <div class="card-actions right">
          <button class="ghost" disabled={connectBusy} on:click={() => (connectTarget = null)}>Cancel</button>
          <button class="primary" disabled={connectBusy} on:click={connectProvider}>{connectBusy ? 'Connecting...' : 'Connect'}</button>
        </div>
      </div>
    </div>
  </div>
{/if}

<style>
  .layer { position:fixed; inset:0; z-index:300; display:flex; align-items:center; justify-content:center; }
  .layer.top { z-index:340; }
  .backdrop { position:absolute; inset:0; border:0; padding:0; margin:0; background:var(--overlay); cursor:default; }
  .connect-prompt { position:relative; z-index:1; width:420px; max-width:92vw; background:var(--bg-elevated); border:1px solid var(--border-strong); border-radius:var(--radius); box-shadow:var(--shadow-lg); overflow:hidden; }
  .connect-body { padding:16px; }
  .right { justify-content:flex-end; }
  .panel { position:relative; z-index:1; display:flex; width:720px; max-width:92vw; height:540px; max-height:88vh; background:var(--bg-elevated); border:1px solid var(--border-strong); border-radius:var(--radius); box-shadow:var(--shadow-lg); overflow:hidden; }

  .sidebar { flex:0 0 160px; display:flex; flex-direction:column; padding:12px 8px; gap:2px; background:var(--bg); border-right:1px solid var(--border); }
  .brand { padding:6px 10px 12px; font-size:11px; text-transform:uppercase; letter-spacing:.5px; color:var(--text-dim); }
  .navitem { text-align:left; padding:8px 10px; border:0; border-radius:var(--radius); background:none; color:var(--text-dim); font-family:var(--font-ui); font-size:13px; cursor:pointer; }
  .navitem:hover { background:var(--accent-soft); color:var(--text); }
  .navitem.active { background:var(--accent-soft); color:var(--accent); }

  .main { flex:1; display:flex; flex-direction:column; min-width:0; }
  .bar { display:flex; align-items:center; justify-content:space-between; padding:12px 16px; border-bottom:1px solid var(--border); flex-shrink:0; }
  .bar-title { font-size:13px; font-weight:600; }
  .x { background:none; border:0; color:var(--text-dim); font-size:13px; cursor:pointer; padding:2px 6px; border-radius:var(--radius); }
  .x:hover { color:var(--text); background:var(--accent-soft); }
  .content { padding:16px; overflow-y:auto; min-height:0; }

  .muted { color:var(--text-dim); font-size:13px; }
  .empty { color:var(--text-dim); font-size:13px; padding:32px 0; text-align:center; }
  .addrow { display:flex; margin-top:12px; }

  .primary { padding:6px 12px; border:1px solid var(--border-button); border-radius:var(--radius); background:none; color:var(--text); font-family:var(--font-ui); font-size:13px; cursor:pointer; white-space:nowrap; }
  .primary:hover { border-color:var(--accent); color:var(--accent); }

  .cards { display:flex; flex-direction:column; gap:8px; }
  .pcard { border:1px solid var(--border); border-radius:var(--radius); background:var(--bg); overflow:hidden; }
  .pcard.open { border-color:var(--border-strong); }
  .card { display:flex; align-items:center; justify-content:space-between; gap:12px; padding:12px 14px; }
  .card-main { display:flex; align-items:center; gap:10px; flex:1; min-width:0; background:none; border:0; padding:0; cursor:pointer; text-align:left; font-family:var(--font-ui); }
  .caret { color:var(--text-dim); font-size:14px; transition:transform .12s; flex-shrink:0; }
  .caret.open { transform:rotate(90deg); }
  .card-name { display:block; font-size:14px; color:var(--text); }
  .card-sub { display:block; font-size:12px; color:var(--text-dim); margin-top:2px; }
  .card-actions { display:flex; gap:6px; flex-shrink:0; }
  .pfold { border-top:1px solid var(--border); padding:10px 14px 14px; }
  .ghost { padding:5px 10px; border:1px solid var(--border-button); border-radius:var(--radius); background:none; color:var(--text-dim); font-family:var(--font-ui); font-size:12px; cursor:pointer; }
  .ghost:hover { border-color:var(--accent); color:var(--text); }
  .ghost.danger:hover { border-color:var(--error); color:var(--error); }

  .mgroup { margin-bottom:18px; }
  .mgroup-hdr { display:flex; align-items:center; justify-content:space-between; padding:0 0 6px; border-bottom:1px solid var(--border); margin-bottom:6px; }
  .mgroup-name { font-size:11px; text-transform:uppercase; letter-spacing:.5px; color:var(--text-dim); }
  .mrow { display:flex; align-items:center; justify-content:space-between; gap:12px; padding:7px 2px; }
  .mrow.dim { opacity:.45; }
  .mname { font-size:13px; color:var(--text); overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }

  .switch { position:relative; display:inline-flex; flex-shrink:0; cursor:pointer; }
  .switch input { position:absolute; opacity:0; width:0; height:0; }
  .track { width:34px; height:18px; border-radius:9px; background:var(--border-button); transition:background .12s; position:relative; }
  .track::after { content:''; position:absolute; top:2px; left:2px; width:14px; height:14px; border-radius:50%; background:var(--text-dim); transition:transform .12s, background .12s; }
  .switch input:checked + .track { background:var(--accent); }
  .switch input:checked + .track::after { transform:translateX(16px); background:#fff; }
  .switch input:disabled + .track { opacity:.4; }

  .field { display:grid; grid-template-columns:170px 1fr; align-items:center; gap:0 12px; margin-bottom:12px; }
  .flabel { font-size:12px; color:var(--text-dim); }
  .field input { width:100%; box-sizing:border-box; padding:7px 10px; border:1px solid var(--border-button); border-radius:var(--radius); background:var(--bg-input); color:var(--text); font-family:var(--font-ui); font-size:13px; outline:none; }
  .field input:focus { border-color:var(--accent); background:var(--bg-input-focus); }
  .field input:disabled { opacity:.55; cursor:not-allowed; }
  .stack { display:flex; flex-direction:column; gap:4px; min-width:0; }
  .hint { font-size:11px; color:var(--text-dim); }
  .hint.warn { color:var(--error); }
  .field input[type=number] { -moz-appearance:textfield; appearance:textfield; }
  .field input[type=number]::-webkit-outer-spin-button,
  .field input[type=number]::-webkit-inner-spin-button { -webkit-appearance:none; margin:0; }
  .sec { font-size:11px; text-transform:uppercase; letter-spacing:.5px; color:var(--text-dim); margin:4px 0 12px; padding-top:10px; border-top:1px solid var(--border); }
</style>
