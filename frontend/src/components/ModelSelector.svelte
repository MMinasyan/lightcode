<script>
  import { ModelList, SwitchModel } from '../../wailsjs/go/main/App';
  import { createEventDispatcher, onMount } from 'svelte';
  export let currentProvider = '';
  export let currentModel = '';
  const dispatch = createEventDispatcher();
  let providers = [];
  let loading = true;

  onMount(async () => {
    try { providers = await ModelList(); } catch (e) { console.error(e); }
    loading = false;
  });

  async function select(prov, model) {
    try { await SwitchModel(prov, model); dispatch('switched', { provider: prov, model }); }
    catch (e) { console.error(e); }
  }

  function isActive(p, m) { return p === currentProvider && m === currentModel; }
</script>

<!-- svelte-ignore a11y-click-events-have-key-events -->
<div class="backdrop" on:click={() => dispatch('close')}>
  <!-- svelte-ignore a11y-click-events-have-key-events -->
  <div class="selector" on:click|stopPropagation>
    <div class="hdr">Select Model</div>
    {#if loading}<div class="loading">Loading...</div>
    {:else}
      {#each providers as p}
        <div class="group">
          <div class="pname">{p.provider}</div>
          {#each p.models as m}
            <button class="item" class:active={isActive(p.provider, m)} on:click={() => select(p.provider, m)}>
              {#if isActive(p.provider, m)}<span class="chk">&#x2713;</span>{:else}<span class="chk"></span>{/if}
              {m}
            </button>
          {/each}
        </div>
      {/each}
    {/if}
  </div>
</div>

<style>
  .backdrop { position:fixed; inset:0; z-index:100; }
  .selector { position:absolute; bottom:60px; left:12px; background:var(--bg-elevated); border:1px solid var(--border-strong); border-radius:var(--radius); min-width:280px; max-height:400px; overflow-y:auto; box-shadow:var(--shadow-lg); }
  .hdr { padding:8px 12px; font-size:12px; color:var(--text-dim); text-transform:uppercase; letter-spacing:.5px; border-bottom:1px solid var(--border); }
  .loading { padding:12px; color:var(--text-dim); font-size:13px; }
  .group { padding:4px 0; border-bottom:1px solid var(--border); }
  .group:last-child { border-bottom:none; }
  .pname { padding:4px 12px; font-size:11px; color:var(--text-dim); text-transform:uppercase; letter-spacing:.5px; }
  .item { display:flex; align-items:center; gap:8px; width:100%; padding:6px 12px; background:none; border:none; color:var(--text); font-family:var(--font-ui); font-size:13px; cursor:pointer; text-align:left; }
  .item:hover { background:var(--accent-soft); }
  .item.active { color:var(--accent); }
  .chk { width:14px; font-size:12px; color:var(--accent); }
</style>
