<script>
  import { onMount } from 'svelte';
  import { EventsOn } from '../wailsjs/runtime/runtime';
  import { Submit, ApplyTurnAction, CurrentModel, ProjectName, SessionCurrent, CompactNow, HydrateSession } from '../wailsjs/go/main/App';
  import { admitSequenced, newTranscriptGate, snapshotMessages } from './lib/hydration.js';
  import { permissionList, removePermission, seedPermissions, upsertPermission } from './lib/permissions.js';
  import Toolbar from './components/Toolbar.svelte';
  import MessageList from './components/MessageList.svelte';
  import InputArea from './components/InputArea.svelte';
  import StatusBar from './components/StatusBar.svelte';
  import ModelSelector from './components/ModelSelector.svelte';
  import Settings from './components/Settings.svelte';

  import PermissionPrompt from './components/PermissionPrompt.svelte';
  import TokenDetails from './components/TokenDetails.svelte';
  import SessionSelector from './components/SessionSelector.svelte';
  import ProjectSelector from './components/ProjectSelector.svelte';
  import WarningDetails from './components/WarningDetails.svelte';
  import Viewer from './components/Viewer.svelte';
  import { viewer, appendSubagentEvent } from './lib/viewer.js';
  import { settings } from './lib/settings.js';
  import { errorText } from './lib/errors.js';
  import {
    mergeSubagentLinks,
    rememberSubagentLink,
    subagentLinksFromMetadata,
    takePendingSubagentLinks,
  } from './lib/subagentLinks.js';

  const VIEWER_THRESHOLD = 1100;
  const VIEWER_MIN_SIDE = VIEWER_THRESHOLD / 2;
  let contentEl;
  let contentWidth = 0;
  let dividerDragging = false;
  $: viewerOverlay = contentWidth > 0 && contentWidth < VIEWER_THRESHOLD;
  $: viewerWidth = viewerOverlay
    ? contentWidth
    : Math.max(VIEWER_MIN_SIDE, Math.min(contentWidth - VIEWER_MIN_SIDE, contentWidth * $settings.viewerFraction));

  function startDividerDrag(e) {
    e.preventDefault();
    dividerDragging = true;
    const rect = contentEl.getBoundingClientRect();
    const onMove = (ev) => {
      const w = Math.max(VIEWER_MIN_SIDE, Math.min(rect.width - VIEWER_MIN_SIDE, rect.right - ev.clientX));
      settings.update((s) => ({ ...s, viewerFraction: w / rect.width }));
    };
    const onUp = () => {
      dividerDragging = false;
      window.removeEventListener('mousemove', onMove);
      window.removeEventListener('mouseup', onUp);
      document.body.style.cursor = '';
      document.body.style.userSelect = '';
    };
    window.addEventListener('mousemove', onMove);
    window.addEventListener('mouseup', onUp);
    document.body.style.cursor = 'col-resize';
    document.body.style.userSelect = 'none';
  }

  let messages = [];
  let messageQueue = [];      // backend-owned: set only from queue_changed + hydration
  let lastQueueVersion = 0;   // drops stale/reordered queue snapshots
  let busy = false;
  let status = 'idle';
  let modelRef = '';
  let modelName = '';
  let sessionId = '';
  let projectName = '';
  let currentTurn = 0;
  let nextId = 0;
  let showModelSelector = false;

  let showSessionSelector = false;
  let showProjectSelector = false;
  let showSettings = false;
  let settingsSection = 'appearance';
  let permissions = new Map(); // pending permission requests keyed by request id
  $: currentPermission = permissionList(permissions)[0] || null;
  let inputArea;
  let streamingIdx = -1;
  let tokens = { total: { cache:0, input:0, output:0, known:true }, perModel: [], contextUsed: 0, contextWindow: 0 };
  let compacting = false;
  let showTokens = false;
  let warnings = [];
  let showWarnings = false;
  let pendingSubagentSessionLinks = {};

  // Complete-state hydration: listeners register before the snapshot is read, so
  // transcript frames arriving during the read are buffered and replayed after the
  // snapshot is applied. The gate then drops any replayed or live transcript item
  // already represented in the snapshot (sequence at or below its high-water).
  let hydrated = false;
  let pendingFrames = [];
  let gate = { highWater: 0 };

  function mid() { return nextId++; }

  function defaultTokens() {
    return { total: { cache:0, input:0, output:0, known:true }, perModel: [], contextUsed: 0, contextWindow: 0 };
  }

  // buffered wraps a delivered-frame handler so it holds until the snapshot is
  // applied, then replays in delivery order. Applying the snapshot first and every
  // buffered frame after it keeps a frame delivered during the read from being
  // overwritten by the older captured state.
  function buffered(handler) {
    return (data) => {
      if (!hydrated) { pendingFrames.push(() => handler(data)); return; }
      handler(data);
    };
  }

  function showError(err, prefix = '') {
    const text = errorText(err);
    messages = [...messages, { _id: mid(), type: 'error', content: prefix ? `${prefix}: ${text}` : text }];
  }

  function appendRevertSkipNotice(result) {
    const skipped = result?.skippedFiles || [];
    if (!skipped.length) return;
    const lines = skipped.map((f) => `- ${f.path}${f.reason ? ` (${f.reason})` : ''}`);
    messages = [...messages, {
      _id: mid(),
      type: 'system',
      content: `System: Kept ${skipped.length} file${skipped.length === 1 ? '' : 's'} changed outside this session:\n${lines.join('\n')}`,
    }];
  }

  function rebuildFromHistory(persisted) {
    currentTurn = 0;
    pendingSubagentSessionLinks = {};
    return (persisted || []).map(m => {
      if ((m.turn || 0) > currentTurn) currentTurn = m.turn;
      return { ...m, _id: mid() };
    });
  }

  // applyResync refreshes a compacted session's transcript and tokens without
  // disturbing activity, queue, warnings, or permissions, which compaction does
  // not change — and which a turn-end refresh could otherwise re-stick to a stale
  // busy. The payload carries no sequence cursor, so the gate resets to admit the
  // refreshed session's live stream.
  function applyResync(data) {
    if (!data) return;
    // A resync only refreshes the current session; it never switches. Reject one
    // for a session we have navigated away from — a compaction resync for A can be
    // enqueued after a switch to B committed its navigation, and applying it would
    // restore A over B.
    if ((data.session?.id || '') !== sessionId) return;
    tokens = data.tokens || defaultTokens();
    messages = rebuildFromHistory(data.messages || []);
    streamingIdx = -1;
    gate = { highWater: 0 };
  }

  // applySnapshot renders one complete-state hydration as the whole live view and
  // seeds the transcript gate at its high-water. The queue guard resets to the
  // snapshot's session and version; later same-session versions must increase.
  function applySnapshot(hs) {
    if (!hs) return;
    sessionId = hs.session?.id || '';
    messages = rebuildFromHistory(snapshotMessages(hs));
    gate = newTranscriptGate(hs);
    streamingIdx = -1;
    tokens = hs.tokens || defaultTokens();
    busy = !!hs.busy;
    compacting = !!hs.compacting;
    lastQueueVersion = hs.queue?.version || 0;
    messageQueue = (hs.queue?.items || []).map((it) => ({ _id: it.id, content: it.content }));
    warnings = hs.warnings || [];
    permissions = seedPermissions(hs.permissions);
  }

  async function hydrate() {
    let id = sessionId;
    if (!id) {
      try { const cur = await SessionCurrent(); id = cur?.id || ''; } catch (e) {}
    }
    if (id) {
      try { applySnapshot(await HydrateSession(id)); }
      catch (e) { showError(e, 'Load session failed'); }
    }
    const buffered = pendingFrames;
    pendingFrames = [];
    hydrated = true;
    for (const replay of buffered) replay();
  }

  function closeStreaming() {
    if (streamingIdx !== -1 && messages[streamingIdx]) {
      messages[streamingIdx] = { ...messages[streamingIdx], partial: false };
    }
    streamingIdx = -1;
  }

  function applyToken(data) {
    if (!admitSequenced(gate, data.seq)) return;
    if (streamingIdx === -1) {
      streamingIdx = messages.length;
      messages = [...messages, { _id: mid(), type: 'assistant', content: data.content, turn: currentTurn, partial: true }];
    } else {
      messages[streamingIdx] = { ...messages[streamingIdx], content: messages[streamingIdx].content + data.content };
      messages = messages;
    }
  }

  function applyToolStart(data) {
    if (!admitSequenced(gate, data.seq)) return;
    closeStreaming();
    const pending = takePendingSubagentLinks(pendingSubagentSessionLinks, data.id);
    pendingSubagentSessionLinks = pending.pending;
    messages = [...messages, { _id: mid(), type: 'tool', id: data.id, name: data.name, args: data.args, done: false, success: true, result: '', subagentSessionIds: pending.links }];
  }

  function applyToolResult(data) {
    // Delivered id-keyed and applied idempotently by tool-call id; not sequence-gated.
    const metadata = data.metadata || null;
    const metadataLinks = subagentLinksFromMetadata(metadata);
    messages = messages.map(m => m.type === 'tool' && m.id === data.id ? {
      ...m,
      done: true,
      success: data.success,
      result: data.output,
      name: data.name || m.name,
      args: data.args || m.args,
      metadata: metadata || m.metadata,
      subagentSessionIds: mergeSubagentLinks(m.subagentSessionIds, metadataLinks),
    } : m);
  }

  function applyBackgroundProcess(data) {
    if (!data || !admitSequenced(gate, data.seq)) return;
    closeStreaming();
    const backgroundProcess = { id: data.id || '', command: data.command || '', reason: data.reason || '', exitCode: data.exitCode || 0, output: data.output || '' };
    messages = [...messages, { _id: mid(), type: 'background_process', id: backgroundProcess.id, done: true, success: data.success !== false, result: backgroundProcess.output, backgroundProcess }];
  }

  function applyUserMessage(data) {
    if (!data || !admitSequenced(gate, data.seq)) return;
    closeStreaming();
    messages = [...messages, { _id: mid(), type: 'user', content: data.content || '', turn: data.turn || 0 }];
  }

  function applySystemSignal(data) {
    if (!data || !admitSequenced(gate, data.seq)) return;
    closeStreaming();
    messages = [...messages, { _id: mid(), type: 'system', content: data.content || '' }];
  }

  onMount(async () => {
    // Register every listener before the first hydration read, each buffered so the
    // snapshot applies first and every frame delivered during the read replays after
    // it in delivery order.
    EventsOn('warnings', buffered((data) => { if (data) warnings = data; }));
    EventsOn('usage', buffered((data) => { if (data) tokens = data; }));

    // A resync boundary carries a compacted session's transcript and tokens;
    // applying it refreshes the view without touching activity or queue that
    // compaction does not change.
    EventsOn('resync', buffered((data) => { applyResync(data); }));

    // A navigation boundary carries the destination session's complete state;
    // applying it replaces the whole live view (messages, gate, tokens, activity,
    // queue, warnings, permissions). An empty state is a detach.
    EventsOn('navigation', buffered((data) => { applySnapshot(data); }));

    // A turn-action boundary carries the fork/revert destination's complete state
    // and any code-revert skip notice as one ordered frame; applying the snapshot
    // and appending the notice together keeps the notice from being clobbered.
    EventsOn('turn_action', buffered((data) => {
      applySnapshot(data?.state);
      appendRevertSkipNotice({ skippedFiles: data?.skippedFiles });
    }));

    EventsOn('token', buffered(applyToken));
    EventsOn('tool_start', buffered(applyToolStart));
    EventsOn('tool_result', buffered(applyToolResult));
    EventsOn('background_process_complete', buffered(applyBackgroundProcess));
    EventsOn('user_message', buffered(applyUserMessage));
    EventsOn('system_signal', buffered(applySystemSignal));

    EventsOn('queue_changed', buffered((data) => {
      if (!data) return;
      const version = data.version || 0;
      if (version <= lastQueueVersion) return; // drop stale/reordered snapshots
      lastQueueVersion = version;
      messageQueue = (data.items || []).map(it => ({ _id: it.id, content: it.content }));
    }));

    EventsOn('turn_start', buffered((data) => {
      busy = true;
      streamingIdx = -1;
      if (data?.turn) currentTurn = data.turn;
    }));

    EventsOn('turn_end', buffered((data) => {
      closeStreaming();
      busy = false;
      // A pending request blocks its turn, so a turn end (including a cancel) leaves
      // no request that still needs an answer.
      permissions = {};
      // Session identity is owned by the ordered navigation/turn_action/hydration
      // boundaries; a turn end never re-resolves it. An out-of-band current-session
      // lookup here could restore a session a concurrent switch already left.
      // Queue draining is backend-owned: the agent auto-drains after the turn
      // ends and emits turn_start + queue_changed. No frontend flush.
    }));

    EventsOn('error', buffered((data) => {
      showError(data?.message || data);
      busy = false;
    }));

    EventsOn('status', buffered((data) => { status = data.state; }));

    EventsOn('permission_request', buffered((data) => { permissions = upsertPermission(permissions, data); }));

    EventsOn('compaction_start', buffered(() => { compacting = true; }));
    EventsOn('compaction_end', buffered(() => { compacting = false; }));

    EventsOn('subagent_token', buffered((data) => {
      appendSubagentEvent(data.sessionId, { type: 'token', content: data.content });
    }));
    EventsOn('subagent_tool_start', buffered((data) => {
      appendSubagentEvent(data.sessionId, { type: 'tool_start', id: data.id, name: data.name, args: data.args });
    }));
    EventsOn('subagent_tool_result', buffered((data) => {
      appendSubagentEvent(data.sessionId, { type: 'tool_result', id: data.id, success: data.success, output: data.output, name: data.name, args: data.args, metadata: data.metadata });
    }));
    EventsOn('subagent_background_process_complete', buffered((data) => {
      appendSubagentEvent(data.sessionId, { type: 'background_process_complete', id: data.id, command: data.command, reason: data.reason, exitCode: data.exitCode, success: data.success, output: data.output });
    }));
    EventsOn('subagent_session_start', buffered((data) => {
      const link = { index: Number(data.taskIndex), sessionId: data.sessionId };
      const rowExists = messages.some(m => m.type === 'tool' && m.id === data.taskToolCallId);
      if (!rowExists) pendingSubagentSessionLinks = rememberSubagentLink(pendingSubagentSessionLinks, data.taskToolCallId, link);
      messages = messages.map(m =>
        m.type === 'tool' && m.id === data.taskToolCallId
          ? { ...m, subagentSessionIds: mergeSubagentLinks(m.subagentSessionIds, [link]) }
          : m
      );
    }));

    // Non-transcript scalars not carried by the snapshot: fetched directly.
    try {
      const r = await CurrentModel();
      modelRef = r.ref || ((r.provider && r.model) ? `${r.provider}/${r.model}` : '');
      modelName = r.displayName || modelRef;
    } catch (e) { showError(e, 'Load model failed'); }
    try { projectName = await ProjectName(); } catch (e) { showError(e, 'Load project failed'); }

    await hydrate();
  });

  async function handleCompact() {
    compacting = true;
    try { await CompactNow(); }
    catch (err) { showError(err, 'Compaction failed'); }
    finally { compacting = false; }
    // Compaction does not change the queue (backend-owned); nothing to flush.
  }

  async function handleSubmit(e) {
    const content = e.detail;
    if (!modelRef) {
      showError("Connect a provider or pick a model before sending.");
      inputArea?.prefill(content);
      return;
    }
    try {
      const r = await Submit(content);
      if (r?.started) {
        busy = true;
        streamingIdx = -1;
        currentTurn = r.turn || currentTurn;
      }
      // If queued (!started), the queue_changed event drives the dimmed
      // indicator; do not touch busy here.
    }
    catch (err) {
      // Rejected (e.g. a session change is in flight): keep the draft so the
      // user can resubmit. InputArea cleared its text on dispatch.
      showError(err);
      inputArea?.prefill(content);
    }
  }

  function handleManageSettings() { showModelSelector = false; settingsSection = 'models'; showSettings = true; }
  function handleModelSwitched(e) { modelRef = e.detail.ref; modelName = e.detail.displayName || e.detail.ref; showModelSelector = false; }
  async function refreshCurrentModel() {
    try {
      const r = await CurrentModel();
      modelRef = r.ref || ((r.provider && r.model) ? `${r.provider}/${r.model}` : '');
      modelName = r.displayName || modelRef;
    } catch (e) { showError(e, 'Load model failed'); }
  }

  async function handleProjectSwitched() {
    try { projectName = await ProjectName(); } catch (e) { showError(e, 'Load project failed'); }
    await refreshCurrentModel();
  }

  async function handleRevertCode(e) {
    const { turn } = e.detail;
    try {
      // The backend appends the skip notice as an ordered turn_action frame; no
      // out-of-band append here that a queued refresh could clobber.
      await ApplyTurnAction(turn, 'revert_code', false);
    }
    catch (err) { showError(err); }
  }

  async function handleRevertHistory(e) {
    const { turn, alsoRevertCode } = e.detail;
    try {
      const result = await ApplyTurnAction(turn, 'revert_history', !!alsoRevertCode);
      // The backend appends the reverted session's complete state and any skip
      // notice as one ordered turn_action boundary; only the input prefill is
      // applied from the direct result here.
      inputArea?.prefill(result?.prefill || '');
    }
    catch (err) { showError(err); }
  }

  async function handleFork(e) {
    const { turn, alsoRevertCode } = e.detail;
    try {
      // The backend appends the forked session's complete state and any skip
      // notice as one ordered turn_action boundary; nothing is applied here.
      await ApplyTurnAction(turn, 'fork', !!alsoRevertCode);
    }
    catch (err) { showError(err); }
  }
  function handleKeydown(e) {
    if ((e.ctrlKey||e.metaKey) && e.key==='m') { e.preventDefault(); showModelSelector = !showModelSelector; }
    if ((e.ctrlKey||e.metaKey) && e.key==='s') { e.preventDefault(); showSessionSelector = !showSessionSelector; }
    if (e.key==='/' && document.activeElement?.tagName !== 'TEXTAREA') { e.preventDefault(); inputArea?.focus(); }
  }
</script>

<svelte:window on:keydown={handleKeydown} />

<main class="app" style="--scale:{$settings.fontScale / 100}">
  <Toolbar {sessionId} {projectName} {tokens} {compacting} {busy} {warnings}
    on:openModelSelector={() => showModelSelector=true}
    on:openTokens={() => showTokens=true}
    on:compact={handleCompact}
    on:openSessionSelector={() => { if (hydrated) showSessionSelector = true; }}
    on:openProjectSelector={() => { if (hydrated) showProjectSelector = true; }}
    on:openSettings={() => { settingsSection = 'providers'; showSettings = true; }}
    on:openWarnings={() => showWarnings=true} />
  <div class="content" bind:this={contentEl} bind:clientWidth={contentWidth}>
    <MessageList {messages} {busy} {compacting} {messageQueue}
      on:revertcode={handleRevertCode}
      on:reverthistory={handleRevertHistory}
      on:fork={handleFork} />
    {#if $viewer}
      {#if viewerOverlay}
        <div class="viewer-pane overlay"><Viewer /></div>
      {:else}
        <!-- svelte-ignore a11y-no-static-element-interactions -->
        <div class="divider" class:dragging={dividerDragging} on:mousedown={startDividerDrag} title="Drag to resize"></div>
        <div class="viewer-pane" style="flex:0 0 {viewerWidth}px"><Viewer /></div>
      {/if}
    {/if}
  </div>
  <InputArea bind:this={inputArea} busy={busy || compacting} hasActiveModel={!!modelRef && hydrated} on:submit={handleSubmit} on:error={(e) => showError(e.detail)}>
    <StatusBar {modelName} on:openModelSelector={() => showModelSelector=true} />
  </InputArea>
  {#if showModelSelector}
    <ModelSelector currentRef={modelRef} on:switched={handleModelSwitched} on:close={() => showModelSelector=false} on:manageSettings={handleManageSettings} on:error={(e) => showError(e.detail)} />
  {/if}

  {#if showSettings}
    <Settings initialSection={settingsSection} on:close={() => { showSettings = false; refreshCurrentModel(); }} on:error={(e) => showError(e.detail)} />
  {/if}

  {#if currentPermission}
    <PermissionPrompt permission={currentPermission} onDone={(id) => { permissions = removePermission(permissions, id); }} on:error={(e) => showError(e.detail)} />
  {/if}
  {#if showTokens}
    <TokenDetails {tokens} on:close={() => showTokens=false} />
  {/if}
  {#if showSessionSelector}
    <SessionSelector on:close={() => showSessionSelector=false} on:error={(e) => showError(e.detail)} />
  {/if}
  {#if showProjectSelector}
    <ProjectSelector on:switched={handleProjectSwitched} on:close={() => showProjectSelector=false} on:error={(e) => showError(e.detail)} />
  {/if}
  {#if showWarnings}
    <WarningDetails {warnings} on:close={() => showWarnings=false} />
  {/if}
</main>

<style>
  .app { height:100vh; display:flex; flex-direction:column; overflow:hidden; }
  .content { flex:1; display:flex; flex-direction:row; overflow:hidden; position:relative; min-height:0; }
  .viewer-pane { display:flex; min-width:0; }
  .viewer-pane.overlay { position:absolute; inset:0; background:var(--bg); z-index:10; }
  .divider { width:4px; flex-shrink:0; background:var(--border); cursor:col-resize; }
  .divider:hover:not(.dragging) { background:var(--accent); }
</style>
