<script>
  import { openDialog } from './dialogs';
  // The target Session-type picker: definitions come from the ConfigurationView
  // agent roster and are displayed uniformly; the server remains the accepting
  // owner of any chosen type.
  import { createEventDispatcher } from 'svelte';
  export let agents = []; // [{ name, description, subagent }]
  export let current = '';
  const dispatch = createEventDispatcher();
  let dialog;

</script>

<dialog class="modal" bind:this={dialog} use:openDialog aria-labelledby="agent-picker-title"
  on:click={(e) => { if (e.target === dialog) dialog.close(); }}
  on:close={() => dispatch('close')}>
  <div class="menu">
    <div id="agent-picker-title" class="hdr">
      <span>Session agent type</span>
      <button class="panel-close" on:click={() => dialog.close()} aria-label="Close agent picker">×</button>
    </div>
    <div class="list">
      {#if agents.length === 0}
        <div class="empty">(no agent definitions)</div>
      {:else}
        {#each agents as agent (agent.name)}
          <button class="row" class:cur={agent.name === current} on:click={() => dispatch('select', agent.name)}
            aria-label={`Set session agent type ${agent.name}`}>
            <span class="name">{agent.name}</span>
            {#if agent.description}<span class="desc">{agent.description}</span>{/if}
          </button>
        {/each}
      {/if}
    </div>
  </div>
</dialog>

<style>
  .menu { position:absolute; z-index:1; top:40px; left:12px; background:var(--bg-elevated); border:1px solid var(--border-strong); min-width:300px; max-width:480px; max-height:360px; display:flex; flex-direction:column; box-shadow:var(--shadow-menu); }
  .hdr { display:flex; align-items:center; justify-content:space-between; padding:6px 12px; font-size:12px; color:var(--text-dim); text-transform:uppercase; letter-spacing:.5px; border-bottom:1px solid var(--border); }
  .list { flex:1; overflow:auto; }
  .empty { padding:8px 12px; color:var(--text-dim); font-family:var(--font-ui); font-size:12px; }
  .row { display:flex; flex-direction:column; gap:2px; width:100%; padding:6px 12px; font-family:var(--font-ui); font-size:12px; color:var(--text); cursor:pointer; background:none; border:none; text-align:left; }
  .row:hover { background:var(--accent-soft); }
  .row.cur { color:var(--accent); }
  .name { font-family:var(--font-mono); }
  .desc { color:var(--text-dim); font-size:11px; }
</style>
