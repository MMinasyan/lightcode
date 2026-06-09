<script>
  import { createEventDispatcher } from 'svelte';
  import { groupByProvider } from '../lib/format.js';
  export let value = '';
  export let models = []; // [{ ref, provider, providerName, displayName, model }]
  export let placeholder = 'Select model';
  export let defaultLabel = ''; // when set, prepends an item with value '' (e.g. "Same as default")
  const dispatch = createEventDispatcher();
  let open = false;
  let query = '';
  let btn;
  let menuStyle = '';

  $: current = models.find((m) => m.ref === value);
  $: triggerLabel = value && current ? `${current.providerName} / ${current.displayName || current.model}` : (defaultLabel || placeholder);
  $: filtered = filterModels(models, query);
  $: groups = groupByProvider(filtered);

  function filterModels(list, q) {
    if (!q.trim()) return list;
    const lq = q.toLowerCase();
    return list.filter((m) => `${m.providerName} ${m.displayName || ''} ${m.model || ''} ${m.ref}`.toLowerCase().includes(lq));
  }
  function toggle() {
    if (!open) {
      const r = btn.getBoundingClientRect();
      const w = Math.max(r.width, 300);
      const below = window.innerHeight - r.bottom;
      menuStyle = below < 320 && r.top > below
        ? `bottom:${window.innerHeight - r.top + 4}px; left:${r.left}px; width:${w}px;`
        : `top:${r.bottom + 4}px; left:${r.left}px; width:${w}px;`;
      query = '';
    }
    open = !open;
  }
  function pick(ref) { value = ref; open = false; dispatch('change', ref); }
</script>

<div class="mp">
  <button bind:this={btn} type="button" class="mp-btn" class:open on:click={toggle}>
    <span class="mp-val" class:placeholder={!value && !defaultLabel}>{triggerLabel}</span>
    <span class="mp-caret">▾</span>
  </button>
  {#if open}
    <button type="button" class="mp-backdrop" tabindex="-1" aria-label="Close" on:click={() => (open = false)}></button>
    <div class="selector" style={menuStyle} role="dialog" aria-label="Select model">
      <div class="hdr"><span>Select model</span></div>
      <input class="search" type="text" placeholder="Search models..." bind:value={query} />
      <div class="list">
        {#if defaultLabel && !query.trim()}
          <div class="group">
            <button class="item" class:active={value === ''} on:click={() => pick('')}>
              {#if value === ''}<span class="chk">&#x2713;</span>{:else}<span class="chk"></span>{/if}
              <span class="label">{defaultLabel}</span>
            </button>
          </div>
        {/if}
        {#each groups as group}
          <div class="group">
            <div class="pname">{group.providerName}</div>
            {#each group.models as m}
              <button class="item" class:active={m.ref === value} on:click={() => pick(m.ref)}>
                {#if m.ref === value}<span class="chk">&#x2713;</span>{:else}<span class="chk"></span>{/if}
                <span class="label">{m.displayName || m.model}</span>
              </button>
            {/each}
          </div>
        {/each}
        {#if groups.length === 0}<div class="loading">No models</div>{/if}
      </div>
    </div>
  {/if}
</div>

<style>
  .mp { position:relative; }
  .mp-btn { display:flex; align-items:center; justify-content:space-between; gap:8px; width:100%; padding:7px 10px; border:1px solid var(--border-button); border-radius:var(--radius); background:var(--bg-input); color:var(--text); font-family:var(--font-ui); font-size:13px; cursor:pointer; text-align:left; }
  .mp-btn:hover, .mp-btn.open { border-color:var(--accent); }
  .mp-val { overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
  .mp-val.placeholder { color:var(--text-dim); }
  .mp-caret { color:var(--text-dim); font-size:11px; flex-shrink:0; }
  .mp-backdrop { position:fixed; inset:0; z-index:400; border:0; background:transparent; cursor:default; }

  /* menu — matches the in-chat ModelSelector exactly */
  .selector { position:fixed; z-index:401; background:var(--bg-elevated); border:1px solid var(--border-strong); border-radius:var(--radius); max-height:400px; overflow:hidden; box-shadow:var(--shadow-lg); display:flex; flex-direction:column; }
  .hdr { display:flex; justify-content:space-between; align-items:center; padding:8px 12px; font-size:12px; color:var(--text-dim); text-transform:uppercase; letter-spacing:.5px; border-bottom:1px solid var(--border); flex-shrink:0; }
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
