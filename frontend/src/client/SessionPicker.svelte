<script>
  import { openDialog } from './dialogs';
  // Session picker: explicit New, Resume (archived), Archive and Delete. The
  // archive-first delete sequence is owned by the App; this component dispatches
  // intents and renders the presented lists.
  import { createEventDispatcher } from 'svelte';
  export let openRoots = []; // [{ id, agentType, lastActivity }]
  export let archivedRoots = [];
  export let current = '';
  export let workspace = '';
  export let loading = false;
  const dispatch = createEventDispatcher();
  let dialog;
  let tab = 'open';

  function fmtTs(timestamp) {
    if (!timestamp) return '';
    const d = new Date(timestamp);
    const mon = ['Jan','Feb','Mar','Apr','May','Jun','Jul','Aug','Sep','Oct','Nov','Dec'][d.getMonth()];
    const pad = n => n < 10 ? '0' + n : n;
    return `${mon} ${d.getDate()} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
  }

  $: rows = tab === 'open' ? openRoots : archivedRoots;
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
    <div class="tabs">
      <button class="tab" class:sel={tab === 'open'} on:click={() => (tab = 'open')} aria-label="Open sessions">Open</button>
      <button class="tab" class:sel={tab === 'archived'} on:click={() => (tab = 'archived')} aria-label="Archived sessions">Archived</button>
      {#if tab === 'open'}
        <button class="new" on:click={() => dispatch('new')} aria-label="New session">+</button>
      {/if}
    </div>
    <div class="list">
      {#if loading && rows.length === 0}
        <div class="empty">loading…</div>
      {:else if rows.length === 0}
        <div class="empty">(no {tab} sessions)</div>
      {:else}
        {#each rows as s (s.id)}
          <div class="row" class:cur={s.id === current} role="button" tabindex="0"
            on:click={() => dispatch(tab === 'open' ? 'select' : 'resume', s.id)}
            on:keydown={(e) => {
              // Only the row's own Enter/Space activates it; nested controls keep native activation.
              if (e.target !== e.currentTarget) return;
              if (e.key === 'Enter' || e.key === ' ') {
                e.preventDefault();
                dispatch(tab === 'open' ? 'select' : 'resume', s.id);
              }
            }}
            aria-label={`${tab === 'open' ? 'Select' : 'Resume'} session ${s.id}`}>
            <span class="id">{s.id}</span>
            <span class="type">{s.agentType}</span>
            <span class="ts">{fmtTs(s.lastActivity)}</span>
            {#if tab === 'open'}
              <button class="act" on:click|stopPropagation={() => dispatch('archive', s.id)} aria-label={`Archive session ${s.id}`}>Archive</button>
            {/if}
            <button class="act danger" on:click|stopPropagation={() => dispatch('remove', s.id)} aria-label={`Delete session ${s.id}`}>Delete</button>
          </div>
        {/each}
      {/if}
    </div>
  </div>
</dialog>

<style>
  .menu { position:absolute; z-index:1; top:40px; left:160px; background:var(--bg-elevated); border:1px solid var(--border-strong); min-width:320px; max-width:520px; max-height:380px; display:flex; flex-direction:column; box-shadow:var(--shadow-menu); }
  .hdr { display:flex; align-items:center; gap:10px; padding:6px 12px; font-size:12px; color:var(--text-dim); text-transform:uppercase; letter-spacing:.5px; border-bottom:1px solid var(--border); }
  .workspace { flex:1; min-width:0; overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
  .tabs { display:flex; align-items:center; border-bottom:1px solid var(--border); }
  .tab { background:none; border:none; border-right:1px solid var(--border); color:var(--text-dim); font-family:var(--font-ui); font-size:12px; padding:5px 14px; cursor:pointer; }
  .tab.sel { color:var(--accent); }
  .new { margin-left:auto; background:none; border:none; color:var(--text-dim); font-family:var(--font-ui); font-size:14px; padding:3px 10px; cursor:pointer; }
  .new:hover { color:var(--accent); }
  .list { flex:1; overflow:auto; }
  .empty { padding:8px 12px; color:var(--text-dim); font-family:var(--font-ui); font-size:12px; }
  .row { display:flex; align-items:center; gap:6px; padding:2px 6px; cursor:pointer; }
  .row:hover { background:var(--accent-soft); }
  .row.cur { color:var(--accent); }
  .id { flex:1; overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
  .type { color:var(--text-dim); }
  .ts { color:var(--text-dim); }
  .act { background:none; border:1px solid var(--border-button); border-radius:var(--radius); color:var(--text-dim); font-family:var(--font-ui); font-size:11px; padding:2px 8px; cursor:pointer; flex-shrink:0; }
  .act:hover { color:var(--accent); border-color:var(--accent); }
  .act.danger:hover { color:var(--error); border-color:var(--error); }
</style>
