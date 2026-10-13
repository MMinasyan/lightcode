<script>
  import { openDialog } from './dialogs';
  // The target Workspace picker: generated ListWorkspaces choices (already
  // normalized absolute lexical paths) plus the native directory dialog. It
  // owns no Project record and performs no path canonicalization; selecting a
  // choice is a host-local decision the App applies through its one shared
  // selection composition.
  import { createEventDispatcher } from 'svelte';
  export let workspaces = []; // [{ root, display_name }]
  export let current = '';
  const dispatch = createEventDispatcher();
  let dialog;

</script>

<dialog class="modal" bind:this={dialog} use:openDialog aria-labelledby="workspace-picker-title"
  on:click={(e) => { if (e.target === dialog) dialog.close(); }}
  on:close={() => dispatch('close')}>
  <div class="menu">
    <div id="workspace-picker-title" class="hdr">
      <span>Workspace</span>
      <button class="panel-close" on:click={() => dialog.close()} aria-label="Close workspace picker">×</button>
    </div>
    <div class="list">
      {#if workspaces.length === 0}
        <div class="empty">(no known workspaces)</div>
      {:else}
        {#each workspaces as entry (entry.root)}
          <button class="row" class:cur={entry.root === current} on:click={() => dispatch('select', entry.root)}
            aria-label={`Select workspace ${entry.root}`}>
            <span class="root">{entry.root}</span>
            <span class="name">{entry.display_name}</span>
          </button>
        {/each}
      {/if}
    </div>
    <div class="actions">
      <button class="pick" on:click={() => dispatch('pick')} aria-label="Choose workspace directory">Choose directory…</button>
    </div>
  </div>
</dialog>

<style>
  .menu { position:absolute; z-index:1; top:40px; left:12px; background:var(--bg-elevated); border:1px solid var(--border-strong); min-width:320px; max-width:560px; max-height:400px; display:flex; flex-direction:column; box-shadow:var(--shadow-menu); }
  .hdr { display:flex; align-items:center; justify-content:space-between; padding:6px 12px; font-size:12px; color:var(--text-dim); text-transform:uppercase; letter-spacing:.5px; border-bottom:1px solid var(--border); }
  .list { flex:1; overflow:auto; }
  .empty { padding:8px 12px; color:var(--text-dim); font-family:var(--font-ui); font-size:12px; }
  .row { display:flex; flex-direction:column; gap:2px; width:100%; padding:6px 12px; font-family:var(--font-ui); font-size:12px; color:var(--text); cursor:pointer; background:none; border:none; text-align:left; }
  .row:hover { background:var(--accent-soft); }
  .row.cur { color:var(--accent); }
  .root { font-family:var(--font-mono); overflow:hidden; text-overflow:ellipsis; white-space:nowrap; max-width:100%; }
  .name { color:var(--text-dim); font-size:11px; }
  .actions { border-top:1px solid var(--border); padding:6px 12px; }
  .pick { background:none; border:1px solid var(--border-button); border-radius:var(--radius); color:var(--text-dim); font-family:var(--font-ui); font-size:12px; cursor:pointer; padding:4px 10px; }
  .pick:hover { color:var(--accent); border-color:var(--accent); }
</style>
