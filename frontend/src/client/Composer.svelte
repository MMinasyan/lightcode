<script>
  import { createEventDispatcher } from 'svelte';
  import { restoreLines } from './restore';
  export let text = '';
  export let active = false; // an admitted running Operation exists
  export let hasSession = true;
  export let attempts = []; // outstanding attempts for this session
  const dispatch = createEventDispatcher();
  let textarea;

  function handleKeydown(e) {
    if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); send(); }
  }

  function send() {
    if (!text.trim() || !hasSession) return;
    dispatch('send', 'regular');
  }

  function queue() {
    if (!text.trim() || !hasSession) return;
    dispatch('send', 'queued');
  }

  function attemptLabel(attempt) {
    if (attempt.kind === 'compact') return 'compaction';
    if (attempt.kind === 'fork') return 'fork';
    return attempt.mode === 'queued' ? 'queued message' : 'message';
  }

  // Input-driven auto-resize with the 132px ceiling (the retained
  // components/InputArea.svelte behavior).
  function autoResize() {
    if (!textarea) return;
    textarea.style.height = 'auto';
    textarea.style.height = Math.min(textarea.scrollHeight, 132) + 'px';
  }

  export function focus() { textarea?.focus(); }
</script>

<div class="input-area composer">
  <div class="input-row">
    <textarea bind:this={textarea} bind:value={text} on:keydown={handleKeydown} on:input={autoResize}
      placeholder={hasSession ? (active ? 'Steer the active message...' : 'Type your message...') : 'No session is open'}
      aria-label="Message input" rows="1"></textarea>
    <div class="controls">
      <button class="send" on:click={send} disabled={!text.trim() || !hasSession}
        title={active ? 'Steering send (Enter)' : 'Send (Enter)'}
        aria-label={active ? 'Steering send' : 'Send'}>
        {active ? 'Steer' : 'Send'}
      </button>
      <button class="queue" on:click={queue} disabled={!text.trim() || !hasSession}
        title="Queue message" aria-label="Queue message">Queue</button>
      {#if active}
        <button class="interrupt" on:click={() => dispatch('interrupt')}
          title="Interrupt (Esc)" aria-label="Interrupt">Interrupt</button>
      {/if}
      <button class="stop" on:click={() => dispatch('stop')}
        title="Stop in background and wait for convergence" aria-label="Stop">Stop</button>
      <button class="compact" on:click={() => dispatch('compact')}
        title="Compact conversation" aria-label="Compact">Compact</button>
    </div>
  </div>
  {#each attempts as attempt (attempt.id)}
    {#if attempt.restore?.kind === 'unknown'}
      <!-- The rewind stage has no known outcome: Fork was never sent and no
           Retry is offered, because a Fork retry could bypass the stage that
           may still be running. Only explicit abandonment leaves it. -->
      <div class="attempt unknown">
        <span class="attempt-content">Rewind outcome unknown: the source restore may still be running. Fork was not sent.</span>
        {#each restoreLines(attempt.restore) as line}
          <span class="attempt-restore">{line}</span>
        {/each}
        <button class="attempt-btn" on:click={() => dispatch('abandon', attempt)} aria-label="Abandon">Abandon</button>
      </div>
    {:else if attempt.status === 'unknown'}
      <div class="attempt unknown">
        <span class="attempt-content">Outcome unknown: {attemptLabel(attempt)} may or may not have been delivered.</span>
        {#if attempt.restore}{#each restoreLines(attempt.restore) as line}
          <span class="attempt-restore">{line}</span>
        {/each}{/if}
        <span class="retry-warning">Retrying keeps the same message identity. If the original was delivered before the connection loss, the message may be duplicated.</span>
        <button class="attempt-btn" on:click={() => dispatch('retry', attempt)} aria-label="Retry">Retry</button>
        <button class="attempt-btn" on:click={() => dispatch('abandon', attempt)} aria-label="Abandon">Abandon</button>
      </div>
    {:else if attempt.status === 'rejected'}
      <div class="attempt rejected">
        <span class="attempt-content">{attempt.error || 'Rejected'}</span>
        {#if attempt.restore}{#each restoreLines(attempt.restore) as line}
          <span class="attempt-restore">{line}</span>
        {/each}{/if}
        <span class="retry-warning">Retrying keeps the same message identity. If the original was delivered before the connection loss, the message may be duplicated.</span>
        <button class="attempt-btn" on:click={() => dispatch('retry', attempt)} aria-label="Retry">Retry</button>
        <button class="attempt-btn" on:click={() => dispatch('abandon', attempt)} aria-label="Abandon">Abandon</button>
      </div>
    {/if}
  {/each}
</div>

<style>
  .input-area { margin:8px 12px; border:1px solid var(--border); border-radius:var(--radius); background:var(--bg-input); }
  .input-row { display:flex; align-items:flex-end; gap:8px; padding:8px; flex-wrap:wrap; }
  textarea { flex:1; min-width:0; background:transparent; color:var(--text); border:none; padding:4px; font-family:var(--font-ui); font-size:calc(13px * var(--scale, 1)); line-height:1.6; resize:none; overflow-y:auto; min-height:36px; max-height:132px; }
  textarea:focus { outline:none; background:var(--bg-input-focus); }
  textarea:disabled { opacity:.5; }
  textarea::placeholder { color:var(--text-dim); }
  .controls { display:flex; align-items:center; gap:6px; flex-wrap:wrap; flex-shrink:0; }
  .send, .queue, .interrupt, .stop, .compact { border:1px solid var(--border-strong); border-radius:var(--radius); cursor:pointer; padding:6px 10px; font-family:var(--font-ui); font-size:calc(12px * var(--scale, 1)); }
  .send:focus-visible, .queue:focus-visible, .interrupt:focus-visible, .stop:focus-visible, .compact:focus-visible { outline:1px solid var(--accent); outline-offset:1px; }
  .send { background:var(--bg-elevated); color:var(--text-dim); }
  .send:hover:not(:disabled) { border-color:var(--accent); color:var(--accent); }
  .send:disabled { opacity:.3; cursor:default; }
  .queue { background:var(--bg-elevated); color:var(--text-dim); }
  .queue:hover:not(:disabled) { border-color:var(--accent); color:var(--accent); }
  .queue:disabled { opacity:.3; cursor:default; }
  .interrupt { background:var(--bg-elevated); color:var(--error); }
  .interrupt:hover { border-color:var(--error); }
  .stop { background:var(--bg-elevated); color:var(--warn); }
  .stop:hover { border-color:var(--warn); }
  .compact { background:var(--bg-elevated); color:var(--text-dim); }
  .compact:hover { border-color:var(--accent); color:var(--accent); }
  .attempt { display:flex; flex-wrap:wrap; align-items:center; gap:6px 8px; margin:0 8px 8px; padding:6px 10px; border-radius:var(--radius); font-family:var(--font-ui); font-size:calc(12px * var(--scale, 1)); }
  .attempt.unknown { background:var(--accent-soft); border:1px solid var(--border-button); color:var(--text); }
  .attempt.rejected { background:var(--error-soft); border:1px solid var(--error); color:var(--error); }
  .attempt-content { flex:1 1 100%; min-width:0; overflow-wrap:anywhere; }
  .attempt-restore { flex:1 1 100%; font-family:var(--font-mono); font-size:calc(11px * var(--scale, 1)); color:var(--text-dim); overflow-wrap:anywhere; }
  .attempt.rejected .attempt-content { color:var(--text); }
  .retry-warning { flex:1 1 100%; color:var(--warn); font-size:calc(11px * var(--scale, 1)); overflow-wrap:anywhere; }
  .attempt-btn { flex-shrink:0; background:none; border:1px solid var(--border-button); border-radius:var(--radius); color:var(--text-dim); font-family:var(--font-ui); font-size:calc(11px * var(--scale, 1)); padding:3px 10px; cursor:pointer; }
  .attempt-btn:hover { color:var(--accent); border-color:var(--accent); }
</style>
