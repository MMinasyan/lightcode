<script>
  import { createEventDispatcher } from 'svelte';
  export let value = {}; // object map string -> string
  export let keyPlaceholder = 'key';
  export let valuePlaceholder = 'value';
  const dispatch = createEventDispatcher();

  let rows = [];
  let valueKey = '';
  let focused = false;
  let dirty = false;

  function rowsFromValue(v) {
    return Object.entries(v || {}).map(([k, v]) => ({ k, v: String(v) }));
  }

  function serialize(v) {
    return JSON.stringify(Object.entries(v || {}).sort(([a], [b]) => a.localeCompare(b)));
  }

  $: {
    const nextKey = serialize(value);
    if (nextKey !== valueKey && !focused) {
      rows = rowsFromValue(value);
      dirty = false;
      valueKey = nextKey;
    }
  }

  function emit() {
    dirty = true;
    const obj = {};
    for (const r of rows) { const k = (r.k || '').trim(); if (k) obj[k] = r.v; }
    dispatch('change', obj);
  }
  function add() { dirty = true; rows = [...rows, { k: '', v: '' }]; }
  function removeRow(i) { rows = rows.filter((_, j) => j !== i); emit(); }
  function onFocus() { focused = true; }
  function onBlur() {
    focused = false;
    if (!dirty) return;
    const nextKey = serialize(value);
    if (nextKey !== valueKey) {
      rows = rowsFromValue(value);
      dirty = false;
      valueKey = nextKey;
    }
  }
</script>

<div class="kv">
  {#each rows as row, i}
    <div class="kv-row">
      <input class="kv-in" placeholder={keyPlaceholder} bind:value={row.k} on:focus={onFocus} on:blur={onBlur} on:change={emit} />
      <input class="kv-in" placeholder={valuePlaceholder} bind:value={row.v} on:focus={onFocus} on:blur={onBlur} on:change={emit} />
      <button class="kv-x" on:click={() => removeRow(i)} aria-label="Remove">&#x2715;</button>
    </div>
  {/each}
  <button class="kv-add" on:click={add}>+ Add</button>
</div>

<style>
  .kv { display:flex; flex-direction:column; gap:6px; }
  .kv-row { display:grid; grid-template-columns:1fr 1fr 26px; gap:6px; align-items:center; }
  .kv-in { padding:6px 8px; border:1px solid var(--border-button); border-radius:var(--radius); background:var(--bg-input); color:var(--text); font-family:var(--font-ui); font-size:12px; outline:none; }
  .kv-in:focus { border-color:var(--accent); background:var(--bg-input-focus); }
  .kv-x { background:none; border:0; color:var(--text-dim); font-size:12px; cursor:pointer; padding:4px; border-radius:var(--radius); }
  .kv-x:hover { color:var(--error); }
  .kv-add { align-self:flex-start; background:none; border:1px solid var(--border-button); border-radius:var(--radius); color:var(--text-dim); font-family:var(--font-ui); font-size:12px; padding:4px 10px; cursor:pointer; }
  .kv-add:hover { border-color:var(--accent); color:var(--text); }
</style>
