<script>
  import { ReadFileContent } from '../../wailsjs/go/main/App';
  import { openViewer, openEditPreview, openSubagentViewer } from '../lib/viewer.js';
  import { settings } from '../lib/settings.js';
  import { collapsedPreviewLines, maxInlineExpandLines } from '../lib/uiConstants.js';
  import EditPreview from './EditPreview.svelte';
  export let name = '';
  export let args = '';
  export let result = '';
  export let success = true;
  export let done = false;
  export let subagentSessionIds = [];
  export let metadata = null;

  let expanded = false;
  let commandExpanded = false;

  $: parsed = parseArgs(args);
  $: displayOutput = name === 'write_file' && success ? (typeof parsed.content === 'string' ? parsed.content : '') : (result || '');
  $: lines = displayOutput.split('\n');
  $: hasMore = lines.length > collapsedPreviewLines;
  $: preview = lines.slice(0, collapsedPreviewLines).join('\n');
  $: outputCanExpandInline = lines.length <= maxInlineExpandLines;
  $: shownOutput = expanded || !hasMore ? displayOutput : preview;
  $: editHunks = normalizeEditPreview(metadata && metadata.edit_preview);
  $: editRowCount = countEditRows(editHunks);
  $: editHasMore = editRowCount > collapsedPreviewLines;
  $: editCanExpandInline = editRowCount <= maxInlineExpandLines;
  $: editChangeCounts = countEditChanges(editHunks);
  $: writeAddedLines = name === 'write_file' ? countContentLines(displayOutput) : 0;
  $: commandHeaderLines = splitCommandLines(parsed.command || '');
  $: commandHasMore = commandHeaderLines.length > 1;
  $: commandHeader = commandHeaderLines.length === 0 ? '' : (commandExpanded || !commandHasMore ? commandHeaderLines.join('\n') : commandHeaderLines[0]);

  function parseArgs(s) {
    try { return JSON.parse(s); } catch { return {}; }
  }

  async function openPath(path) {
    if (!path) return;
    try {
      const content = await ReadFileContent(path);
      openViewer(path, content);
    } catch (e) {
      openViewer(path, 'Error: ' + (e?.message || e));
    }
  }

  function openOutput(title, content = displayOutput) {
    openViewer(title, content || '');
  }

  function toggleOrOpenOutput(title) {
    if (!hasMore) return;
    if (outputCanExpandInline) expanded = !expanded;
    else openOutput(title);
  }

  function toggleOrOpenEditPreview(title) {
    if (!editHasMore) return;
    if (editCanExpandInline) expanded = !expanded;
    else openEditPreview(title, editHunks);
  }

  function normalizeEditPreview(preview) {
    if (!preview || !Array.isArray(preview.hunks)) return [];
    return preview.hunks.map(hunk => ({
      rows: Array.isArray(hunk.rows) ? hunk.rows : []
    })).filter(hunk => hunk.rows.length > 0);
  }

  function countEditRows(hunks) {
    return (hunks || []).reduce((sum, hunk) => sum + ((hunk.rows || []).length), 0);
  }

  function countEditChanges(hunks) {
    let added = 0;
    let removed = 0;
    for (const hunk of hunks || []) {
      for (const row of hunk.rows || []) {
        if (row.kind === 'add') added++;
        else if (row.kind === 'remove') removed++;
      }
    }
    return { added, removed };
  }

  function countContentLines(content) {
    if (!content) return 0;
    const matches = content.match(/\n/g);
    const newlineCount = matches ? matches.length : 0;
    return content.endsWith('\n') ? newlineCount : newlineCount + 1;
  }

  function moreLinesLabel(n) {
    return n === 1 ? '(1 more line)' : `(${n} more lines)`;
  }

  function splitCommandLines(command) {
    if (!command) return [];
    const normalized = command.replace(/\r\n/g, '\n').replace(/\r/g, '\n');
    return normalized.replace(/\n+$/, '').split('\n');
  }
</script>

<div class="tool-call" class:error={done && !success} class:wrap={$settings.wrapCode}>
  {#if name === 'read_file' || name === 'write_file'}
    <div class="line">
      <span class="tool-name">{name}</span>
      <!-- svelte-ignore a11y-click-events-have-key-events a11y-no-static-element-interactions -->
      <span class="arg path" on:click={() => openPath(parsed.path)} title={parsed.path || ''}>{parsed.path || ''}</span>
      {#if name === 'write_file' && writeAddedLines > 0}<span class="change add">+{writeAddedLines}</span>{/if}
    </div>
    {#if name === 'read_file' && done && !success && result}
      <div class="output-block">
        <pre class="output">{result}</pre>
      </div>
    {/if}
    {#if name === 'write_file' && done}
      <div class="output-block">
        <pre class="output">{hasMore ? preview : displayOutput}</pre>
        {#if hasMore}<div class="more">({lines.length} lines)</div>{/if}
      </div>
    {/if}
  {:else if name === 'edit_file'}
    <div class="line">
      <span class="tool-name">{name}</span>
      <span class="arg">{parsed.path || ''}</span>
      {#if editChangeCounts.added > 0}<span class="change add">+{editChangeCounts.added}</span>{/if}
      {#if editChangeCounts.removed > 0}<span class="change remove">-{editChangeCounts.removed}</span>{/if}
    </div>
    {#if done && success && editHunks.length}
      <!-- svelte-ignore a11y-click-events-have-key-events a11y-no-static-element-interactions -->
      <div class="output-block edit-preview" class:expandable={editHasMore} on:click={() => toggleOrOpenEditPreview(parsed.path || 'edit_file output')}>
        <EditPreview hunks={editHunks} limit={expanded || !editHasMore ? 0 : collapsedPreviewLines} />
        {#if editHasMore && !expanded}<div class="more">show all ({editRowCount} lines)</div>{/if}
        {#if editHasMore && expanded}<div class="more">collapse</div>{/if}
      </div>
    {/if}
    {#if done && result && !success}
      <!-- svelte-ignore a11y-click-events-have-key-events a11y-no-static-element-interactions -->
      <div class="output-block" class:expandable={hasMore} on:click={() => toggleOrOpenOutput(parsed.path || 'edit_file output')}>
        <pre class="output">{shownOutput}</pre>
        {#if hasMore && !expanded}<div class="more">show all ({lines.length} lines)</div>{/if}
        {#if hasMore && expanded}<div class="more">collapse</div>{/if}
      </div>
    {/if}
  {:else if name === 'run_command' || name === 'process'}
    <div class="line">
      <span class="tool-name">{name}</span>
      <span class="arg" class:preserve={name === 'run_command'}>{name === 'run_command' ? ('$ ' + commandHeader) : (parsed.action || '') + (parsed.id ? ' ' + parsed.id : '')}</span>
      {#if name === 'run_command' && commandHasMore}
        <button type="button" class="inline-more" on:click={() => commandExpanded = !commandExpanded}>{commandExpanded ? 'collapse' : moreLinesLabel(commandHeaderLines.length - 1)}</button>
      {/if}
    </div>
    {#if done && result}
      <!-- svelte-ignore a11y-click-events-have-key-events a11y-no-static-element-interactions -->
      <div class="output-block" class:expandable={hasMore} on:click={() => toggleOrOpenOutput(parsed.command || parsed.action || 'output')}>
        <pre class="output">{shownOutput}</pre>
        {#if hasMore && !expanded}<div class="more">show all ({lines.length} lines)</div>{/if}
        {#if hasMore && expanded}<div class="more">collapse</div>{/if}
      </div>
    {/if}
  {:else if name === 'task'}
    <div class="line">
      <span class="tool-name">task</span>
      <span class="arg">{(parsed.tasks || []).length} subagent{(parsed.tasks || []).length !== 1 ? 's' : ''}</span>
    </div>
    {#if !done}
      {#each (parsed.tasks || []) as t, i}
        <!-- svelte-ignore a11y-click-events-have-key-events a11y-no-static-element-interactions -->
        <div class="subtask-row" on:click={() => { const s = (subagentSessionIds || []).find(x => x.index === i); if (s) openSubagentViewer(t.subagent_type + ': ' + (t.prompt || '').slice(0, 60), s.sessionId); }}>
          <span class="subtask-type">{t.subagent_type}</span>
          <span class="subtask-prompt">{(t.prompt || '').slice(0, 80)}</span>
        </div>
      {/each}
    {:else}
      <!-- svelte-ignore a11y-click-events-have-key-events a11y-no-static-element-interactions -->
      <div class="output-block" class:expandable={hasMore} on:click={() => toggleOrOpenOutput('task results')}>
        <pre class="output">{shownOutput}</pre>
        {#if hasMore && !expanded}<div class="more">show all ({lines.length} lines)</div>{/if}
        {#if hasMore && expanded}<div class="more">collapse</div>{/if}
      </div>
    {/if}
  {:else if name === 'save_memory'}
    <div class="line">
      <span class="tool-name">{name}</span>
      <span class="arg">{parsed.title || ''}</span>
    </div>
    {#if done && result}
      <pre class="output bare">{result}</pre>
    {/if}
  {:else if name === 'search_memory' || name === 'search_history'}
    <div class="line">
      <span class="tool-name">{name}</span>
      <span class="arg">{parsed.query || ''}</span>
    </div>
    {#if done && result}
      {#if hasMore}
        <!-- svelte-ignore a11y-click-events-have-key-events a11y-no-static-element-interactions -->
        <div class="output-block" class:expandable={hasMore} on:click={() => toggleOrOpenOutput(name + ' results')}>
          <pre class="output">{shownOutput}</pre>
          {#if !expanded}<div class="more">show all ({lines.length} lines)</div>{/if}
          {#if expanded}<div class="more">collapse</div>{/if}
        </div>
      {:else}
        <pre class="output bare">{result}</pre>
      {/if}
    {/if}
  {:else if name === 'diagnostics'}
    <div class="line">
      <span class="tool-name">{name}</span>
    </div>
    {#if done && result}
      {#if hasMore}
        <!-- svelte-ignore a11y-click-events-have-key-events a11y-no-static-element-interactions -->
        <div class="output-block" class:expandable={hasMore} on:click={() => toggleOrOpenOutput('diagnostics results')}>
          <pre class="output">{shownOutput}</pre>
          {#if !expanded}<div class="more">show all ({lines.length} lines)</div>{/if}
          {#if expanded}<div class="more">collapse</div>{/if}
        </div>
      {:else}
        <pre class="output bare">{result}</pre>
      {/if}
    {/if}
  {:else if name === 'workspace_symbol'}
    <div class="line">
      <span class="tool-name">{name}</span>
      <span class="arg">{parsed.query || ''}</span>
    </div>
    {#if done && result}
      {#if hasMore}
        <!-- svelte-ignore a11y-click-events-have-key-events a11y-no-static-element-interactions -->
        <div class="output-block" class:expandable={hasMore} on:click={() => toggleOrOpenOutput('workspace_symbol results')}>
          <pre class="output">{shownOutput}</pre>
          {#if !expanded}<div class="more">show all ({lines.length} lines)</div>{/if}
          {#if expanded}<div class="more">collapse</div>{/if}
        </div>
      {:else}
        <pre class="output bare">{result}</pre>
      {/if}
    {/if}
  {:else}
    <div class="line">
      <span class="tool-name">{name}</span>
      <span class="arg">{args}</span>
    </div>
    {#if done && result}
      <pre class="output bare">{result}</pre>
    {/if}
  {/if}
</div>

<style>
  .tool-call { margin: 2px 0 2px 16px; font-size: calc(12px * var(--scale, 1)); }
  .line { display:flex; align-items:center; gap:6px; padding:4px 8px; color:var(--text-dim); font-family:var(--font-mono); }
  .tool-name { color: var(--accent); }
  .arg { overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
  .tool-call.wrap .line { align-items:flex-start; flex-wrap:wrap; }
  .tool-call.wrap .arg { flex:0 1 auto; min-width:0; max-width:100%; overflow:visible; text-overflow:clip; white-space:normal; overflow-wrap:anywhere; word-break:break-word; }
  .arg.preserve { overflow:visible; text-overflow:clip; white-space:pre; }
  .tool-call.wrap .arg.preserve { white-space:pre-wrap; }
  .arg.path { cursor:pointer; }
  .arg.path:hover { color:var(--accent); text-decoration:underline; }
  .change { flex-shrink:0; font-family:var(--font-mono); }
  .change.add { color:var(--success); }
  .change.remove { color:var(--error); }
  .tool-call.error .line { color: var(--error); }
  .output-block { box-sizing:border-box; width:calc(100% - 16px); max-width:760px; margin:2px 8px 0; padding:6px 8px 6px 12px; background:var(--bg-code); border:1px solid var(--border); border-radius:4px; }
  .output-block.expandable { cursor:pointer; }
  .output-block.expandable:hover { border-color:var(--border-strong); }
  .output { margin:0; font-family:var(--font-mono); font-size:calc(12px * var(--scale, 1)); white-space:pre-wrap; word-break:break-all; color:var(--text); }
  .edit-preview { overflow:auto; }
  .output.bare { box-sizing:border-box; max-width:760px; padding:6px 8px 6px 12px; margin:2px 8px 0; background:var(--bg-code); border:1px solid var(--border); border-radius:4px; max-height:300px; overflow-y:auto; }
  .more { margin-top:4px; color:var(--text-dim); font-family:var(--font-ui); font-size:calc(11px * var(--scale, 1)); }
  .output-block.expandable:hover .more { color:var(--accent); }
  .inline-more { flex-shrink:0; align-self:flex-start; margin-top:1px; border:0; background:none; padding:0; color:var(--text-dim); font-family:var(--font-ui); font-size:calc(11px * var(--scale, 1)); cursor:pointer; }
  .inline-more:hover { color:var(--accent); }
  .subtask-row { display:flex; gap:8px; padding:3px 8px 3px 24px; cursor:pointer; color:var(--text-dim); font-family:var(--font-mono); font-size:calc(12px * var(--scale, 1)); }
  .subtask-row:hover { color:var(--accent); }
  .subtask-type { color:var(--accent); flex-shrink:0; }
  .subtask-prompt { overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
</style>
