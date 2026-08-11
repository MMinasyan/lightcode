<script>
  import { ModelList } from '../../wailsjs/go/main/App';
  import { createEventDispatcher, onMount } from 'svelte';
  import { errorText } from '../lib/errors.js';
  import { groupByProvider } from '../lib/format.js';
  export let currentRef = '';
  const dispatch = createEventDispatcher();
  let entries = [];
  let loading = true;
  let query = '';

  onMount(async () => {
    try { entries = await ModelList(); } catch (e) { dispatch('error', errorText(e)); }
    loading = false;
  });

  $: filtered = filterEntries((entries || []).filter(e => !e.incomplete), query);
  $: groups = groupByProvider(filtered);

  function filterEntries(list, q) {
    if (!q.trim()) return list;
    const lq = q.toLowerCase();
    return list.filter(e =>
      (e.displayName || '').toLowerCase().includes(lq) ||
      (e.provider || '').toLowerCase().includes(lq) ||
      (e.model || '').toLowerCase().includes(lq) ||
      (e.ref || '').toLowerCase().includes(lq)
    );
  }

  // The selector only names the choice; App.svelte invokes the switch against
  // the session/generation captured at click time and closes the picker or
  // shows the error only while that view is still presented.
  function select(entry) {
    dispatch('select', { ref: entry.ref, displayName: entry.displayName || entry.model || entry.ref });
  }

  function isActive(entry) { return entry.ref === currentRef; }
  function displayName(entry) { return entry.displayName || entry.model || entry.ref; }
</script>

<div class="layer">
  <button type="button" class="backdrop" tabindex="-1" aria-label="Close model selector" on:click={() => dispatch('close')}></button>
  <div class="selector" role="dialog" aria-modal="true" aria-labelledby="model-selector-title" tabindex="-1">
    <div class="hdr"><span id="model-selector-title">Select Model</span><button class="manage" on:click={() => dispatch('manageSettings')}>Models</button></div>
    <input class="search" type="text" placeholder="Search models..." bind:value={query} />
    <div class="list">
      {#if loading}<div class="loading">Loading...</div>
      {:else}
        {#each groups as group}
          <div class="group">
            <div class="pname">{group.providerName}</div>
            {#each group.models as entry}
              <button class="item" class:active={isActive(entry)} on:click={() => select(entry)}>
                {#if isActive(entry)}<span class="chk">&#x2713;</span>{:else}<span class="chk"></span>{/if}
                <span class="label">{displayName(entry)}</span>
              </button>
            {/each}
          </div>
        {/each}
      {/if}
    </div>
  </div>
</div>

<style>
  .layer { position:fixed; inset:0; z-index:100; }
  .backdrop { position:absolute; inset:0; border:0; padding:0; margin:0; background:transparent; cursor:default; }
  .selector { position:absolute; z-index:1; bottom:60px; left:12px; background:var(--bg-elevated); border:1px solid var(--border-strong); border-radius:var(--radius); min-width:320px; max-height:400px; overflow:hidden; box-shadow:var(--shadow-lg); display:flex; flex-direction:column; }
  .hdr { display:flex; justify-content:space-between; align-items:center; padding:8px 12px; font-size:12px; color:var(--text-dim); text-transform:uppercase; letter-spacing:.5px; border-bottom:1px solid var(--border); flex-shrink:0; }
  .manage { background:none; border:none; padding:0; font-family:var(--font-ui); font-size:12px; color:var(--text-dim); text-transform:none; letter-spacing:normal; cursor:pointer; }
  .manage:hover { color:var(--accent); }
  .search { width:100%; padding:6px 12px; background:var(--bg-input); border:none; border-bottom:1px solid var(--border); color:var(--text); font-family:var(--font-ui); font-size:13px; outline:none; box-sizing:border-box; flex-shrink:0; }
  .search::placeholder { color:var(--text-dim); }
  .search:focus { background:var(--bg-input-focus); }
  .list { overflow-y:auto; min-height:0; }
  .loading { padding:12px; color:var(--text-dim); font-size:13px; }
  .group { padding:0 0 4px; border-bottom:1px solid var(--border); }
  .group:last-child { border-bottom:none; }
  .pname { padding:4px 12px; font-size:11px; color:var(--text-dim); text-transform:uppercase; letter-spacing:.5px; position:sticky; top:0; background:var(--bg-elevated); z-index:1; }
  .item { display:grid; grid-template-columns:14px 1fr; align-items:center; column-gap:8px; width:100%; padding:6px 12px; background:none; border:none; color:var(--text); font-family:var(--font-ui); font-size:13px; cursor:pointer; text-align:left; }
  .item:hover { background:var(--accent-soft); }
  .item.active { color:var(--accent); }
  .chk { width:14px; font-size:12px; color:var(--accent); }
  .label { overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
</style>
