<script>
  import { createEventDispatcher } from 'svelte';
  export let children = []; // [{ id, agentType, lifecycle, lastActivity, live, unknownHeader }]
  export let jobs = []; // [{ id }]
  export let currentChild = '';
  const dispatch = createEventDispatcher();
  let dialog;

  function openDialog(node) {
    node.showModal();
    return { destroy() { if (node.open) node.close(); } };
  }

  function detail(child) {
    // A live child missing from both lifecycle lists keeps only its actual
    // ID: no agent type, time or lineage is ever fabricated.
    if (child.unknownHeader) return 'live · header not listed';
    const parts = [child.lifecycle];
    if (child.agentType) parts.push(child.agentType);
    if (child.live) parts.push('live');
    return parts.join(' · ');
  }
</script>

<dialog class="modal" bind:this={dialog} use:openDialog aria-labelledby="background-panel-title"
  on:click={(e) => { if (e.target === dialog) dialog.close(); }}
  on:close={() => dispatch('close')}>
  <div class="menu">
    <div id="background-panel-title" class="hdr">
      <span>Background</span>
      <button class="panel-close" on:click={() => dialog.close()} aria-label="Close background panel">×</button>
    </div>
    <div class="list">
      <div class="section">Children</div>
      {#if children.length === 0}
        <div class="empty">(no child sessions)</div>
      {:else}
        {#each children as child (child.id)}
          <button class="row" class:cur={child.id === currentChild} on:click={() => dispatch('open', child.id)}
            aria-label={`Inspect child session ${child.id}`}>
            <span class="id">{child.id}</span>
            <span class="detail">{detail(child)}</span>
          </button>
        {/each}
      {/if}
      <div class="section">Jobs</div>
      {#if jobs.length === 0}
        <div class="empty">(no live jobs)</div>
      {:else}
        {#each jobs as job (job.id)}
          <div class="job" title="Live background job"><span class="id">{job.id}</span><span class="detail">job</span></div>
        {/each}
      {/if}
    </div>
  </div>
</dialog>

<style>
  .modal { position:fixed; inset:0; width:100vw; height:100vh; max-width:none; max-height:none; margin:0; padding:0; border:0; background:transparent; }
  .modal::backdrop { background:transparent; }
  .menu { position:absolute; z-index:1; top:40px; right:12px; background:var(--bg-elevated); border:1px solid var(--border-strong); min-width:300px; max-height:420px; display:flex; flex-direction:column; box-shadow:var(--shadow-menu); }
  .hdr { display:flex; justify-content:space-between; align-items:center; padding:6px 12px; font-size:12px; color:var(--text-dim); text-transform:uppercase; letter-spacing:.5px; border-bottom:1px solid var(--border); }
  .panel-close { background:none; border:none; color:var(--text-dim); font-size:16px; cursor:pointer; padding:0 4px; }
  .panel-close:hover { color:var(--accent); }
  .list { flex:1; overflow:auto; }
  .section { padding:6px 12px 2px; font-size:11px; color:var(--text-dim); text-transform:uppercase; letter-spacing:.5px; }
  .empty { padding:4px 12px 8px; color:var(--text-dim); font-family:var(--font-ui); font-size:12px; }
  .row { display:flex; align-items:center; gap:10px; width:100%; padding:6px 12px; font-family:var(--font-ui); font-size:12px; color:var(--text); cursor:pointer; background:none; border:none; text-align:left; }
  .row:hover { background:var(--accent-soft); }
  .row.cur { color:var(--accent); }
  .job { display:flex; align-items:center; gap:10px; padding:6px 12px; font-family:var(--font-ui); font-size:12px; color:var(--text-dim); }
  .id { flex:1; overflow:hidden; text-overflow:ellipsis; white-space:nowrap; font-family:var(--font-mono); }
  .detail { color:var(--text-dim); flex-shrink:0; font-size:11px; }
</style>
