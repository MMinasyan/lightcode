<script>
  import { openDialog } from './dialogs';
  // Snapshot layout as an actual generated task: current groups use the
  // generated Operation identities.
  import { createEventDispatcher, onMount } from 'svelte';
  import { invokeNative } from './results';
  import { classifyRestore, restoreLines } from './restore';
  import { sameStamp } from './reader';
  export let getClient;
  export let stamp; // () => ReaderStamp
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
  let restoreText = [];
  let busy = false;

  $: if (currentPending !== null && currentPending !== sessionID) {
    groupsToken += 1;
    currentPending = null;
  }

  const invoke = (label, operation, params, decode) => invokeNative(getClient, label, operation, params, decode);

  async function loadGroups() {
    const token = ++groupsToken;
    const cap = stamp();
    const target = sessionID;
    groupsError = '';
    if (!target) { loadedCurrent = null; currentPending = null; return; }
    currentPending = target;
    const classified = await invoke('code snapshots', 'GetSessionCodeSnapshots', JSON.stringify({ session_id: target }), JSON.parse);
    if (token !== groupsToken || !sameStamp(cap, stamp())) return;
    currentPending = null;
    if (classified.kind === 'ok') {
      loadedCurrent = { sessionID: target, groups: classified.data };
    } else {
      loadedCurrent = null;
      groupsError = classified.message;
    }
  }

  // A restore reports its addressed outcome through the App note and inline; the
  // stamped refresh stays gated, so unmounting loses no actual response.
  async function revertCurrent(group) {
    if (busy || !loadedCurrent) return;
    busy = true;
    const cap = stamp();
    const target = loadedCurrent.sessionID;
    const classified = await invoke('code rewind', 'RevertSessionCode', JSON.stringify({
      session_id: target,
      body: { boundary_operation_id: group.operation_id },
    }), JSON.parse);
    busy = false;
    const outcome = classifyRestore(classified, 'code rewind');
    restoreText = restoreLines(outcome);
    dispatch('note', `Code rewind for session ${target}: ${restoreLines(outcome).join('; ')}`);
    if (sameStamp(cap, stamp()) && loadedCurrent !== null && loadedCurrent.sessionID === target) void loadGroups();
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
  .ghost { padding:4px 10px; border:1px solid var(--border-button); border-radius:var(--radius); background:none; color:var(--text-dim); font-family:var(--font-ui); font-size:11px; cursor:pointer; flex-shrink:0; }
  .ghost:hover:not(:disabled) { color:var(--accent); border-color:var(--accent); }
  .ghost:disabled { opacity:.45; cursor:default; }
  .error { color:var(--error); font-family:var(--font-ui); font-size:12px; white-space:pre-wrap; }
  .empty { color:var(--text-dim); font-family:var(--font-ui); font-size:12px; }
  .restore { border-left:3px solid var(--accent); padding:4px 8px; background:var(--bg); font-family:var(--font-mono); font-size:11px; }
  .restore-line { white-space:pre-wrap; word-break:break-word; }
</style>
