<script>
  import { openDialog } from './dialogs';
  // Bounded generated file viewer: ReadWorkspaceFile with the explicit Workspace
  // and entered path, showing the canonical path and truncation marker. A
  // superseded reply, including one settling after close/reopen, is ignored.
  import { createEventDispatcher } from 'svelte';
  import { readWorkspaceFile } from '../generated/protocol/sdk.gen';
  import { errorText } from '../lib/errors.js';
  import { settings } from '../lib/settings.js';
  import { invokeGenerated } from './results';
  import { sameStamp } from './reader';
  export let getClient;
  export let stamp; // () => ReaderStamp
  export let workspace = '';
  const dispatch = createEventDispatcher();

  let path = '';
  let result = null;
  let error = '';
  let loading = false;
  let readToken = 0;
  let dialog;


  const invoke = (call) => invokeGenerated(getClient, 'file read', call);

  async function open() {
    const token = ++readToken;
    const cap = stamp();
    const target = path;
    loading = true;
    error = '';
    const classified = await invoke((client) => readWorkspaceFile({
      client,
      body: { workspace, path: target },
    }));
    try {
      // A superseded reply applies nothing, success and error alike.
      if (token !== readToken || !sameStamp(cap, stamp())) return;
      if (classified.kind === 'ok') {
        result = classified.data;
      } else {
        result = null;
        error = classified.message;
      }
    } finally {
      if (token === readToken) loading = false;
    }
  }

  function close() {
    readToken += 1;
    dispatch('close');
  }
</script>

<dialog class="modal" bind:this={dialog} use:openDialog aria-labelledby="viewer-title"
  on:click={(e) => { if (e.target === dialog) close(); }}
  on:close={() => dispatch('close')}>
  <div class="panel">
    <div id="viewer-title" class="hdr"><span>File viewer</span>
      <button class="panel-close" on:click={close} aria-label="Close file viewer">×</button>
    </div>
    <div class="open-row">
      <span class="ws" title={workspace}>{workspace || '(no workspace)'}</span>
      <input class="path" type="text" bind:value={path} placeholder="path relative to the workspace"
        aria-label="File path" on:keydown={(e) => { if (e.key === 'Enter') void open(); }} />
      <button class="open" disabled={!workspace || !path.trim()} on:click={() => void open()} aria-label="Open file">
        {loading ? 'Opening…' : 'Open'}
      </button>
    </div>
    {#if error}
      <div class="error">{error}</div>
    {:else if result}
      <div class="meta">
        <span class="canonical" title={result.canonical_path}>{result.canonical_path}</span>
        {#if result.truncated}<span class="truncated">truncated</span>{/if}
      </div>
      <pre class="content" class:wrap={$settings.wrapCode}>{result.content}</pre>
    {:else}
      <div class="empty">Enter a path to read it through the Runtime.</div>
    {/if}
  </div>
</dialog>

<style>
  .panel { position:absolute; inset:40px 24px 60px 24px; background:var(--bg-elevated); border:1px solid var(--border-strong); display:flex; flex-direction:column; box-shadow:var(--shadow-lg); }
  .hdr { display:flex; justify-content:space-between; align-items:center; padding:6px 12px; font-size:12px; color:var(--text-dim); text-transform:uppercase; letter-spacing:.5px; border-bottom:1px solid var(--border); }
  .open-row { display:flex; align-items:center; gap:8px; padding:8px 12px; border-bottom:1px solid var(--border); }
  .ws { flex:0 1 auto; max-width:40%; overflow:hidden; text-overflow:ellipsis; white-space:nowrap; color:var(--text-dim); font-family:var(--font-mono); font-size:12px; }
  .path { flex:1; min-width:0; padding:6px 8px; border:1px solid var(--border-button); border-radius:var(--radius); background:var(--bg-input); color:var(--text); font-family:var(--font-mono); font-size:12px; outline:none; }
  .path:focus { border-color:var(--accent); background:var(--bg-input-focus); }
  .open { background:none; border:1px solid var(--border-button); border-radius:var(--radius); color:var(--text-dim); font-family:var(--font-ui); font-size:12px; cursor:pointer; padding:5px 12px; }
  .open:hover:not(:disabled) { color:var(--accent); border-color:var(--accent); }
  .open:disabled { opacity:.4; cursor:default; }
  .meta { display:flex; align-items:center; gap:8px; padding:6px 12px; border-bottom:1px solid var(--border); }
  .canonical { flex:1; min-width:0; overflow:hidden; text-overflow:ellipsis; white-space:nowrap; color:var(--text-dim); font-family:var(--font-mono); font-size:11px; }
  .truncated { color:var(--warn); font-family:var(--font-ui); font-size:11px; text-transform:uppercase; letter-spacing:.5px; }
  .content { flex:1; overflow:auto; margin:0; padding:12px; background:var(--bg-code); color:var(--text); font-family:var(--font-mono); font-size:calc(12px * var(--scale, 1)); white-space:pre; }
  .content.wrap { white-space:pre-wrap; word-break:break-word; overflow-wrap:anywhere; }
  .error { padding:8px 12px; color:var(--error); font-family:var(--font-ui); font-size:12px; white-space:pre-wrap; }
  .empty { padding:8px 12px; color:var(--text-dim); font-family:var(--font-ui); font-size:12px; }
</style>
