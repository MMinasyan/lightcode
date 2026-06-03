<script>
  import { createEventDispatcher } from 'svelte';
  export let warnings = [];
  const dispatch = createEventDispatcher();

  const actionLabels = {
    rules_too_large: 'Shorten rules file',
    rules_not_found: 'Create rules file',
    rules_read_error: null,
    lsp_install_failed: null,
    lsp_server_unavailable: null,
  };
</script>

<div class="layer">
  <button type="button" class="backdrop" tabindex="-1" aria-label="Close warnings" on:click={() => dispatch('close')}></button>
  <div class="prompt" role="dialog" aria-modal="true" aria-labelledby="warning-details-title" tabindex="-1">
    <div class="hdr" id="warning-details-title">Warnings</div>
    <div class="list">
      {#each warnings as w}
        <div class="warn-item">
          <div class="warn-msg">{w.message}</div>
          {#if actionLabels[w.kind]}
            <button class="btn action" disabled title="Not yet implemented">{actionLabels[w.kind]}</button>
          {/if}
        </div>
      {/each}
      {#if warnings.length === 0}
        <div class="empty">No warnings</div>
      {/if}
    </div>
    <div class="actions">
      <button class="btn" on:click={() => dispatch('close')}>Close</button>
    </div>
  </div>
</div>

<style>
  .layer { position:fixed; inset:0; z-index:300; display:flex; align-items:center; justify-content:center; }
  .backdrop { position:absolute; inset:0; border:0; padding:0; margin:0; background:var(--overlay); cursor:default; }
  .prompt { position:relative; z-index:1; background:var(--bg-elevated); border:1px solid var(--border-strong); min-width:400px; max-width:600px; }
  .hdr { padding:8px 12px; font-size:12px; font-weight:600; text-transform:uppercase; letter-spacing:.5px; border-bottom:1px solid var(--border); }
  .list { padding:8px 12px; max-height:320px; overflow:auto; }
  .warn-item { padding:6px 0; border-bottom:1px solid var(--border); }
  .warn-item:last-child { border-bottom:none; }
  .warn-msg { font-family:var(--font-ui); font-size:12px; color:var(--text); margin-bottom:4px; }
  .empty { font-family:var(--font-ui); font-size:12px; color:var(--text-dim); }
  .actions { display:flex; gap:8px; padding:8px 12px; border-top:1px solid var(--border); justify-content:flex-end; }
  .btn { padding:4px 12px; font-size:12px; cursor:pointer; border:1px solid var(--border-button); background:none; color:var(--text-dim); font-family:var(--font-ui); }
  .btn:hover { border-color:var(--accent); color:var(--text); }
  .btn.action { color:var(--warning, #e6a700); border-color:var(--warning, #e6a700); }
  .btn.action:disabled { opacity:0.4; cursor:not-allowed; }
</style>
