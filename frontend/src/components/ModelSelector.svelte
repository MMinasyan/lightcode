<script>
  import { ModelList, SwitchModel } from '../../wailsjs/go/main/App';
  import { createEventDispatcher, onMount } from 'svelte';
  export let currentRef = '';
  const dispatch = createEventDispatcher();
  let entries = [];
  let loading = true;

  onMount(async () => {
    try { entries = await ModelList(); } catch (e) { console.error(e); }
    loading = false;
  });

  $: groups = groupByProvider(entries || []);

  function groupByProvider(list) {
    const grouped = [];
    const byProvider = new Map();
    for (const entry of list) {
      const provider = entry.provider || '';
      if (!byProvider.has(provider)) {
        const group = { provider, providerName: entry.providerName || provider, models: [] };
        byProvider.set(provider, group);
        grouped.push(group);
      }
      byProvider.get(provider).models.push(entry);
    }
    return grouped;
  }

  async function select(entry) {
    try { await SwitchModel(entry.ref); dispatch('switched', { ref: entry.ref }); }
    catch (e) { console.error(e); }
  }

  function isActive(entry) { return entry.ref === currentRef; }
  function displayName(entry) { return entry.displayName || entry.model || entry.ref; }
</script>

<!-- svelte-ignore a11y-click-events-have-key-events -->
<div class="backdrop" on:click={() => dispatch('close')}>
  <!-- svelte-ignore a11y-click-events-have-key-events -->
  <div class="selector" on:click|stopPropagation>
    <div class="hdr">Select Model</div>
    {#if loading}<div class="loading">Loading...</div>
    {:else}
      {#each groups as group}
        <div class="group">
          <div class="pname">{group.providerName}</div>
          {#each group.models as entry}
            <button class="item" class:active={isActive(entry)} on:click={() => select(entry)}>
              {#if isActive(entry)}<span class="chk">&#x2713;</span>{:else}<span class="chk"></span>{/if}
              <span class="label">{displayName(entry)}</span>
              <span class="ref">{entry.ref}</span>
            </button>
          {/each}
        </div>
      {/each}
    {/if}
  </div>
</div>

<style>
  .backdrop { position:fixed; inset:0; z-index:100; }
  .selector { position:absolute; bottom:60px; left:12px; background:var(--bg-elevated); border:1px solid var(--border-strong); border-radius:var(--radius); min-width:320px; max-height:400px; overflow-y:auto; box-shadow:var(--shadow-lg); }
  .hdr { padding:8px 12px; font-size:12px; color:var(--text-dim); text-transform:uppercase; letter-spacing:.5px; border-bottom:1px solid var(--border); }
  .loading { padding:12px; color:var(--text-dim); font-size:13px; }
  .group { padding:4px 0; border-bottom:1px solid var(--border); }
  .group:last-child { border-bottom:none; }
  .pname { padding:4px 12px; font-size:11px; color:var(--text-dim); text-transform:uppercase; letter-spacing:.5px; }
  .item { display:grid; grid-template-columns:14px 1fr; grid-template-rows:auto auto; align-items:center; column-gap:8px; width:100%; padding:6px 12px; background:none; border:none; color:var(--text); font-family:var(--font-ui); font-size:13px; cursor:pointer; text-align:left; }
  .item:hover { background:var(--accent-soft); }
  .item.active { color:var(--accent); }
  .chk { width:14px; font-size:12px; color:var(--accent); grid-row:1 / span 2; }
  .label { overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
  .ref { color:var(--text-dim); font-size:11px; overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
</style>
