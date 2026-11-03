<script>
  // The per-message Markdown copy action: the row's own success feedback and
  // timer, and a rejected clipboard write that leaves the row unchanged.
  export let text = '';

  let copySuccess = false;
  let copyTimer = null;

  async function copyMarkdown() {
    try {
      await navigator.clipboard.writeText(text);
      copySuccess = true;
      clearTimeout(copyTimer);
      copyTimer = setTimeout(() => { copySuccess = false; }, 1500);
    } catch (e) {}
  }
</script>

<button class="copy-icon" class:copied={copySuccess} on:click={copyMarkdown}
  title="Copy as markdown" aria-label="Copy message as markdown">
  {#if copySuccess}
    <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"><polyline points="20 6 9 17 4 12"/></svg>
  {:else}
    <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="9" y="9" width="13" height="13" rx="2" ry="2"/><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"/></svg>
  {/if}
</button>

<style>
  .copy-icon { position:absolute; bottom:4px; right:4px; display:flex; align-items:center; background:none; border:none; color:var(--text-dim); cursor:pointer; padding:2px; visibility:hidden; z-index:1; }
  :global(.message.assistant:hover) .copy-icon { visibility:visible; }
  .copy-icon:hover { color:var(--accent); }
</style>
