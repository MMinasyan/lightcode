<script>
  import { onMount } from 'svelte';
  import { EventsOn } from '../wailsjs/runtime/runtime';
  import { Submit, QueueSnapshot, ApplyTurnAction, CurrentModel, CurrentWarnings, TokenUsage, ProjectName, SessionCurrent, SessionMessages, CompactNow } from '../wailsjs/go/main/App';
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
  let permissionQueue = [];
  $: currentPermission = permissionQueue[0] || null;
  let inputArea;
  let streamingIdx = -1;
  let tokens = { total: { cache:0, input:0, output:0, known:true }, perModel: [], contextUsed: 0, contextWindow: 0 };
  let compacting = false;
  let showTokens = false;
  let warnings = [];
  let showWarnings = false;
  let pendingSubagentSessionLinks = {};

  function mid() { return nextId++; }

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

  function applySessionId(nextSessionId) {
    // Queue is backend-owned: session changes clear it server-side and emit
    // queue_changed; no local reset here.
    sessionId = nextSessionId;
  }

  function applySessionPayload(result) {
    if (!result?.sessionChanged) return;
    applySessionId(result.session?.id || sessionId);
    if (result.tokens) tokens = result.tokens;
    else tokens = { total: { cache:0, input:0, output:0, known:true }, perModel: [], contextUsed: 0, contextWindow: 0 };
    messages = rebuildFromHistory(result.messages || []);
    streamingIdx = -1;
  }

  onMount(async () => {
    EventsOn('warnings', (data) => { if (data) warnings = data; });
    try {
      const currentWarnings = await CurrentWarnings();
      if (currentWarnings) warnings = currentWarnings;
    } catch (e) { showError(e, 'Load warnings failed'); }

    try {
      const r = await CurrentModel();
      modelRef = r.ref || ((r.provider && r.model) ? `${r.provider}/${r.model}` : '');
      modelName = r.displayName || modelRef;
    } catch (e) { showError(e, 'Load model failed'); }

    try { projectName = await ProjectName(); } catch (e) { showError(e, 'Load project failed'); }

    try {
      const cur = await SessionCurrent();
      sessionId = cur?.id || '';
      if (sessionId) {
        const hist = await SessionMessages();
        messages = rebuildFromHistory(hist || []);
      }
    } catch (e) { showError(e, 'Load session failed'); }

    try {
      const t = await TokenUsage();
      if (t) tokens = t;
    } catch (e) { showError(e, 'Load token usage failed'); }

    EventsOn('usage', (data) => { if (data) tokens = data; });

    EventsOn('session_changed', (data) => {
      if (!data) return;
      applySessionId(data.session?.id || '');
      if (data.tokens) tokens = data.tokens;
      else tokens = { total: { cache:0, input:0, output:0, known:true }, perModel: [], contextUsed: 0, contextWindow: 0 };
      messages = rebuildFromHistory(data.messages || []);
      streamingIdx = -1;
    });

    EventsOn('token', (data) => {
      if (streamingIdx === -1) {
        streamingIdx = messages.length;
        messages = [...messages, { _id:mid(), type:'assistant', content:data.content, turn:currentTurn, partial:true }];
      } else {
        messages[streamingIdx] = { ...messages[streamingIdx], content: messages[streamingIdx].content + data.content };
        messages = messages;
      }
    });

    EventsOn('tool_start', (data) => {
      if (streamingIdx !== -1 && messages[streamingIdx]) {
        messages[streamingIdx] = { ...messages[streamingIdx], partial:false };
      }
      streamingIdx = -1;
      const pending = takePendingSubagentLinks(pendingSubagentSessionLinks, data.id);
      pendingSubagentSessionLinks = pending.pending;
      const subagentSessionIds = pending.links;
      messages = [...messages, { _id:mid(), type:'tool', id:data.id, name:data.name, args:data.args, done:false, success:true, result:'', subagentSessionIds }];
    });

    EventsOn('tool_result', (data) => {
      const metadata = data.metadata || null;
      const metadataLinks = subagentLinksFromMetadata(metadata);
      messages = messages.map(m => m.type==='tool' && m.id===data.id ? {
        ...m,
        done:true,
        success:data.success,
        result:data.output,
        name:data.name || m.name,
        args:data.args || m.args,
        metadata:metadata || m.metadata,
        subagentSessionIds:mergeSubagentLinks(m.subagentSessionIds, metadataLinks),
      } : m);
    });

    EventsOn('background_process_complete', (data) => {
      if (!data) return;
      if (streamingIdx !== -1 && messages[streamingIdx]) {
        messages[streamingIdx] = { ...messages[streamingIdx], partial:false };
      }
      streamingIdx = -1;
      const backgroundProcess = {
        id: data.id || '',
        command: data.command || '',
        reason: data.reason || '',
        exitCode: data.exitCode || 0,
        output: data.output || '',
      };
      messages = [...messages, {
        _id: mid(),
        type: 'background_process',
        id: backgroundProcess.id,
        done: true,
        success: data.success !== false,
        result: backgroundProcess.output,
        backgroundProcess,
      }];
    });

    EventsOn('user_message', (data) => {
      if (!data) return;
      if (streamingIdx !== -1 && messages[streamingIdx]) {
        messages[streamingIdx] = { ...messages[streamingIdx], partial:false };
      }
      streamingIdx = -1;
      messages = [...messages, { _id: mid(), type: 'user', content: data.content || '', turn: data.turn || 0 }];
    });

    EventsOn('system_signal', (data) => {
      if (!data) return;
      if (streamingIdx !== -1 && messages[streamingIdx]) {
        messages[streamingIdx] = { ...messages[streamingIdx], partial:false };
      }
      streamingIdx = -1;
      messages = [...messages, { _id: mid(), type: 'system', content: data.content || '' }];
    });

    EventsOn('queue_changed', (data) => {
      if (!data) return;
      const version = data.version || 0;
      if (version <= lastQueueVersion) return; // drop stale/reordered snapshots
      lastQueueVersion = version;
      messageQueue = (data.items || []).map(it => ({ _id: it.id, content: it.content }));
    });
    // Subscribe-then-GET: hydrate the queue only after the listener is registered.
    try {
      const q = await QueueSnapshot();
      if (q && (q.version || 0) >= lastQueueVersion) {
        lastQueueVersion = q.version || 0;
        messageQueue = (q.items || []).map(it => ({ _id: it.id, content: it.content }));
      }
    } catch (e) {}

    EventsOn('turn_start', (data) => {
      busy = true;
      streamingIdx = -1;
      if (data?.turn) currentTurn = data.turn;
    });

    EventsOn('turn_end', async (data) => {
      if (streamingIdx !== -1 && messages[streamingIdx]) {
        messages[streamingIdx] = { ...messages[streamingIdx], partial:false };
      }
      streamingIdx = -1;
      busy = false;
      permissionQueue = [];
      try {
        const cur = await SessionCurrent();
        if (cur?.id) sessionId = cur.id;
      } catch (e) {}
      // Queue draining is backend-owned: the agent auto-drains after the turn
      // ends and emits turn_start + queue_changed. No frontend flush.
    });

    EventsOn('error', (data) => {
      showError(data?.message || data);
      busy = false;
    });

    EventsOn('status', (data) => { status = data.state; });

    EventsOn('permission_request', (data) => { permissionQueue = [...permissionQueue, data]; });

    EventsOn('compaction_start', () => { compacting = true; });
    EventsOn('compaction_end', () => { compacting = false; });

    EventsOn('subagent_token', (data) => {
      appendSubagentEvent(data.sessionId, { type: 'token', content: data.content });
    });
    EventsOn('subagent_tool_start', (data) => {
      appendSubagentEvent(data.sessionId, { type: 'tool_start', id: data.id, name: data.name, args: data.args });
    });
    EventsOn('subagent_tool_result', (data) => {
      appendSubagentEvent(data.sessionId, { type: 'tool_result', id: data.id, success: data.success, output: data.output, name: data.name, args: data.args, metadata: data.metadata });
    });
    EventsOn('subagent_background_process_complete', (data) => {
      appendSubagentEvent(data.sessionId, { type: 'background_process_complete', id: data.id, command: data.command, reason: data.reason, exitCode: data.exitCode, success: data.success, output: data.output });
    });
    EventsOn('subagent_session_start', (data) => {
      const link = { index: Number(data.taskIndex), sessionId: data.sessionId };
      const rowExists = messages.some(m => m.type === 'tool' && m.id === data.taskToolCallId);
      if (!rowExists) pendingSubagentSessionLinks = rememberSubagentLink(pendingSubagentSessionLinks, data.taskToolCallId, link);
      messages = messages.map(m =>
        m.type === 'tool' && m.id === data.taskToolCallId
          ? { ...m, subagentSessionIds: mergeSubagentLinks(m.subagentSessionIds, [link]) }
          : m
      );
    });
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

  async function handleRevertCode(e) {
    const { turn } = e.detail;
    try {
      const result = await ApplyTurnAction(turn, 'revert_code', false);
      appendRevertSkipNotice(result);
    }
    catch (err) { showError(err); }
  }

  async function handleRevertHistory(e) {
    const { turn, alsoRevertCode } = e.detail;
    try {
      const result = await ApplyTurnAction(turn, 'revert_history', !!alsoRevertCode);
      applySessionPayload(result);
      appendRevertSkipNotice(result);
      inputArea?.prefill(result?.prefill || '');
    }
    catch (err) { showError(err); }
  }

  async function handleFork(e) {
    const { turn, alsoRevertCode } = e.detail;
    try {
      const result = await ApplyTurnAction(turn, 'fork', !!alsoRevertCode);
      applySessionPayload(result);
      appendRevertSkipNotice(result);
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
    on:openSessionSelector={() => showSessionSelector=true}
    on:openProjectSelector={() => showProjectSelector=true}
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
  <InputArea bind:this={inputArea} busy={busy || compacting} hasActiveModel={!!modelRef} on:submit={handleSubmit} on:error={(e) => showError(e.detail)}>
    <StatusBar {modelName} on:openModelSelector={() => showModelSelector=true} />
  </InputArea>
  {#if showModelSelector}
    <ModelSelector currentRef={modelRef} on:switched={handleModelSwitched} on:close={() => showModelSelector=false} on:manageSettings={handleManageSettings} on:error={(e) => showError(e.detail)} />
  {/if}

  {#if showSettings}
    <Settings initialSection={settingsSection} on:close={() => { showSettings = false; refreshCurrentModel(); }} on:error={(e) => showError(e.detail)} />
  {/if}

  {#if currentPermission}
    <PermissionPrompt permission={currentPermission} onDone={() => { permissionQueue = permissionQueue.slice(1); }} on:error={(e) => showError(e.detail)} />
  {/if}
  {#if showTokens}
    <TokenDetails {tokens} on:close={() => showTokens=false} />
  {/if}
  {#if showSessionSelector}
    <SessionSelector on:close={() => showSessionSelector=false} on:error={(e) => showError(e.detail)} />
  {/if}
  {#if showProjectSelector}
    <ProjectSelector on:close={() => showProjectSelector=false} on:error={(e) => showError(e.detail)} />
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
