<script>
  import { createEventDispatcher } from 'svelte';
  export let value = '';
  export let options = []; // [{ value, label }]
  export let placeholder = 'Select...';
  const dispatch = createEventDispatcher();
  let open = false;
  let btn;
  let menuStyle = '';
  $: current = options.find((o) => o.value === value);

  function toggle() {
    if (!open) {
      // Fixed positioning so the menu escapes scrollable/overflow ancestors.
      const r = btn.getBoundingClientRect();
      const below = window.innerHeight - r.bottom;
      const top = below < 240 && r.top > below ? `bottom:${window.innerHeight - r.top + 4}px` : `top:${r.bottom + 4}px`;
      menuStyle = `${top}; left:${r.left}px; width:${r.width}px;`;
    }
    open = !open;
  }
  function pick(v) { value = v; open = false; dispatch('change', v); }
</script>

<div class="dd">
  <button bind:this={btn} type="button" class="dd-btn" class:open on:click={toggle}>
    <span class="dd-val" class:placeholder={!current}>{current ? current.label : placeholder}</span>
    <span class="dd-caret">▾</span>
  </button>
  {#if open}
    <button type="button" class="dd-backdrop" tabindex="-1" aria-label="Close" on:click={() => (open = false)}></button>
    <div class="dd-menu" style={menuStyle}>
      {#each options as o}
        <button type="button" class="dd-opt" class:active={o.value === value} on:click={() => pick(o.value)}>{o.label}</button>
      {/each}
      {#if options.length === 0}<div class="dd-empty">No options</div>{/if}
    </div>
  {/if}
</div>

<style>
  .dd { position:relative; }
  .dd-btn { display:flex; align-items:center; justify-content:space-between; gap:8px; width:100%; padding:7px 10px; border:1px solid var(--border-button); border-radius:var(--radius); background:var(--bg-input); color:var(--text); font-family:var(--font-ui); font-size:13px; cursor:pointer; text-align:left; }
  .dd-btn:hover, .dd-btn.open { border-color:var(--accent); }
  .dd-val { overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
  .dd-val.placeholder { color:var(--text-dim); }
  .dd-caret { color:var(--text-dim); font-size:11px; flex-shrink:0; }
  .dd-backdrop { position:fixed; inset:0; z-index:400; border:0; background:transparent; cursor:default; }
  .dd-menu { position:fixed; z-index:401; max-height:220px; overflow-y:auto; background:var(--bg-elevated); border:1px solid var(--border-strong); border-radius:var(--radius); box-shadow:var(--shadow-menu); padding:4px; }
  .dd-opt { display:block; width:100%; padding:6px 8px; border:0; border-radius:4px; background:none; color:var(--text); font-family:var(--font-ui); font-size:13px; text-align:left; cursor:pointer; overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
  .dd-opt:hover { background:var(--accent-soft); }
  .dd-opt.active { color:var(--accent); }
  .dd-empty { padding:6px 8px; color:var(--text-dim); font-size:13px; }
</style>
