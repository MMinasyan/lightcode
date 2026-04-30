<script>
  import { createEventDispatcher } from 'svelte';
  export let provider = '';
  export let model = '';
  const dispatch = createEventDispatcher();
</script>

<div class="status-bar">
  <button class="item" on:click={() => dispatch('openModelSelector')}>
    {(model + ' (' + provider + ')').length > 30 ? (model + ' (' + provider + ')').slice(0, 30) + '...' : model + ' (' + provider + ')'}
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
