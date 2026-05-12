<script>
  import { createEventDispatcher, onMount } from 'svelte';
  import { settings } from '../lib/settings.js';
  import { AllModelList, SetModelHidden, SetProviderHidden } from '../../wailsjs/go/main/App';
  const dispatch = createEventDispatcher();
  export let initialSection = 'appearance';

  const sections = [
    { id: 'appearance', label: 'Appearance' },
    { id: 'models', label: 'Models' },
  ];
  let active = initialSection;

  let prevInitial = initialSection;
  $: if (initialSection !== prevInitial) { active = initialSection; prevInitial = initialSection; }

  function toggleWrap(e) {
    settings.update((s) => ({ ...s, wrapCode: e.target.checked }));
  }

  function setFontScale(e) {
    const n = parseInt(e.target.value, 10);
    if (!Number.isFinite(n)) return;
    const clamped = Math.max(50, Math.min(200, n));
    settings.update((s) => ({ ...s, fontScale: clamped }));
  }

  function stepFontScale(delta) {
    settings.update((s) => {
      const next = Math.max(50, Math.min(200, (s.fontScale || 100) + delta));
      return { ...s, fontScale: next };
    });
  }

  let allModels = [];
  let modelGroups = [];
  let modelQuery = '';

  $: filteredGroups = filterModelGroups(modelGroups, modelQuery);

  function filterModelGroups(groups, q) {
    if (!q.trim()) return groups;
    const lq = q.toLowerCase();
    return groups.map(g => {
      const models = g.models.filter(e =>
        (e.displayName || '').toLowerCase().includes(lq) ||
        (e.model || '').toLowerCase().includes(lq) ||
        (e.ref || '').toLowerCase().includes(lq)
      );
      return { ...g, models };
    }).filter(g => g.models.length > 0);
  }

  onMount(async () => {
    await refreshModels();
  });

  async function refreshModels() {
    try { allModels = await AllModelList(); } catch (e) { console.error(e); allModels = []; }
    modelGroups = groupByProvider(allModels);
  }

  function groupByProvider(list) {
    const grouped = [];
    const byProvider = new Map();
    for (const entry of list) {
      if (!byProvider.has(entry.provider)) {
        const group = { provider: entry.provider, providerName: entry.providerName || entry.provider, providerHidden: entry.providerHidden, models: [] };
        byProvider.set(entry.provider, group);
        grouped.push(group);
      }
      byProvider.get(entry.provider).models.push(entry);
    }
    return grouped;
  }

  function modelDisplayName(entry) {
    return entry.displayName || entry.model || entry.ref;
  }

  async function toggleModel(entry, e) {
    const hidden = !e.target.checked;
    try {
      await SetModelHidden(entry.ref, hidden);
      await refreshModels();
    } catch (err) { console.error(err); }
  }

  async function toggleProvider(group, e) {
    const hidden = !e.target.checked;
    try {
      await SetProviderHidden(group.provider, hidden);
      await refreshModels();
    } catch (err) { console.error(err); }
  }
</script>

<!-- svelte-ignore a11y-click-events-have-key-events -->
<div class="backdrop" on:click={() => dispatch('close')}>
  <!-- svelte-ignore a11y-click-events-have-key-events -->
  <div class="prompt" on:click|stopPropagation>
    <div class="hdr">Settings</div>
    <div class="body">
      <div class="sidebar">
        {#each sections as s}
          <button class="nav-item" class:active={active === s.id} on:click={() => active = s.id}>{s.label}</button>
        {/each}
      </div>
      <div class="content">
        {#if active === 'appearance'}
          <div class="section-title">Appearance</div>
          <label class="option">
            <span>Wrap code lines</span>
            <span class="switch">
              <input type="checkbox" checked={$settings.wrapCode} on:change={toggleWrap} />
              <span class="track"><span class="thumb"></span></span>
            </span>
          </label>
          <div class="option">
            <span>Message font scale (%)</span>
            <span class="stepper">
              <button type="button" class="step" on:click={() => stepFontScale(-10)} disabled={$settings.fontScale <= 50}>−</button>
              <input type="number" min="50" max="200" step="10" value={$settings.fontScale} on:change={setFontScale} class="num" />
              <button type="button" class="step" on:click={() => stepFontScale(10)} disabled={$settings.fontScale >= 200}>+</button>
            </span>
          </div>
        {/if}
        {#if active === 'models'}
          <div class="section-title">Models</div>
          <p class="placeholder">Toggle model visibility in the model selector. Hidden models are still available by ref.</p>
          <input class="model-search" type="text" placeholder="Filter models..." bind:value={modelQuery} />
          {#each filteredGroups as group (group.provider)}
            <div class="models-group">
              <label class="option provider-row">
                <span>{group.providerName}</span>
                <span class="switch">
                  <input type="checkbox" checked={!group.providerHidden} on:change={(e) => toggleProvider(group, e)} />
                  <span class="track"><span class="thumb"></span></span>
                </span>
              </label>
              {#each group.models as entry (entry.ref)}
                <label class="option model-row" class:dimmed={group.providerHidden}>
                  <span>{modelDisplayName(entry)}</span>
                  <span class="switch">
                    <input type="checkbox" checked={!entry.hidden} disabled={group.providerHidden} on:change={(e) => toggleModel(entry, e)} />
                    <span class="track"><span class="thumb"></span></span>
                  </span>
                </label>
              {/each}
            </div>
          {/each}
        {/if}
      </div>
    </div>
    <div class="actions">
      <button class="btn" on:click={() => dispatch('close')}>Close</button>
    </div>
  </div>
</div>

<style>
  .backdrop { position:fixed; inset:0; background:var(--overlay); z-index:300; display:flex; align-items:center; justify-content:center; }
  .prompt { background:var(--bg-elevated); border:1px solid var(--border-strong); min-width:560px; max-width:720px; height:88vh; max-height:88vh; display:flex; flex-direction:column; }
  .hdr { padding:8px 12px; font-size:12px; font-weight:600; text-transform:uppercase; letter-spacing:.5px; border-bottom:1px solid var(--border); }
  .body { display:flex; flex:1; min-height:0; }
  .sidebar { width:140px; border-right:1px solid var(--border); padding:8px 0; display:flex; flex-direction:column; overflow-y:auto; }
  .nav-item { background:none; border:none; color:var(--text-dim); font-family:var(--font-ui); font-size:12px; padding:6px 12px; cursor:pointer; text-align:left; }
  .nav-item:hover { color:var(--text); }
  .nav-item.active { color:var(--accent); background:var(--accent-soft); }
  .content { flex:1; padding:12px; overflow-y:auto; }
  .section-title { font-size:12px; font-weight:600; text-transform:uppercase; letter-spacing:.5px; color:var(--text); margin-bottom:8px; }
  .placeholder { font-family:var(--font-ui); font-size:12px; color:var(--text-dim); margin-bottom:8px; }
  .model-search { width:100%; padding:6px 12px; background:var(--bg-input); border:none; border-bottom:1px solid var(--border); color:var(--text); font-family:var(--font-ui); font-size:12px; outline:none; box-sizing:border-box; margin-bottom:8px; }
  .model-search::placeholder { color:var(--text-dim); }
  .model-search:focus { background:var(--bg-input-focus); }
  .option { display:flex; align-items:center; justify-content:space-between; font-family:var(--font-ui); font-size:12px; color:var(--text); cursor:pointer; padding:6px 0; min-height:28px; }
  .option + .option { border-top:1px solid var(--border); }
  .models-group + .models-group { margin-top:12px; }
  .provider-row { color:var(--text); font-weight:600; border-top:1px solid var(--border); }
  .model-row { padding-left:16px; }
  .model-row.dimmed { opacity:.4; }
  .switch { position:relative; display:inline-block; width:32px; height:18px; flex-shrink:0; }
  .switch input { position:absolute; inset:0; opacity:0; cursor:pointer; margin:0; }
  .switch .track { position:absolute; inset:0; background:var(--bg-input); border:1px solid var(--border-button); border-radius:999px; transition:background .15s, border-color .15s; }
  .switch .thumb { position:absolute; top:2px; left:2px; width:12px; height:12px; background:var(--text-dim); border-radius:50%; transition:transform .15s, background .15s; }
  .switch input:hover + .track { border-color:var(--accent); }
  .switch input:disabled + .track { cursor:default; }
  .switch input:disabled:hover + .track { border-color:var(--border-button); }
  .switch input:checked + .track { background:var(--accent-soft); border-color:var(--accent); }
  .switch input:checked + .track .thumb { transform:translateX(14px); background:var(--accent); }
  .stepper { display:inline-flex; align-items:stretch; }
  .stepper .step { width:22px; background:transparent; border:1px solid var(--border-button); color:var(--text-dim); font-family:var(--font-ui); font-size:13px; line-height:1; cursor:pointer; padding:0; }
  .stepper .step:hover:not(:disabled) { border-color:var(--accent); color:var(--accent); }
  .stepper .step:disabled { opacity:.4; cursor:default; }
  .stepper .step:first-child { border-right:none; }
  .stepper .step:last-child { border-left:none; }
  .option .num { width:48px; background:transparent; border:1px solid var(--border-button); color:var(--text); font-family:var(--font-ui); font-size:12px; padding:2px 6px; text-align:center; appearance:textfield; -moz-appearance:textfield; }
  .option .num::-webkit-outer-spin-button, .option .num::-webkit-inner-spin-button { -webkit-appearance:none; margin:0; }
  .option .num:focus { outline:none; border-color:var(--accent); position:relative; z-index:1; }
  .actions { display:flex; gap:8px; padding:8px 12px; border-top:1px solid var(--border); justify-content:flex-end; }
  .btn { padding:4px 12px; font-size:12px; cursor:pointer; border:1px solid var(--border-button); background:none; color:var(--text-dim); font-family:var(--font-ui); }
  .btn:hover { border-color:var(--accent); color:var(--text); }
</style>
