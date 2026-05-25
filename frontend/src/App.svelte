<script>
  import { onMount } from 'svelte';
  import { EventsOn } from '../wailsjs/runtime/runtime';
  import { SendPrompt, SendQueuedMessages, ApplyTurnAction, CurrentModel, CurrentWarnings, TokenUsage, ProjectName, SessionCurrent, SessionMessages, CompactNow } from '../wailsjs/go/main/App';
  import Toolbar from './components/Toolbar.svelte';
  import MessageList from './components/MessageList.svelte';
  import InputArea from './components/InputArea.svelte';
  import StatusBar from './components/StatusBar.svelte';
  import ModelSelector from './components/ModelSelector.svelte';

  import PermissionPrompt from './components/PermissionPrompt.svelte';
  import TokenDetails from './components/TokenDetails.svelte';
  import SessionSelector from './components/SessionSelector.svelte';
  import ProjectSelector from './components/ProjectSelector.svelte';
  import Settings from './components/Settings.svelte';
  import WarningDetails from './components/WarningDetails.svelte';
  import Viewer from './components/Viewer.svelte';
  import { viewer, appendSubagentEvent } from './lib/viewer.js';
  import { settings } from './lib/settings.js';
  import { errorText } from './lib/errors.js';

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
  let messageQueue = [];
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

  function mid() { return nextId++; }

  function showError(err, prefix = '') {
    const text = errorText(err);
    messages = [...messages, { _id: mid(), type: 'error', content: prefix ? `${prefix}: ${text}` : text }];
  }

  function isTurnBusyError(err) {
    return errorText(err).includes('turn is already in progress');
  }

  function rebuildFromHistory(persisted) {
    currentTurn = 0;
    return (persisted || []).map(m => {
      if ((m.turn || 0) > currentTurn) currentTurn = m.turn;
      return { ...m, _id: mid() };
    });
  }

  function applySessionId(nextSessionId) {
    if (nextSessionId !== sessionId) messageQueue = [];
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
      messages = [...messages, { _id:mid(), type:'tool', id:data.id, name:data.name, args:data.args, done:false, success:true, result:'' }];
    });

    EventsOn('tool_result', (data) => {
      messages = messages.map(m => m.type==='tool' && m.id===data.id ? {...m, done:true, success:data.success, result:data.output, name:data.name || m.name, args:data.args || m.args, metadata:data.metadata || m.metadata} : m);
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

    EventsOn('turn_start', (data) => {
      busy = true;
      streamingIdx = -1;
      if (data?.turn) currentTurn = data.turn;
    });

    EventsOn('turn_end', async (data) => {
      if (streamingIdx !== -1 && messages[streamingIdx]) {
        messages[streamingIdx] = { ...messages[streamingIdx], partial:false };
      }
      if (data?.cancelled) {
        messages = [...messages, { _id:mid(), type:'system', content:'interrupted' }];
      } else {
        messages = messages;
      }
      streamingIdx = -1;
      busy = false;
      permissionQueue = [];
      try {
        const cur = await SessionCurrent();
        if (cur?.id) sessionId = cur.id;
      } catch (e) {}
      await flushQueue();
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
    EventsOn('subagent_session_start', (data) => {
      messages = messages.map(m =>
        m.type === 'tool' && m.id === data.taskToolCallId
          ? { ...m, subagentSessionIds: [...(m.subagentSessionIds || []), { index: data.taskIndex, sessionId: data.sessionId }] }
          : m
      );
    });
  });

  async function handleCompact() {
    busy = true;
    try { await CompactNow(); }
    catch (err) { showError(err, 'Compaction failed'); messageQueue = []; }
    finally { busy = false; }
    await flushQueue();
  }

  async function handleSubmit(e) {
    const content = e.detail;
    if (busy || messageQueue.length > 0) {
      messageQueue = [...messageQueue, { _id: mid(), content }];
      if (!busy) await flushQueue();
      return;
    }
    busy = true;
    streamingIdx = -1;
    try {
      const turn = await SendPrompt(content);
      currentTurn = turn;
      messages = [...messages, { _id:mid(), type:'user', content, turn }];
    }
    catch (err) { showError(err); busy = false; }
  }

  async function flushQueue() {
    if (messageQueue.length === 0) return;
    if (busy) return;
    const queued = messageQueue;
    busy = true;
    streamingIdx = -1;
    try {
      const result = await SendQueuedMessages(queued.map(q => q.content));
      messageQueue = messageQueue.slice(queued.length);
      const appended = result?.appended || [];
      for (let i = 0; i < appended.length; i++) {
        const queuedMessage = queued[i];
        messages = [...messages, { _id: queuedMessage._id, type: 'user', content: queuedMessage.content, turn: appended[i].turn }];
      }
      const last = queued[queued.length - 1];
      const turn = result?.started?.turn || 0;
      currentTurn = turn;
      messages = [...messages, { _id: last._id, type: 'user', content: last.content, turn }];
    }
    catch (err) {
      if (isTurnBusyError(err)) {
        busy = true;
        return;
      }
      showError(err);
      busy = false;
    }
  }

  function handleManageSettings() { showModelSelector = false; settingsSection = 'models'; showSettings = true; }
  function handleModelSwitched(e) { modelRef = e.detail.ref; modelName = e.detail.displayName || e.detail.ref; showModelSelector = false; }

  async function handleRevertCode(e) {
    const { turn } = e.detail;
    try { await ApplyTurnAction(turn, 'revert_code', false); }
    catch (err) { showError(err); }
  }

  async function handleRevertHistory(e) {
    const { turn, alsoRevertCode } = e.detail;
    try {
      const result = await ApplyTurnAction(turn, 'revert_history', !!alsoRevertCode);
      applySessionPayload(result);
      inputArea?.prefill(result?.prefill || '');
    }
    catch (err) { showError(err); }
  }

  async function handleFork(e) {
    const { turn, alsoRevertCode } = e.detail;
    try {
      const result = await ApplyTurnAction(turn, 'fork', !!alsoRevertCode);
      applySessionPayload(result);
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
    on:openSettings={() => { settingsSection = 'appearance'; showSettings = true; }}
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
  <InputArea bind:this={inputArea} {busy} on:submit={handleSubmit} on:error={(e) => showError(e.detail)}>
    <StatusBar {modelName} on:openModelSelector={() => showModelSelector=true} />
  </InputArea>
  {#if showModelSelector}
    <ModelSelector currentRef={modelRef} on:switched={handleModelSwitched} on:close={() => showModelSelector=false} on:manageSettings={handleManageSettings} on:error={(e) => showError(e.detail)} />
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
  {#if showSettings}
    <Settings initialSection={settingsSection} on:close={() => showSettings=false} on:error={(e) => showError(e.detail)} />
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
