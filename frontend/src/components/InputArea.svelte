<script>
  import { Cancel } from '../../wailsjs/go/main/App';
  import { createEventDispatcher } from 'svelte';
  export let busy = false;
  const dispatch = createEventDispatcher();
  let text = '';
  let textarea;

  function handleKeydown(e) {
    if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); submit(); }
    if (e.key === 'Escape' && busy) { cancel(); }
  }

  function submit() {
    const t = text.trim();
    if (!t) return;
    dispatch('submit', t);
    text = '';
    if (textarea) textarea.style.height = 'auto';
  }

  async function cancel() {
    try { await Cancel(); } catch (err) { console.error('Cancel failed:', err); }
  }

  function autoResize() {
    if (!textarea) return;
    textarea.style.height = 'auto';
    textarea.style.height = Math.min(textarea.scrollHeight, 132) + 'px';
  }

  export function focus() { textarea?.focus(); }
  export function prefill(t) { text = t; if (textarea) { textarea.style.height = 'auto'; setTimeout(autoResize, 0); } focus(); }
</script>

<div class="input-area">
  <div class="input-row">
    <textarea bind:this={textarea} bind:value={text} on:keydown={handleKeydown} on:input={autoResize}
      placeholder={busy ? 'Queue a message...' : 'Type your message...'} rows="1"></textarea>
    {#if busy && !text.trim()}
      <button class="icon-btn cancel" on:click={cancel} title="Stop (Esc)">
        <svg width="16" height="16" viewBox="0 0 24 24" fill="currentColor" stroke="none"><rect x="6" y="6" width="12" height="12" rx="2" /></svg>
      </button>
    {:else}
      <button class="icon-btn send" on:click={submit} disabled={!text.trim()} title={busy ? 'Queue message (Enter)' : 'Send (Enter)'}>
        <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><line x1="12" y1="19" x2="12" y2="5"/><polyline points="5 12 12 5 19 12"/></svg>
      </button>
    {/if}
  </div>
  <slot></slot>
</div>

<style>
  .input-area { margin:8px 12px; border:1px solid var(--border); border-radius:var(--radius); background:var(--bg-input); overflow:hidden; }
  .input-row { display:flex; align-items:flex-end; gap:8px; padding:8px; }
  textarea { flex:1; background:transparent; color:var(--text); border:none; padding:4px; font-family:var(--font-ui); font-size:calc(13px * var(--scale, 1)); line-height:1.6; resize:none; overflow-y:auto; min-height:36px; max-height:132px; }
  textarea:focus { outline:none; }
  textarea:disabled { opacity:.5; }
  textarea::placeholder { color:var(--text-dim); }
  .icon-btn { display:flex; align-items:center; justify-content:center; width:32px; height:32px; border:1px solid var(--border-strong); border-radius:var(--radius); cursor:pointer; flex-shrink:0; }
  .icon-btn.send { background:var(--bg-elevated); color:var(--text-dim); }
  .icon-btn.send:hover:not(:disabled) { border-color:var(--accent); color:var(--accent); }
  .icon-btn.send:disabled { opacity:.3; cursor:default; }
  .icon-btn.cancel { background:var(--bg-elevated); color:var(--error); }
  .icon-btn.cancel:hover { border-color:var(--error); }
</style>
