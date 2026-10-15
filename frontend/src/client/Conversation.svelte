<script>
  import { afterUpdate, createEventDispatcher } from 'svelte';
  import { renderMarkdown } from '../lib/markdown.js';
  import { settings } from '../lib/settings.js';
  import { BrowserOpenURL } from '../../client-bindings/wailsjs/runtime/runtime';
  import CopyMarkdown from './CopyMarkdown.svelte';
  import ToolCallBlock from './ToolCallBlock.svelte';
  export let items = [];
  export let activity = null; // ActivityState | null
  export let activeOperation = null; // Operation | null
  export let busy = false;
  export let pending = { steering: [], queued: [] };
  export let error = '';
  export let olderAvailable = false;
  export let viewKey = ''; // root/child + connection generation identity
  // Fork actions exist only on the root view; the child inspector stays passive.
  export let forksEnabled = false;
  const dispatch = createEventDispatcher();

  let el;
  let userScrolled = false;
  let lastViewKey = viewKey;

  // A view-identity change resets following; scroll within the same view is kept.
  $: if (viewKey !== lastViewKey) {
    lastViewKey = viewKey;
    userScrolled = false;
  }

  function onScroll() {
    if (!el) return;
    userScrolled = el.scrollHeight - el.scrollTop - el.clientHeight > 50;
  }

  afterUpdate(() => { if (!userScrolled && el) el.scrollTop = el.scrollHeight; });

  function contentText(part) {
    if (part.kind === 'text') return part.text || '';
    if (part.kind === 'image_url') return part.url || '';
    if (part.kind === 'opaque') {
      // Opaque content stays raw display text; no control interpretation.
      const extra = part.extra === undefined ? '' : JSON.stringify(part.extra);
      return `[opaque ${part.opaque_wire_type || ''}${extra ? ' ' + extra : ''}]`;
    }
    return '';
  }

  function itemContent(item) {
    return (item.content || []).map(contentText).join('\n');
  }

  // The copy action carries the canonical text parts only; image URLs and
  // synthesized opaque text are display-only.
  function canonicalText(item) {
    return (item.content || [])
      .filter((part) => part.kind === 'text')
      .map((part) => part.text || '')
      .join('\n');
  }

  // A safe Markdown anchor opens in the system browser; the webview itself
  // never navigates. Every other click stays inert.
  function handleMdClick(e) {
    const a = e.target.closest('a');
    if (!a) return;
    e.preventDefault();
    const href = a.getAttribute('href') || '';
    if (href) BrowserOpenURL(href);
  }

  function signalLabel(item) {
    if (item.subtype === 'interruption') return 'Interrupted';
    if (item.subtype === 'model_failure_continuation') return 'Continuing after a model failure';
    if (item.subtype === 'background_completion') return 'Background completed';
    return 'Signal';
  }

  function endLabel(item) {
    if (item.status === 'success') return 'completed';
    return item.status || '';
  }

  function toolCalls(item) {
    return item.tool_calls || [];
  }

  function relatedText(item) {
    const member = item.related_member;
    if (!member) return '';
    return member.kind === 'job' ? `job ${member.id}` : `child ${member.id}`;
  }

  let renderedMarkdown = new Map();
  $: {
    const next = new Map();
    for (const item of items) {
      if (item.kind === 'assistant') {
        const text = itemContent(item);
        if (text) next.set(item.item_id, renderMarkdown(text));
      }
    }
    renderedMarkdown = next;
  }
</script>

<div class="message-list" bind:this={el} on:scroll={onScroll}>
  {#each items as item (item.item_id)}
    {#if item.kind === 'input'}
      <div class="message user">
        <div class="body plain">{itemContent(item)}</div>
        {#if forksEnabled && item.origin === 'user'}
          <button class="fork" on:click={() => dispatch('fork', item)} aria-label={`Fork from input ${item.item_id}`}>Fork</button>
        {/if}
      </div>
    {:else if item.kind === 'assistant'}
      <div class="message assistant">
        {#if canonicalText(item)}
          <CopyMarkdown text={canonicalText(item)} />
        {/if}
        <div class="source">{item.source || ''}{item.status && item.status !== 'completed' ? ` · ${item.status}` : ''}</div>
        {#if renderedMarkdown.get(item.item_id)}
          <!-- svelte-ignore a11y-click-events-have-key-events a11y-no-static-element-interactions -->
          <div class="body md" class:wrap={$settings.wrapCode} on:click={handleMdClick}>{@html renderedMarkdown.get(item.item_id)}</div>
        {/if}
        {#if item.refusal}
          <div class="refusal">Refused: {item.refusal}</div>
        {/if}
        {#each toolCalls(item) as call (call.id)}
          <ToolCallBlock {call} />
        {/each}
        {#if item.extra}
          <div class="opaque-field"><span class="opaque-label">extra</span> {JSON.stringify(item.extra)}</div>
        {/if}
      </div>
    {:else if item.kind === 'signal'}
      <div class="system-msg">
        <span class="signal-label">{signalLabel(item)}</span>
        {#if relatedText(item)}<span class="signal-related"> ({relatedText(item)})</span>{/if}
        {#if item.content}<div class="signal-content">{item.content}</div>{/if}
      </div>
    {:else if item.kind === 'compaction'}
      <div class="system-msg compaction">
        <span class="signal-label">Compaction summary</span><span class="signal-related"> ({item.model || 'model'})</span>
        <div class="signal-content">{item.summary}</div>
      </div>
    {:else if item.kind === 'operation_end'}
      <div class="system-msg op-end">
        <span class="signal-label">Operation {endLabel(item)}</span>
        {#if item.detail}<div class="signal-content">{item.detail}</div>{/if}
      </div>
    {:else}
      <div class="system-msg opaque-item">{JSON.stringify(item)}</div>
    {/if}
  {/each}
  {#each pending.steering as queued}
    <div class="queued-msg steering">
      <span class="pending-label">Steering</span>
      <span class="pending-text">{queued.content.map(contentText).join('\n')}</span>
    </div>
  {/each}
  {#each pending.queued as queued}
    <div class="queued-msg">
      <span class="pending-label">Queued</span>
      <span class="pending-text">{queued.content.map(contentText).join('\n')}</span>
    </div>
  {/each}
  {#if activity && (activity.pieces.length > 0 || activity.tools.length > 0)}
    <div class="activity-area">
      <div class="activity-identity">Model activity — provisional</div>
      {#each activity.pieces as piece, index (index)}
        {#if piece.kind === 'refusal'}
          <div class="provisional-refusal">Refusing: {piece.content}</div>
        {:else}
          <div class="provisional-text">{piece.content}</div>
        {/if}
      {/each}
      {#if activity.truncated}<div class="truncation">… earlier activity truncated</div>{/if}
      {#each activity.tools as tool (tool.callID)}
        <div class="activity-tool"><span class="activity-tool-name">{tool.name || 'tool'}</span>{tool.status ? ` · ${tool.status}` : ' · running'}</div>
      {/each}
    </div>
  {:else if busy}
    <!-- Empty Thinking/Compacting stays generic: it names no active transport
         model, only that a running Operation is working. -->
    <div class="activity-label">{activeOperation && activeOperation.request_kind === 'compact' ? 'Compacting…' : 'Thinking…'}</div>
  {/if}
  {#if error}
    <div class="error-msg">{error}</div>
  {/if}
  {#if olderAvailable}
    <button type="button" class="load-older" on:click={() => dispatch('older')} aria-label="Load older messages">Load older messages</button>
  {/if}
</div>

<style>
  .message-list { flex:1; overflow-y:auto; padding:8px 0; min-width:0; }
  .message { position:relative; }
  .message.user { margin:10px 12px; padding:8px 12px; background:var(--bg-user-msg); border:1px solid var(--border); border-radius:var(--radius); }
  .fork { margin-top:4px; background:none; border:1px solid var(--border-button); border-radius:var(--radius); color:var(--text-dim); font-family:var(--font-ui); font-size:calc(11px * var(--scale, 1)); padding:2px 10px; cursor:pointer; }
  .fork:hover { color:var(--accent); border-color:var(--accent); }
  .message.assistant { padding:4px 12px; }
  .body { font-family:var(--font-ui); font-size:calc(13px * var(--scale, 1)); line-height:1.6; }
  .plain { white-space:pre-wrap; word-break:break-word; }
  .source { padding:2px 0; color:var(--text-dim); font-size:calc(11px * var(--scale, 1)); font-family:var(--font-mono); }
  .refusal { margin:2px 0; padding:4px 8px; color:var(--warn); border-left:3px solid var(--warn); font-family:var(--font-ui); font-size:calc(13px * var(--scale, 1)); white-space:pre-wrap; word-break:break-word; }
  .system-msg { padding:4px 12px 4px 24px; margin:4px 12px; color:var(--text-dim); font-size:calc(13px * var(--scale, 1)); font-style:italic; }
  .signal-label { color:var(--text-dim); }
  .signal-related { color:var(--text-dim); font-style:normal; font-family:var(--font-mono); font-size:calc(11px * var(--scale, 1)); }
  .signal-content { font-style:normal; white-space:pre-wrap; word-break:break-word; }
  .compaction, .op-end { border-left:3px solid var(--border-strong); }
  .error-msg { padding:8px 12px; margin:4px 12px; background:var(--error-soft); border-left:3px solid var(--error); border-radius:var(--radius); color:var(--error); font-size:calc(12px * var(--scale, 1)); white-space:pre-wrap; }
  .queued-msg { display:flex; align-items:flex-start; gap:8px; margin:10px 12px; padding:8px 12px; background:var(--bg-user-msg); border:1px solid var(--border); border-radius:var(--radius); font-family:var(--font-ui); font-size:calc(13px * var(--scale, 1)); line-height:1.6; opacity:0.6; }
  .queued-msg.steering { border-color:var(--border-strong); }
  .pending-label { flex-shrink:0; color:var(--text-dim); font-family:var(--font-ui); font-size:calc(11px * var(--scale, 1)); text-transform:uppercase; letter-spacing:.5px; }
  .pending-text { min-width:0; white-space:pre-wrap; word-break:break-word; }
  .activity-area { margin:4px 12px; padding:8px 12px; border:1px dashed var(--border-strong); border-radius:var(--radius); }
  .activity-identity { color:var(--text-dim); font-family:var(--font-ui); font-size:calc(11px * var(--scale, 1)); text-transform:uppercase; letter-spacing:.5px; margin-bottom:4px; }
  .provisional-text { font-family:var(--font-ui); font-size:calc(13px * var(--scale, 1)); line-height:1.6; white-space:pre-wrap; word-break:break-word; }
  .provisional-refusal { font-family:var(--font-ui); font-size:calc(13px * var(--scale, 1)); line-height:1.6; white-space:pre-wrap; word-break:break-word; color:var(--warn); }
  .truncation { color:var(--text-dim); font-size:calc(11px * var(--scale, 1)); }
  .activity-label { padding:6px 12px 6px 24px; color:var(--text-dim); font-family:var(--font-ui); font-size:calc(13px * var(--scale, 1)); }
  .activity-tool { padding:2px 0; color:var(--text-dim); font-family:var(--font-mono); font-size:calc(12px * var(--scale, 1)); }
  .activity-tool-name { color:var(--accent); }
  .opaque-field { margin:2px 0; padding:4px 8px; color:var(--text-dim); font-family:var(--font-mono); font-size:calc(11px * var(--scale, 1)); white-space:pre-wrap; word-break:break-all; }
  .opaque-item { white-space:pre-wrap; word-break:break-all; }
  .opaque-label { color:var(--accent); }
  .load-older { display:block; margin:4px auto; background:none; border:1px solid var(--border-button); border-radius:var(--radius); color:var(--text-dim); font-family:var(--font-ui); font-size:calc(12px * var(--scale, 1)); padding:4px 12px; cursor:pointer; }
  .load-older:hover { color:var(--accent); border-color:var(--accent); }
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
