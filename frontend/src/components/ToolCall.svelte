<script>
  import { ReadFileContent } from '../../wailsjs/go/main/App';
  import { openViewer, openSubagentViewer } from '../lib/viewer.js';
  export let name = '';
  export let args = '';
  export let result = '';
  export let success = true;
  export let done = false;
  export let subagentSessionIds = [];
  export let metadata = null;

  let expanded = false;

  $: parsed = parseArgs(args);
  $: displayOutput = name === 'write_file' && success ? (typeof parsed.content === 'string' ? parsed.content : '') : (result || '');
  $: lines = displayOutput.split('\n');
  $: hasMore = lines.length > 3;
  $: preview = lines.slice(0, 3).join('\n');
  $: diff = (metadata && metadata.diff) || '';

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
</script>

<div class="tool-call" class:error={done && !success}>
  {#if name === 'read_file' || name === 'write_file'}
    <div class="line">
      <span class="tool-name">{name}</span>
      <!-- svelte-ignore a11y-click-events-have-key-events a11y-no-static-element-interactions -->
      <span class="arg path" on:click={() => openPath(parsed.path)} title={parsed.path || ''}>{parsed.path || ''}</span>
    </div>
    {#if name === 'read_file' && done && !success && result}
      <div class="output-block">
        <pre class="output">{result}</pre>
      </div>
    {/if}
    {#if name === 'write_file' && done}
      <!-- svelte-ignore a11y-click-events-have-key-events a11y-no-static-element-interactions -->
      <div class="output-block" class:expandable={hasMore} on:click={() => { if (hasMore) openOutput(parsed.path || 'write_file output'); }}>
        <pre class="output">{hasMore ? preview : displayOutput}</pre>
        {#if hasMore}<div class="more">show all ({lines.length} lines)</div>{/if}
      </div>
    {/if}
  {:else if name === 'edit_file'}
    <div class="line">
      <span class="tool-name">{name}</span>
      <span class="arg">{parsed.path || ''}</span>
    </div>
    {#if done && success && diff}
      <div class="diff-block">
        <pre class="diff">{diff}</pre>
      </div>
    {/if}
    {#if done && result}
      <!-- svelte-ignore a11y-click-events-have-key-events a11y-no-static-element-interactions -->
      <div class="output-block" class:expandable={hasMore} on:click={() => { if (hasMore) expanded = !expanded; }}>
        <pre class="output">{expanded || !hasMore ? result : preview}</pre>
        {#if hasMore && !expanded}<div class="more">show all ({lines.length} lines)</div>{/if}
        {#if hasMore && expanded}<div class="more">collapse</div>{/if}
      </div>
    {/if}
  {:else if name === 'run_command' || name === 'process'}
    <div class="line">
      <span class="tool-name">{name}</span>
      <span class="arg">{name === 'run_command' ? (parsed.command || '') : (parsed.action || '') + (parsed.id ? ' ' + parsed.id : '')}</span>
    </div>
    {#if done && result}
      <!-- svelte-ignore a11y-click-events-have-key-events a11y-no-static-element-interactions -->
      <div class="output-block" class:expandable={hasMore} on:click={() => { if (hasMore) openOutput(parsed.command || parsed.action || 'output'); }}>
        <pre class="output">{hasMore ? preview : result}</pre>
        {#if hasMore}<div class="more">show all ({lines.length} lines)</div>{/if}
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
      <div class="output-block" class:expandable={hasMore} on:click={() => { if (hasMore) openOutput('task results'); }}>
        <pre class="output">{hasMore ? preview : result}</pre>
        {#if hasMore}<div class="more">show all ({lines.length} lines)</div>{/if}
      </div>
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
  .arg.path { cursor:pointer; }
  .arg.path:hover { color:var(--accent); text-decoration:underline; }
  .tool-call.error .line { color: var(--error); }
  .output-block { width:calc(100% - 16px); margin:2px 8px 0; padding:6px 8px; background:var(--bg-code); border:1px solid var(--border); border-radius:4px; }
  .output-block.expandable { cursor:pointer; }
  .output-block.expandable:hover { border-color:var(--border-strong); }
  .output { margin:0; font-family:var(--font-mono); font-size:calc(12px * var(--scale, 1)); white-space:pre-wrap; word-break:break-all; color:var(--text); }
  .diff-block { width:calc(100% - 16px); margin:2px 8px 0; padding:6px 8px; background:var(--bg-code); border:1px solid var(--border); border-radius:4px; }
  .diff { margin:0; font-family:var(--font-mono); font-size:calc(12px * var(--scale, 1)); white-space:pre-wrap; word-break:break-all; color:var(--text-dim); }
  .output.bare { padding:6px 8px; margin:2px 8px 0; background:var(--bg-code); border:1px solid var(--border); border-radius:4px; max-height:300px; overflow-y:auto; }
  .more { margin-top:4px; color:var(--text-dim); font-family:var(--font-ui); font-size:calc(11px * var(--scale, 1)); }
  .output-block.expandable:hover .more { color:var(--accent); }
  .subtask-row { display:flex; gap:8px; padding:3px 8px 3px 24px; cursor:pointer; color:var(--text-dim); font-family:var(--font-mono); font-size:calc(12px * var(--scale, 1)); }
  .subtask-row:hover { color:var(--accent); }
  .subtask-type { color:var(--accent); flex-shrink:0; }
  .subtask-prompt { overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
</style>
