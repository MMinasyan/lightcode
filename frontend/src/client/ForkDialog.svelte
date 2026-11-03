<script>
  import { openDialog } from './dialogs';
  // Temporary replacement-Fork editor for one root user-origin item. The exact
  // edited content text is forwarded, so malformed payloads stay the server's
  // decision; rewind exists only for an owned operation_id, and an unknown
  // rewind offers no Retry.
  import { createEventDispatcher } from 'svelte';
  import { restoreLines } from './restore';
  export let item = null;
  export let text = '';
  export let rewind = false;
  export let attempt = null;
  export let note = '';
  export let busy = false;
  const dispatch = createEventDispatcher();
  let dialog;

  $: owned = !!(item && item.operation_id);

</script>

<dialog class="modal" bind:this={dialog} use:openDialog aria-labelledby="fork-title"
  on:click={(e) => { if (e.target === dialog) dialog.close(); }}
  on:close={() => dispatch('close')}>
  <div class="panel">
    <div id="fork-title" class="hdr">
      <span>Fork from {item ? item.item_id : ''}</span>
      <button class="panel-close" on:click={() => dialog.close()} aria-label="Close fork dialog">×</button>
    </div>
    <div class="body">
      {#if item}
        <div class="source">
          <div class="source-label">Source input ({item.origin}{owned ? ` · operation ${item.operation_id}` : ' · copied, no Operation'})</div>
          <div class="source-text">{(item.content || []).map((part) => part.kind === 'text' ? part.text : `[${part.kind}]`).join('\n')}</div>
        </div>
      {/if}
      <label class="field"><span class="flabel">Replacement content (ContentPart[] JSON)</span>
        <textarea class="content" bind:value={text} rows="6" aria-label="Replacement content JSON"></textarea>
      </label>
      {#if owned}
        <label class="rewind"><input type="checkbox" aria-label="Rewind code before Fork" bind:checked={rewind} />
          Rewind source code to before this input's Operation, then Fork</label>
      {/if}
      {#if attempt && attempt.restore}
        <div class="restore" class:failed={attempt.restore.kind === 'failed'}>
          {#each restoreLines(attempt.restore) as line}
            <div class="restore-line">{line}</div>
          {/each}
        </div>
      {/if}
      {#if attempt && attempt.restore?.kind === 'unknown'}
        <div class="unknown">Rewind outcome unknown: the source restore may still be running. Fork was not sent.</div>
        <div class="actions">
          <button class="ghost" on:click={() => dispatch('abandon', attempt)} aria-label="Abandon fork attempt">Abandon</button>
        </div>
      {:else if attempt && attempt.status === 'unknown'}
        <div class="unknown">Outcome unknown: the Fork may or may not have been admitted.</div>
        <div class="actions">
          <button class="ghost" on:click={() => dispatch('retry', attempt)} aria-label="Retry fork">Retry Fork</button>
          <button class="ghost" on:click={() => dispatch('abandon', attempt)} aria-label="Abandon fork attempt">Abandon</button>
        </div>
      {:else if attempt && attempt.status === 'rejected'}
        <div class="rejected">{attempt.error || 'Rejected'}</div>
        <div class="actions">
          <button class="ghost" on:click={() => dispatch('retry', attempt)} aria-label="Retry fork">Retry Fork</button>
          <button class="ghost" on:click={() => dispatch('abandon', attempt)} aria-label="Abandon fork attempt">Abandon</button>
        </div>
      {:else}
        {#if note}<div class="note">{note}</div>{/if}
        <div class="actions">
          <button class="ghost" on:click={() => dialog.close()}>Cancel</button>
          <button class="primary" disabled={busy || !text.trim()} on:click={() => dispatch('submit')} aria-label="Submit fork">
            {busy ? 'Forking…' : (rewind ? 'Rewind and Fork' : 'Fork')}
          </button>
        </div>
      {/if}
    </div>
  </div>
</dialog>

<style>
  .panel { position:absolute; top:40px; left:50%; transform:translateX(-50%); width:560px; max-width:94vw; max-height:86vh; background:var(--bg-elevated); border:1px solid var(--border-strong); box-shadow:var(--shadow-lg); display:flex; flex-direction:column; }
  .hdr { display:flex; justify-content:space-between; align-items:center; padding:6px 12px; font-size:12px; color:var(--text-dim); text-transform:uppercase; letter-spacing:.5px; border-bottom:1px solid var(--border); }
  .body { padding:12px; overflow-y:auto; display:flex; flex-direction:column; gap:8px; }
  .source { border:1px solid var(--border); border-radius:var(--radius); background:var(--bg); padding:6px 8px; }
  .source-label { color:var(--text-dim); font-family:var(--font-ui); font-size:11px; text-transform:uppercase; letter-spacing:.5px; margin-bottom:4px; }
  .source-text { color:var(--text); font-family:var(--font-ui); font-size:12px; white-space:pre-wrap; word-break:break-word; }
  .field { display:flex; flex-direction:column; gap:4px; }
  .flabel { color:var(--text-dim); font-family:var(--font-ui); font-size:12px; }
  .content { width:100%; box-sizing:border-box; padding:6px 8px; border:1px solid var(--border-button); border-radius:var(--radius); background:var(--bg-input); color:var(--text); font-family:var(--font-mono); font-size:12px; resize:vertical; outline:none; }
  .content:focus { border-color:var(--accent); background:var(--bg-input-focus); }
  .rewind { display:flex; align-items:center; gap:6px; color:var(--text); font-family:var(--font-ui); font-size:12px; }
  .restore { border-left:3px solid var(--accent); padding:4px 8px; background:var(--bg); font-family:var(--font-mono); font-size:11px; color:var(--text); }
  .restore.failed { border-left-color:var(--warn); }
  .restore-line { white-space:pre-wrap; word-break:break-word; }
  .unknown { border-left:3px solid var(--warn); padding:6px 8px; background:var(--accent-soft); color:var(--warn); font-family:var(--font-ui); font-size:12px; }
  .rejected { border-left:3px solid var(--error); padding:6px 8px; background:var(--error-soft); color:var(--error); font-family:var(--font-ui); font-size:12px; }
  .note { color:var(--text-dim); font-family:var(--font-ui); font-size:12px; }
  .actions { display:flex; justify-content:flex-end; gap:8px; }
  .ghost, .primary { padding:5px 12px; border:1px solid var(--border-button); border-radius:var(--radius); background:none; color:var(--text-dim); font-family:var(--font-ui); font-size:12px; cursor:pointer; }
  .ghost:hover, .primary:hover:not(:disabled) { color:var(--accent); border-color:var(--accent); }
  .primary:disabled { opacity:.45; cursor:default; }
</style>
