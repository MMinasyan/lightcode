<script>
  import { openDialog } from './dialogs';
  // Exact per-model usage details from one accepted hydration. Counts are the
  // producer's signed decimal strings rendered verbatim; a model with no
  // recorded usage contributes no row (absence is never displayed as zero) and
  // no child aggregation is invented.
  import { createEventDispatcher } from 'svelte';
  export let usage = { totals: { by_model: [] }, context: { used_tokens: '0', context_window: 0 } };
  const dispatch = createEventDispatcher();
  let dialog;

</script>

<dialog class="modal" bind:this={dialog} use:openDialog aria-labelledby="usage-title"
  on:click={(e) => { if (e.target === dialog) dialog.close(); }}
  on:close={() => dispatch('close')}>
  <div class="panel">
    <div id="usage-title" class="hdr"><span>Token usage</span>
      <button class="panel-close" on:click={() => dialog.close()} aria-label="Close usage details">×</button>
    </div>
    <div class="list">
      {#if usage.totals.by_model.length === 0}
        <div class="empty">(no usage recorded yet)</div>
      {:else}
        <div class="row head"><span class="model">model</span><span>cache</span><span>input</span><span>output</span></div>
        {#each usage.totals.by_model as entry (entry.model)}
          <div class="row">
            <span class="model" title={entry.model}>{entry.model}</span>
            <span>{entry.usage.cached_input_tokens}</span>
            <span>{entry.usage.input_tokens}</span>
            <span>{entry.usage.output_tokens}</span>
          </div>
        {/each}
      {/if}
      <div class="context">Context used: {usage.context.used_tokens} / {usage.context.context_window}</div>
    </div>
  </div>
</dialog>

<style>
  .panel { position:absolute; top:40px; right:12px; background:var(--bg-elevated); border:1px solid var(--border-strong); min-width:420px; max-width:640px; max-height:420px; display:flex; flex-direction:column; box-shadow:var(--shadow-menu); }
  .hdr { display:flex; justify-content:space-between; align-items:center; padding:6px 12px; font-size:12px; color:var(--text-dim); text-transform:uppercase; letter-spacing:.5px; border-bottom:1px solid var(--border); }
  .list { flex:1; overflow:auto; padding:6px 12px; }
  .row { display:grid; grid-template-columns:1fr 120px 120px 120px; gap:6px; padding:3px 0; font-family:var(--font-mono); font-size:12px; color:var(--text); text-align:right; }
  .row.head { color:var(--text-dim); text-transform:uppercase; font-size:10px; letter-spacing:.5px; }
  .row .model { text-align:left; overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
  .empty { padding:8px 0; color:var(--text-dim); font-family:var(--font-ui); font-size:12px; }
  .context { padding:8px 0 2px; border-top:1px solid var(--border); color:var(--text-dim); font-family:var(--font-mono); font-size:12px; }
</style>
