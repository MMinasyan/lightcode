<script>
  import { openDialog } from './dialogs';
  // Both snapshot layouts as actual generated tasks: current groups use the
  // generated Operation identities; retained snapshots use the selected
  // Workspace, an 8-hex retained ID and each turn's exact after_turn token.
  import { createEventDispatcher, onMount } from 'svelte';
  import {
    getRetainedCodeSnapshots,
    getSessionCodeSnapshots,
    revertRetainedCode,
    revertSessionCode,
  } from '../generated/protocol/sdk.gen';
  import { errorText } from '../lib/errors.js';
  import { invokeGenerated } from './results';
  import { classifyRestore, restoreLines } from './restore';
  import { sameStamp } from './reader';
  import { composeObjectPatch, parseExactJSON, readRetainedTurnsText } from './exactjson';
  export let getClient;
  export let stamp; // () => ReaderStamp
  export let workspace = '';
  export let sessionID = '';
  const dispatch = createEventDispatcher();

  let dialog;

  onMount(() => {
    void loadGroups();
  });
  // Each result carries the identity it was read with; revert actions use that
  // loaded identity and boundary, never the live props.
  let loadedCurrent = null; // { sessionID, groups }
  let currentPending = null; // sessionID
  let groupsError = '';
  let groupsToken = 0;
  let retainedID = '';
  let loadedRetained = null; // { workspace, sessionID, turns }
  let retainedPending = null; // { workspace, sessionID }
  let retainedError = '';
  let retainedToken = 0;
  let restoreText = [];
  let busy = false;

  $: if (currentPending !== null && currentPending !== sessionID) {
    groupsToken += 1;
    currentPending = null;
  }
  $: if (retainedPending !== null && (retainedPending.workspace !== workspace || retainedPending.sessionID !== retainedID)) {
    retainedToken += 1;
    retainedPending = null;
  }


  const invoke = (call, label) => invokeGenerated(getClient, label, call);

  async function loadGroups() {
    const token = ++groupsToken;
    const cap = stamp();
    const target = sessionID;
    groupsError = '';
    if (!target) { loadedCurrent = null; currentPending = null; return; }
    currentPending = target;
    const classified = await invoke((client) => getSessionCodeSnapshots({ client, path: { id: target } }), 'code snapshots');
    if (token !== groupsToken || !sameStamp(cap, stamp())) return;
    currentPending = null;
    if (classified.kind === 'ok') {
      loadedCurrent = { sessionID: target, groups: classified.data };
    } else {
      loadedCurrent = null;
      groupsError = classified.message;
    }
  }

  async function loadRetained() {
    const token = ++retainedToken;
    const cap = stamp();
    const targetWorkspace = workspace;
    const target = retainedID;
    retainedError = '';
    if (!target) { loadedRetained = null; retainedPending = null; return; }
    retainedPending = { workspace: targetWorkspace, sessionID: target };
    const classified = await invoke((client) => getRetainedCodeSnapshots({
      client,
      query: { workspace: targetWorkspace, session_id: target },
      parseAs: 'text',
    }), 'retained snapshots');
    if (token !== retainedToken || !sameStamp(cap, stamp())) return;
    retainedPending = null;
    if (classified.kind !== 'ok') {
      loadedRetained = null;
      retainedError = classified.message;
      return;
    }
    try {
      loadedRetained = { workspace: targetWorkspace, sessionID: target, turns: readRetainedTurnsText(parseExactJSON(classified.data)) };
    } catch (e) {
      loadedRetained = null;
      retainedError = errorText(e);
    }
  }

  // A restore reports its addressed outcome through the App note and inline; the
  // stamped refresh stays gated, so unmounting loses no actual response.
  async function revertCurrent(group) {
    if (busy || !loadedCurrent) return;
    busy = true;
    const cap = stamp();
    const target = loadedCurrent.sessionID;
    const classified = await invoke((client) => revertSessionCode({
      client,
      path: { id: target },
      body: { boundary_operation_id: group.operation_id },
    }), 'code rewind');
    busy = false;
    const outcome = classifyRestore(classified, 'code rewind');
    restoreText = restoreLines(outcome);
    dispatch('note', `Code rewind for session ${target}: ${restoreLines(outcome).join('; ')}`);
    if (sameStamp(cap, stamp()) && loadedCurrent !== null && loadedCurrent.sessionID === target) void loadGroups();
  }

  async function revertRetained(turnText) {
    if (busy || !loadedRetained) return;
    busy = true;
    const targetWorkspace = loadedRetained.workspace;
    const target = loadedRetained.sessionID;
    const bodyText = composeObjectPatch([
      ['workspace', JSON.stringify(targetWorkspace)],
      ['session_id', JSON.stringify(target)],
      ['after_turn', turnText],
    ]);
    const classified = await invoke((client) => revertRetainedCode({
      client,
      body: { workspace: targetWorkspace, session_id: target, after_turn: 0 },
      bodySerializer: () => bodyText,
    }), 'retained rewind');
    busy = false;
    const outcome = classifyRestore(classified, 'retained rewind');
    restoreText = restoreLines(outcome);
    dispatch('note', `Retained code rewind for ${targetWorkspace} · ${target}: ${restoreLines(outcome).join('; ')}`);
  }
</script>

<dialog class="modal" bind:this={dialog} use:openDialog aria-labelledby="snapshots-title"
  on:click={(e) => { if (e.target === dialog) dialog.close(); }}
  on:close={() => dispatch('close')}>
  <div class="panel">
    <div id="snapshots-title" class="hdr"><span>Code snapshots</span>
      <button class="panel-close" on:click={() => dialog.close()} aria-label="Close snapshots">×</button>
    </div>
    <div class="body">
      <div class="sec">Current session snapshots{loadedCurrent ? ` (loaded ${loadedCurrent.sessionID})` : ''}</div>
      {#if groupsError}
        <div class="error">{groupsError}</div>
      {:else if !loadedCurrent || loadedCurrent.groups.length === 0}
        <div class="empty">(no snapshot groups)</div>
      {:else}
        {#each loadedCurrent.groups as group (group.group_item_id)}
          <div class="group">
            <div class="group-hdr">
              <span class="ids">Operation {group.operation_id} · group {group.group_item_id}</span>
              <button class="ghost" disabled={busy} on:click={() => void revertCurrent(group)}
                aria-label={`Revert code to before operation ${group.operation_id}`}>Revert to before</button>
            </div>
            {#each group.files as file (file.path)}
              <div class="file"><span class="path">{file.path}</span><span class="flag">{file.existed ? 'existed' : 'created'}</span></div>
            {/each}
          </div>
        {/each}
      {/if}

      <div class="sec">Retained pre-cutover snapshots</div>
      <div class="retained-row">
        <span class="ws" title={workspace}>{workspace || '(no workspace)'}</span>
        <input class="id" type="text" bind:value={retainedID} placeholder="8-hex retained session id"
          aria-label="Retained session id" on:keydown={(e) => { if (e.key === 'Enter') void loadRetained(); }} />
        <button class="ghost" disabled={!workspace || !retainedID || busy} on:click={() => void loadRetained()} aria-label="Read retained snapshots">Read</button>
      </div>
      {#if retainedError}
        <div class="error">{retainedError}</div>
      {:else if loadedRetained && loadedRetained.turns.length > 0}
        <div class="sec">Loaded retained target: {loadedRetained.workspace} · {loadedRetained.sessionID}</div>
        <div class="group">
          <div class="group-hdr">
            <span class="ids">Retained snapshot boundary after turn 0 (initial)</span>
            <button class="ghost" disabled={busy} on:click={() => void revertRetained('0')}
              aria-label="Revert to initial retained boundary">Revert after</button>
          </div>
        </div>
        {#each loadedRetained.turns as turn (turn.turnText)}
          <div class="group">
            <div class="group-hdr">
              <span class="ids">Retained snapshot boundary after turn {turn.turnText}</span>
              <button class="ghost" disabled={busy} on:click={() => void revertRetained(turn.turnText)}
                aria-label={`Revert after retained turn ${turn.turnText}`}>Revert after</button>
            </div>
            {#each turn.files as file (file.path)}
              <div class="file"><span class="path">{file.path}</span><span class="flag">{file.existed ? 'existed' : 'created'}</span></div>
            {/each}
          </div>
        {/each}
      {/if}

      {#if restoreText.length > 0}
        <div class="sec">Restore outcome</div>
        <div class="restore">
          {#each restoreText as line}
            <div class="restore-line">{line}</div>
          {/each}
        </div>
      {/if}
    </div>
  </div>
</dialog>

<style>
  .panel { position:absolute; top:40px; left:50%; transform:translateX(-50%); width:600px; max-width:94vw; max-height:86vh; background:var(--bg-elevated); border:1px solid var(--border-strong); box-shadow:var(--shadow-lg); display:flex; flex-direction:column; }
  .hdr { display:flex; justify-content:space-between; align-items:center; padding:6px 12px; font-size:12px; color:var(--text-dim); text-transform:uppercase; letter-spacing:.5px; border-bottom:1px solid var(--border); }
  .body { padding:10px 12px; overflow-y:auto; display:flex; flex-direction:column; gap:8px; }
  .sec { color:var(--text-dim); font-family:var(--font-ui); font-size:11px; text-transform:uppercase; letter-spacing:.5px; border-top:1px solid var(--border); padding-top:8px; }
  .group { border:1px solid var(--border); border-radius:var(--radius); background:var(--bg); padding:6px 8px; }
  .group-hdr { display:flex; align-items:center; justify-content:space-between; gap:8px; }
  .ids { color:var(--text); font-family:var(--font-mono); font-size:11px; overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
  .file { display:flex; justify-content:space-between; gap:8px; padding:2px 0; font-family:var(--font-mono); font-size:11px; }
  .path { overflow:hidden; text-overflow:ellipsis; white-space:nowrap; color:var(--text); }
  .flag { color:var(--text-dim); flex-shrink:0; }
  .retained-row { display:flex; align-items:center; gap:6px; }
  .ws { flex:0 1 auto; max-width:38%; overflow:hidden; text-overflow:ellipsis; white-space:nowrap; color:var(--text-dim); font-family:var(--font-mono); font-size:11px; }
  .id { flex:1; min-width:0; padding:5px 8px; border:1px solid var(--border-button); border-radius:var(--radius); background:var(--bg-input); color:var(--text); font-family:var(--font-mono); font-size:12px; outline:none; }
  .id:focus { border-color:var(--accent); }
  .ghost { padding:4px 10px; border:1px solid var(--border-button); border-radius:var(--radius); background:none; color:var(--text-dim); font-family:var(--font-ui); font-size:11px; cursor:pointer; flex-shrink:0; }
  .ghost:hover:not(:disabled) { color:var(--accent); border-color:var(--accent); }
  .ghost:disabled { opacity:.45; cursor:default; }
  .error { color:var(--error); font-family:var(--font-ui); font-size:12px; white-space:pre-wrap; }
  .empty { color:var(--text-dim); font-family:var(--font-ui); font-size:12px; }
  .restore { border-left:3px solid var(--accent); padding:4px 8px; background:var(--bg); font-family:var(--font-mono); font-size:11px; }
  .restore-line { white-space:pre-wrap; word-break:break-word; }
</style>
