<script>
  import { createEventDispatcher } from 'svelte';
  export let modelName = '';
  const dispatch = createEventDispatcher();
  $: label = modelName || '-';
  $: shortLabel = label.length > 30 ? label.slice(0, 30) + '...' : label;
</script>

<div class="status-bar">
  <button class="item" on:click={() => dispatch('openModelSelector')} title={label}>
    {shortLabel}
  </button>
</div>

<style>
  .status-bar {
    display: flex;
    align-items: center;
    border-top: 1px solid var(--border);
    font-size: 12px;
  }
  .item {
    background: none;
    border: none;
    border-right: 1px solid var(--border);
    color: var(--text-dim);
    font-family: var(--font-ui);
    font-size: 12px;
    cursor: pointer;
    padding: 4px 12px;
  }
  .item:hover {
    color: var(--text);
  }
</style>
