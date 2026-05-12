<script>
  export let hunks = [];
  export let limit = 0;

  $: lineWidth = lineNumberWidth(hunks);
  $: visibleHunks = limit > 0 ? limitHunks(hunks, limit) : hunks;

  function lineNumber(row) {
    return row.kind === 'remove' ? (row.oldLine || 0) : (row.newLine || 0);
  }

  function marker(row) {
    if (row.kind === 'remove') return '-';
    if (row.kind === 'add') return '+';
    return ' ';
  }

  function gutter(row) {
    return String(lineNumber(row)).padStart(lineWidth, ' ') + ' ' + marker(row);
  }

  function lineNumberWidth(sourceHunks) {
    let width = 1;
    for (const hunk of sourceHunks || []) {
      for (const row of hunk.rows || []) {
        const line = lineNumber(row);
        if (line > 0) width = Math.max(width, String(line).length);
      }
    }
    return width;
  }

  function limitHunks(sourceHunks, maxRows) {
    const out = [];
    let remaining = maxRows;
    for (const hunk of sourceHunks || []) {
      if (remaining <= 0) break;
      const rows = (hunk.rows || []).slice(0, remaining);
      if (rows.length > 0) {
        out.push({ rows });
        remaining -= rows.length;
      }
    }
    return out;
  }
</script>

<div class="edit-preview-rows">
  {#each visibleHunks as hunk, hunkIndex}
    {#if hunkIndex > 0 && limit <= 0}<div class="edit-gap"></div>{/if}
    {#each hunk.rows as row}
      <div class="edit-row" class:add={row.kind === 'add'} class:remove={row.kind === 'remove'}>
        <span class="edit-gutter" class:add={row.kind === 'add'} class:remove={row.kind === 'remove'}>{gutter(row)}</span><span class="edit-code">{row.text}</span>
      </div>
    {/each}
  {/each}
</div>

<style>
  .edit-preview-rows { min-width:100%; }
  .edit-row { display:flex; margin:0; font-family:var(--font-mono); font-size:calc(12px * var(--scale, 1)); white-space:pre; }
  .edit-row.add { background:rgba(80,250,123,.08); }
  .edit-row.remove { background:rgba(255,107,107,.10); }
  .edit-gutter { flex:0 0 auto; color:var(--text-dim); }
  .edit-gutter.add { color:var(--success); }
  .edit-gutter.remove { color:var(--error); }
  .edit-code { color:var(--text); white-space:pre; }
  .edit-gap { height:1em; }
</style>
