<script>
  import { settings } from '../lib/settings.js';
  import { collapsedPreviewLines } from '../lib/uiConstants.js';
  import EditPreview from '../components/EditPreview.svelte';
  import { presentNumber } from './exactjson';
  export let call = {}; // ToolCallView

  // Three independent expansion states: arguments, output, edit preview.
  let argsExpanded = false;
  let outputExpanded = false;
  let editExpanded = false;

  $: content = call.content || '';
  $: outputLines = content.split('\n');
  $: outputHasMore = outputLines.length > collapsedPreviewLines;
  $: outputShown = outputExpanded || !outputHasMore ? content : outputLines.slice(0, collapsedPreviewLines).join('\n');
  $: argumentsText = formatArguments(call.arguments, call.normalized_arguments);
  $: argumentLines = argumentsText.split('\n');
  $: argsHasMore = argumentLines.length > collapsedPreviewLines;
  $: argsShown = argsExpanded || !argsHasMore ? argumentsText : argumentLines.slice(0, collapsedPreviewLines).join('\n');
  $: editHunks = (recognizePreview(call.metadata && call.metadata.edit_preview) || []).filter((hunk) => hunk.rows.length > 0);
  $: editRowCount = countEditRows(editHunks);
  $: editHasMore = editRowCount > collapsedPreviewLines;
  // edit_preview_files: each file is its own collapse unit, keyed by index.
  $: editFiles = recognizeEditFiles(call.metadata && call.metadata.edit_preview_files);
  $: extraText = serializeExtra(call.extra);
  $: metadataText = serializeExtra(call.metadata);
  $: statusLabel = call.status ? ` · ${call.status}` : '';

  let filesExpanded = {};
  function toggleFile(index) { filesExpanded = { ...filesExpanded, [index]: filesExpanded[index] !== true }; }

  function formatArguments(argumentsText, normalized) {
    if (normalized !== undefined && normalized !== null) {
      // Normalized arguments win when present; plain text, decoded JSON only.
      return JSON.stringify(normalized, null, 1);
    }
    return argumentsText || '';
  }

  function serializeExtra(extra) {
    if (extra === undefined || extra === null) return '';
    return JSON.stringify(extra);
  }

  // The consumed row shape: absent or finite.
  function isOptionalFiniteNumber(value) {
    return value === undefined || (typeof value === 'number' && Number.isFinite(value));
  }

  // Validate the ONE consumed preview shape before EditPreview: a non-matching
  // candidate yields no preview, never an exception or guessed rows; unknown
  // members stay visible in the unchanged raw metadata.
  function recognizePreview(value) {
    if (value === null || typeof value !== 'object' || Array.isArray(value)) return null;
    const hunks = value.hunks;
    if (!Array.isArray(hunks)) return null;
    const out = [];
    for (const hunk of hunks) {
      if (hunk === null || typeof hunk !== 'object' || Array.isArray(hunk)) return null;
      const rows = hunk.rows;
      if (!Array.isArray(rows)) return null;
      const kept = [];
      for (const row of rows) {
        if (row === null || typeof row !== 'object' || Array.isArray(row)) return null;
        if (row.kind !== 'context' && row.kind !== 'add' && row.kind !== 'remove') return null;
        if (typeof row.text !== 'string') return null;
        // Exact numeric tokens are interpreted for presentation only; the raw
        // metadata stays untouched and a nonfinite token yields no preview.
        const oldLine = presentNumber(row.oldLine);
        const newLine = presentNumber(row.newLine);
        if (!isOptionalFiniteNumber(oldLine)) return null;
        if (!isOptionalFiniteNumber(newLine)) return null;
        kept.push({ ...row, oldLine, newLine });
      }
      out.push({ rows: kept });
    }
    return out;
  }

  // Multi-file candidates require a nonempty path and an A/M/D op; no fabricated default.
  function recognizeEditFiles(files) {
    if (!Array.isArray(files)) return [];
    const out = [];
    for (const file of files) {
      if (file === null || typeof file !== 'object' || Array.isArray(file)) continue;
      if (typeof file.path !== 'string' || file.path === '') continue;
      if (file.op !== 'A' && file.op !== 'M' && file.op !== 'D') continue;
      const hunks = (recognizePreview(file.preview) || []).filter((hunk) => hunk.rows.length > 0);
      if (hunks.length === 0) continue;
      out.push({ path: file.path, op: file.op, hunks });
    }
    return out;
  }

  function countEditRows(hunks) {
    return hunks.reduce((sum, hunk) => sum + hunk.rows.length, 0);
  }
</script>

<div class="tool-call" class:error={call.status === 'error'} class:wrap={$settings.wrapCode}>
  <div class="line">
    <span class="tool-name">{call.name || 'tool'}</span>
    {#if statusLabel}<span class="status">{statusLabel}</span>{/if}
  </div>
  {#if argumentsText}
    <div class="output-block">
      <pre class="output">{argsShown}</pre>
      {#if argsHasMore}
        <button type="button" class="more-btn" aria-expanded={argsExpanded}
          on:click={() => (argsExpanded = !argsExpanded)}>{argsExpanded ? 'collapse arguments' : 'show all arguments'} ({argumentLines.length} lines)</button>
      {/if}
    </div>
  {/if}
  {#if content}
    <div class="output-block">
      <pre class="output">{outputShown}</pre>
      {#if outputHasMore}
        <button type="button" class="more-btn" aria-expanded={outputExpanded}
          on:click={() => (outputExpanded = !outputExpanded)}>{outputExpanded ? 'collapse output' : 'show all output'} ({outputLines.length} lines)</button>
      {/if}
    </div>
  {/if}
  {#if editHunks.length}
    <div class="output-block edit-preview">
      <EditPreview hunks={editHunks} limit={editExpanded || !editHasMore ? 0 : collapsedPreviewLines} />
      {#if editHasMore}
        <button type="button" class="more-btn" aria-expanded={editExpanded}
          on:click={() => (editExpanded = !editExpanded)}>{editExpanded ? 'collapse preview' : 'show all preview'} ({editRowCount} lines)</button>
      {/if}
    </div>
  {/if}
  {#each editFiles as file, index (index)}
    {@const fileRowCount = countEditRows(file.hunks)}
    {@const fileHasMore = fileRowCount > collapsedPreviewLines}
    <div class="output-block edit-preview">
      <div class="file-label"><span class="file-op">{file.op}</span> <span class="file-path">{file.path}</span></div>
      <EditPreview hunks={file.hunks} limit={filesExpanded[index] === true || !fileHasMore ? 0 : collapsedPreviewLines} />
      {#if fileHasMore}
        <button type="button" class="more-btn" aria-expanded={filesExpanded[index] === true}
          on:click={() => toggleFile(index)}>{filesExpanded[index] === true ? 'collapse preview' : 'show all preview'} ({fileRowCount} lines)</button>
      {/if}
    </div>
  {/each}
  {#if extraText}
    <div class="opaque"><span class="opaque-label">extra</span> {extraText}</div>
  {/if}
  {#if metadataText}
    <div class="opaque"><span class="opaque-label">metadata</span> {metadataText}</div>
  {/if}
</div>

<style>
  .tool-call { margin: 2px 0 2px 16px; font-size: calc(12px * var(--scale, 1)); }
  .line { display:flex; align-items:center; gap:6px; padding:4px 8px; color:var(--text-dim); font-family:var(--font-mono); }
  .tool-name { color: var(--accent); }
  .status { color: var(--text-dim); }
  .tool-call.error .line { color: var(--error); }
  .tool-call.wrap .line { align-items:flex-start; flex-wrap:wrap; }
  .output-block { box-sizing:border-box; width:calc(100% - 16px); max-width:760px; margin:2px 8px 0; padding:6px 8px 6px 12px; background:var(--bg-code); border:1px solid var(--border); border-radius:4px; }
  .output { margin:0; font-family:var(--font-mono); font-size:calc(12px * var(--scale, 1)); white-space:pre-wrap; word-break:break-all; color:var(--text); }
  .edit-preview { overflow:auto; }
  .file-label { display:flex; gap:6px; align-items:center; font-family:var(--font-mono); font-size:calc(11px * var(--scale, 1)); margin-bottom:2px; }
  .file-op { color:var(--accent); font-weight:bold; }
  .file-path { color:var(--text); word-break:break-all; }
  .more-btn { display:block; margin-top:4px; border:0; background:none; padding:0; color:var(--text-dim); font-family:var(--font-ui); font-size:calc(11px * var(--scale, 1)); cursor:pointer; text-align:left; }
  .more-btn:hover { color:var(--accent); }
  .more-btn:focus-visible { outline:1px solid var(--accent); outline-offset:1px; }
  .opaque { margin:2px 8px 0; padding:4px 8px; color:var(--text-dim); font-family:var(--font-mono); font-size:calc(11px * var(--scale, 1)); white-space:pre-wrap; word-break:break-all; }
  .opaque-label { color:var(--accent); }
</style>
