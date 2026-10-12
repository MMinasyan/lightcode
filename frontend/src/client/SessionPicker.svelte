<script>
  import { createEventDispatcher } from 'svelte';
  export let roots = []; // [{ id, agentType, lastActivity }]
  export let current = '';
  export let workspace = '';
  const dispatch = createEventDispatcher();
  let dialog;

  function fmtTs(timestamp) {
    if (!timestamp) return '';
    const d = new Date(timestamp);
    const mon = ['Jan','Feb','Mar','Apr','May','Jun','Jul','Aug','Sep','Oct','Nov','Dec'][d.getMonth()];
    const pad = n => n < 10 ? '0' + n : n;
    return `${mon} ${d.getDate()} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
  }

  function openDialog(node) {
    node.showModal();
    return { destroy() { if (node.open) node.close(); } };
  }
</script>

<dialog class="modal" bind:this={dialog} use:openDialog aria-labelledby="session-picker-title"
  on:click={(e) => { if (e.target === dialog) dialog.close(); }}
  on:close={() => dispatch('close')}>
  <div class="menu">
    <div id="session-picker-title" class="hdr">
      <span class="workspace" title={workspace || ''}>{workspace || 'workspace'}</span>
      <span>Sessions</span>
      <button class="panel-close" on:click={() => dialog.close()} aria-label="Close session picker">×</button>
    </div>
    <div class="list">
      {#if roots.length === 0}
        <div class="empty">(no open root sessions)</div>
      {:else}
        {#each roots as s (s.id)}
          <button class="row" class:cur={s.id === current} on:click={() => dispatch('select', s.id)}
            aria-label={`Select session ${s.id}`}>
            <span class="id">{s.id}</span>
            <span class="type">{s.agentType}</span>
            <span class="ts">{fmtTs(s.lastActivity)}</span>
          </button>
        {/each}
      {/if}
    </div>
  </div>
</dialog>

<style>
  .modal { position:fixed; inset:0; width:100vw; height:100vh; max-width:none; max-height:none; margin:0; padding:0; border:0; background:transparent; }
  .modal::backdrop { background:transparent; }
  .menu { position:absolute; z-index:1; top:40px; left:160px; background:var(--bg-elevated); border:1px solid var(--border-strong); min-width:260px; max-height:360px; display:flex; flex-direction:column; box-shadow:var(--shadow-menu); }
  .hdr { display:flex; align-items:center; gap:10px; padding:6px 12px; font-size:12px; color:var(--text-dim); text-transform:uppercase; letter-spacing:.5px; border-bottom:1px solid var(--border); }
  .workspace { flex:1; min-width:0; overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
  .panel-close { background:none; border:none; color:var(--text-dim); font-size:16px; cursor:pointer; padding:0 4px; }
  .panel-close:hover { color:var(--accent); }
  .list { flex:1; overflow:auto; }
  .empty { padding:8px 12px; color:var(--text-dim); font-family:var(--font-ui); font-size:12px; }
  .row { display:flex; align-items:center; gap:12px; width:100%; padding:6px 12px; font-family:var(--font-ui); font-size:12px; color:var(--text); cursor:pointer; background:none; border:none; text-align:left; }
  .row:hover { background:var(--accent-soft); }
  .row.cur { color:var(--accent); }
  .id { flex:1; overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
  .type { color:var(--text-dim); }
  .ts { color:var(--text-dim); }
</style>
