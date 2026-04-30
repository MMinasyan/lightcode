<script>
  import { createEventDispatcher } from 'svelte';
  export let provider = '';
  export let model = '';
  export let sessionId = '';
  export let projectName = '';
  export let tokens = { total: { cache:0, input:0, output:0, known:true }, perModel: [], contextUsed: 0, contextWindow: 0 };
  export let compacting = false;
  export let busy = false;
  export let warnings = [];
  const dispatch = createEventDispatcher();

  function fmt(n, known) {
    if (!known) return '-';
    if (n < 1000) return String(n);
    if (n < 1_000_000) return (n/1000).toFixed(1).replace(/\.0$/, '') + 'k';
    if (n < 1_000_000_000) return (n/1_000_000).toFixed(1).replace(/\.0$/, '') + 'M';
    return (n/1_000_000_000).toFixed(1).replace(/\.0$/, '') + 'B';
  }

  $: contextUsed = tokens.contextUsed || 0;
  $: contextWindow = tokens.contextWindow || 0;
  $: pct = contextWindow > 0 ? contextUsed / contextWindow : 0;
  $: showContext = contextWindow > 0;
  $: canCompact = !busy && !compacting && contextUsed > 0;
  $: contextTitle = showContext
    ? fmt(contextUsed, true) + '/' + fmt(contextWindow, true) + ' context used\n' + (compacting ? 'compacting...' : 'click to compact session')
    : '';

  // SVG circle math: radius=8, circumference=2*pi*8≈50.27
  const R = 8;
  const C = 2 * Math.PI * R;
  $: dashOffset = C - C * Math.min(pct, 1);
</script>

<div class="toolbar">
  <!-- svelte-ignore a11y-click-events-have-key-events -->
  <div class="label project clickable"
    on:click={() => dispatch('openProjectSelector')}
    role="button" tabindex="0"
    title={projectName || ''}>{projectName || '-'}</div>
  <!-- svelte-ignore a11y-click-events-have-key-events -->
  <div class="label session clickable"
    on:click={() => dispatch('openSessionSelector')}
    role="button" tabindex="0"
    title={sessionId || 'No session'}>
    {sessionId || 'new session'}
  </div>
  <div class="spacer"></div>
  {#if warnings.length > 0}
    <!-- svelte-ignore a11y-click-events-have-key-events -->
    <div class="warn-icon" on:click={() => dispatch('openWarnings')} role="button" tabindex="0" title="{warnings.length} warning{warnings.length > 1 ? 's' : ''}">
      <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M10.29 3.86L1.82 18a2 2 0 0 0 1.71 3h16.94a2 2 0 0 0 1.71-3L13.71 3.86a2 2 0 0 0-3.42 0z"/><line x1="12" y1="9" x2="12" y2="13"/><line x1="12" y1="17" x2="12.01" y2="17"/></svg>
    </div>
  {/if}
  {#if showContext}
    <!-- svelte-ignore a11y-click-events-have-key-events -->
    <div class="context-ring" class:clickable={canCompact}
      on:click={() => canCompact && dispatch('compact')}
      role="button" tabindex="0"
      title={contextTitle}>
      <svg width="20" height="20" viewBox="0 0 20 20">
        <circle cx="10" cy="10" r={R} fill="none" stroke="var(--border-strong)" stroke-width="2.5" />
        <circle cx="10" cy="10" r={R} fill="none"
          stroke={pct >= 0.8 ? 'var(--warning, #e6a700)' : 'var(--accent)'}
          stroke-width="2.5"
          stroke-dasharray={C}
          stroke-dashoffset={dashOffset}
          stroke-linecap="round"
          transform="rotate(-90 10 10)" />
      </svg>
    </div>
  {/if}
  <!-- svelte-ignore a11y-click-events-have-key-events -->
  <div class="tokens"
    on:click={() => dispatch('openTokens')}
    role="button" tabindex="0">
    <span class="tok">⚡ {fmt(tokens.total.cache, tokens.total.known)}</span>
    <span class="tok">↑ {fmt(tokens.total.input, tokens.total.known)}</span>
    <span class="tok">↓ {fmt(tokens.total.output, tokens.total.known)}</span>
  </div>
  <!-- svelte-ignore a11y-click-events-have-key-events -->
  <button class="settings-btn" on:click={() => dispatch('openSettings')} title="Settings">
    <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="3"/><path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 0 1 0 2.83 2 2 0 0 1-2.83 0l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 0 1-2 2 2 2 0 0 1-2-2v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 0 1-2.83 0 2 2 0 0 1 0-2.83l.06-.06a1.65 1.65 0 0 0 .33-1.82 1.65 1.65 0 0 0-1.51-1H3a2 2 0 0 1-2-2 2 2 0 0 1 2-2h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 0 1 0-2.83 2 2 0 0 1 2.83 0l.06.06a1.65 1.65 0 0 0 1.82.33H9a1.65 1.65 0 0 0 1-1.51V3a2 2 0 0 1 2-2 2 2 0 0 1 2 2v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 0 1 2.83 0 2 2 0 0 1 0 2.83l-.06.06a1.65 1.65 0 0 0-.33 1.82V9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 0 1 2 2 2 2 0 0 1-2 2h-.09a1.65 1.65 0 0 0-1.51 1z"/></svg>
  </button>
</div>

<style>
  .toolbar { display:flex; align-items:center; gap:10px; padding:6px 12px; background:var(--bg-elevated); border-bottom:1px solid var(--border); min-height:36px; }
  .spacer { flex:1; }
  .label { padding:2px 8px; border:1px solid var(--border-strong); border-radius:4px; color:var(--text-dim); font-family:var(--font-ui); font-size:12px; white-space:nowrap; overflow:hidden; text-overflow:ellipsis; }
  .label.project { width:140px; }
  .label.session { width:120px; }
  .label.clickable { cursor:pointer; }
  .label.clickable:hover { border-color:var(--accent); color:var(--text); }
  .tokens { display:flex; gap:8px; padding:2px 8px; border:1px solid var(--border-strong); border-radius:4px; color:var(--text-dim); font-family:var(--font-ui); font-size:12px; cursor:pointer; }
  .tokens:hover { border-color:var(--accent); color:var(--text); }
  .tok { white-space:nowrap; }
  .context-ring { display:flex; align-items:center; flex-shrink:0; }
  .context-ring.clickable { cursor:pointer; }
  .context-ring.clickable:hover svg circle:last-child { opacity:0.8; }
  .warn-icon { display:flex; align-items:center; color:var(--warning, #e6a700); cursor:pointer; flex-shrink:0; }
  .warn-icon:hover { opacity:0.8; }
  .settings-btn { display:flex; align-items:center; background:none; border:none; color:var(--text-dim); cursor:pointer; padding:2px; }
  .settings-btn:hover { color:var(--accent); }
</style>
