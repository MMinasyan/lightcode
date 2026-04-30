<script>
  import { createEventDispatcher } from 'svelte';
  import { renderMarkdown } from '../lib/markdown.js';
  import { settings } from '../lib/settings.js';
  import { BrowserOpenURL } from '../../wailsjs/runtime/runtime';
  export let role = 'user';
  export let content = '';
  export let turn = 0;
  export let partial = false;

  const dispatch = createEventDispatcher();
  let expanded = false;
  let plainEl;
  let overflows = false;
  let revertBtn;
  let menuStyle = '';
  const lineHeight = 1.6;
  const maxLines = 5;

  function checkOverflow(node) {
    const check = () => {
      const fontSize = parseFloat(getComputedStyle(node).fontSize);
      const maxH = fontSize * lineHeight * maxLines;
      overflows = node.scrollHeight > maxH + 1;
    };
    check();
    const ro = new ResizeObserver(check);
    ro.observe(node);
    return { destroy() { ro.disconnect(); } };
  }
  let renderedHtml = '';
  let timer = null;

  // Revert popover state.
  let showMenu = false;
  let confirmAction = null; // 'revertcode' | 'reverthistory' | 'fork'

  $: {
    if (role === 'assistant' && content) {
      if (partial) {
        clearTimeout(timer);
        timer = setTimeout(() => { renderedHtml = renderMarkdown(content); }, 50);
      } else {
        clearTimeout(timer);
        renderedHtml = renderMarkdown(content);
      }
    }
  }

  function openMenu() {
    if (revertBtn) {
      const r = revertBtn.getBoundingClientRect();
      menuStyle = `top:${r.bottom + 4}px;right:${window.innerWidth - r.right}px`;
    }
    showMenu = true;
    confirmAction = null;
  }
  function closeMenu() { showMenu = false; confirmAction = null; }

  function pickAction(action) { confirmAction = action; }

  function confirm(alsoRevertCode) {
    const detail = { turn, content, alsoRevertCode };
    if (confirmAction === 'revertcode') {
      dispatch('revertcode', detail);
    } else if (confirmAction === 'reverthistory') {
      dispatch('reverthistory', { ...detail, turn: turn - 1 });
    } else if (confirmAction === 'fork') {
      dispatch('fork', { ...detail, turn: turn - 1 });
    }
    closeMenu();
  }

  const labels = {
    revertcode: 'Revert code',
    reverthistory: 'Revert history',
    fork: 'Fork from here',
  };

  function handleMdClick(e) {
    const a = e.target.closest('a');
    if (!a) return;
    e.preventDefault();
    const href = a.getAttribute('href') || '';
    if (href) BrowserOpenURL(href);
  }
</script>

<div class="message {role}">
  {#if role === 'user' && turn > 0}
    <button class="revert-icon" bind:this={revertBtn} on:click={openMenu} title="Revert options">
      <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M3 12a9 9 0 1 0 9-9 9.75 9.75 0 0 0-6.74 2.74L3 8"/><path d="M3 3v5h5"/></svg>
    </button>
    {#if showMenu}
      <!-- svelte-ignore a11y-click-events-have-key-events -->
      <div class="revert-backdrop" on:click={closeMenu}></div>
      <!-- svelte-ignore a11y-click-events-have-key-events -->
      <div class="revert-menu" style={menuStyle} on:click|stopPropagation>
        {#if !confirmAction}
          <button class="menu-item" on:click={() => pickAction('revertcode')}>Revert code</button>
          <button class="menu-item" on:click={() => pickAction('reverthistory')}>Revert history</button>
          <button class="menu-item" on:click={() => pickAction('fork')}>Fork from here</button>
        {:else}
          <div class="confirm-label">{labels[confirmAction]}?</div>
          <div class="confirm-actions">
            <button class="confirm-btn no" on:click={closeMenu}>No</button>
            <button class="confirm-btn yes" on:click={() => confirm(false)}>Yes</button>
            {#if confirmAction !== 'revertcode'}
              <button class="confirm-btn yes-code" on:click={() => confirm(true)}>Yes, and revert code</button>
            {/if}
          </div>
        {/if}
      </div>
    {/if}
  {/if}
  <div class="body">
    {#if role === 'assistant'}
      <!-- svelte-ignore a11y-click-events-have-key-events a11y-no-static-element-interactions -->
      <div class="md" class:wrap={$settings.wrapCode} on:click={handleMdClick}>{@html renderedHtml}</div>
    {:else}
      <!-- svelte-ignore a11y-click-events-have-key-events a11y-no-static-element-interactions -->
      <div class="plain" class:collapsed={!expanded} use:checkOverflow on:click={() => expanded = !expanded}>{content}</div>
      {#if overflows}
        <!-- svelte-ignore a11y-click-events-have-key-events a11y-no-static-element-interactions -->
        <div class="expand-hint" on:click={() => expanded = !expanded}>{expanded ? 'less' : 'more'}</div>
      {/if}
    {/if}
  </div>
</div>

<style>
  .message { position:relative; }
  .message.user { margin:10px 12px; padding:8px 12px; background:var(--bg-user-msg); border:1px solid var(--border); border-radius:var(--radius); }
  .message.assistant { padding:4px 12px; }
  .revert-icon { position:absolute; top:6px; right:6px; display:flex; align-items:center; background:none; border:none; color:var(--text-dim); cursor:pointer; padding:2px; visibility:hidden; z-index:1; }
  .message.user:hover .revert-icon { visibility:visible; }
  .revert-icon:hover { color:var(--accent); }
  .revert-backdrop { position:fixed; inset:0; z-index:50; }
  .revert-menu { position:fixed; background:var(--bg-elevated); border:1px solid var(--border-strong); min-width:180px; z-index:51; box-shadow:var(--shadow-menu); }
  .menu-item { display:block; width:100%; background:none; border:none; color:var(--text); font-family:var(--font-ui); font-size:calc(12px * var(--scale, 1)); padding:6px 12px; cursor:pointer; text-align:left; }
  .menu-item:hover { background:var(--accent-soft); color:var(--accent); }
  .confirm-label { padding:6px 12px; font-family:var(--font-ui); font-size:calc(12px * var(--scale, 1)); color:var(--text); border-bottom:1px solid var(--border); }
  .confirm-actions { display:flex; flex-wrap:wrap; gap:4px; padding:6px 8px; justify-content:flex-end; }
  .confirm-btn { background:none; border:1px solid var(--border-button); color:var(--text-dim); font-family:var(--font-ui); font-size:calc(12px * var(--scale, 1)); padding:4px 12px; cursor:pointer; }
  .confirm-btn.no:hover { border-color:var(--warn); color:var(--warn); }
  .confirm-btn.yes { border-color:var(--accent); color:var(--accent); }
  .confirm-btn.yes:hover { background:var(--accent-soft); }
  .confirm-btn.yes-code { border-color:var(--accent); color:var(--accent); }
  .confirm-btn.yes-code:hover { background:var(--accent-soft); }
  .body { font-family:var(--font-ui); font-size:calc(13px * var(--scale, 1)); line-height:1.6; }
  .plain { white-space:pre-wrap; word-break:break-word; cursor:pointer; }
  .plain.collapsed { max-height:calc(1.6em * 5); overflow:hidden; }
  .expand-hint { font-size:calc(11px * var(--scale, 1)); color:var(--text-dim); cursor:pointer; margin-top:2px; }
  .md :global(p) { margin:0 0 8px; }
  .md :global(p:last-child) { margin-bottom:0; }
  .md :global(code) { font-family:var(--font-mono); font-size:calc(12px * var(--scale, 1)); color:var(--code-inline); }
  .md :global(pre) { background:var(--bg-code); padding:10px; border-radius:var(--radius); overflow-x:auto; margin:8px 0; font-family:var(--font-mono); }
  .md :global(pre code) { background:none; padding:0; color:var(--code-inline); }
  .md.wrap :global(pre) { white-space:pre-wrap; word-break:break-word; overflow-wrap:anywhere; overflow-x:visible; }
  .md.wrap :global(pre code) { white-space:pre-wrap; word-break:break-word; overflow-wrap:anywhere; }
  .md :global(h1),.md :global(h2),.md :global(h3) { margin:12px 0 6px; }
  .md :global(ul),.md :global(ol) { padding-left:20px; margin:4px 0; }
  .md :global(blockquote) { border-left:3px solid var(--text-dim); padding-left:12px; color:var(--text-dim); margin:8px 0; }
  .md :global(a) { color:var(--accent); text-decoration:none; }
  .md :global(a:hover) { text-decoration:underline; }
</style>
