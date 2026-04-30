<script>
  import { onMount } from 'svelte';
  import { EventsOn } from '../wailsjs/runtime/runtime';
  import { SendPrompt, AppendUserMessage, CurrentModel, RespondPermission, TokenUsage, ProjectName, SessionCurrent, SessionMessages, RevertCode, RevertHistory, ForkSession, CompactNow } from '../wailsjs/go/main/App';
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
  let provider = '';
  let model = '';
  let sessionId = '';
  let projectName = '';
  let currentTurn = 0;
  let nextId = 0;
  let showModelSelector = false;

  let showSessionSelector = false;
  let showProjectSelector = false;
  let showSettings = false;
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

  function rebuildFromHistory(persisted) {
    currentTurn = 0;
    return (persisted || []).map(m => {
      if ((m.turn || 0) > currentTurn) currentTurn = m.turn;
      return { ...m, _id: mid() };
    });
  }

  onMount(async () => {
    try {
      const r = await CurrentModel();
      provider = r.provider || '';
      model = r.model || '';
    } catch (e) { console.error(e); }

    try { projectName = await ProjectName(); } catch (e) { console.error(e); }

    try {
      const cur = await SessionCurrent();
      sessionId = cur?.id || '';
      if (sessionId) {
        const hist = await SessionMessages();
        messages = rebuildFromHistory(hist || []);
      }
    } catch (e) { console.error(e); }

    try {
      const t = await TokenUsage();
      if (t) tokens = t;
    } catch (e) { console.error(e); }

    EventsOn('usage', (data) => { if (data) tokens = data; });

    EventsOn('session_changed', (data) => {
      if (!data) return;
      sessionId = data.session?.id || '';
      if (data.tokens) tokens = data.tokens;
      else tokens = { total: { cache:0, input:0, output:0, known:true }, perModel: [], contextUsed: 0, contextWindow: 0 };
      messages = rebuildFromHistory(data.messages || []);
      messageQueue = [];
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
      messages = messages.map(m => m.type==='tool' && m.id===data.id ? {...m, done:true, success:data.success, result:data.output} : m);
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
      messages = [...messages, { _id:mid(), type:'error', content:data.message }];
      busy = false;
      messageQueue = [];
    });

    EventsOn('status', (data) => { status = data.state; });

    EventsOn('permission_request', (data) => { permissionQueue = [...permissionQueue, data]; });

    EventsOn('compaction_start', () => { compacting = true; });
    EventsOn('compaction_end', () => { compacting = false; });

    EventsOn('warnings', (data) => { if (data) warnings = data; });

    EventsOn('subagent_token', (data) => {
      appendSubagentEvent(data.sessionId, { type: 'token', content: data.content });
    });
    EventsOn('subagent_tool_start', (data) => {
      appendSubagentEvent(data.sessionId, { type: 'tool_start', id: data.id, name: data.name, args: data.args });
    });
    EventsOn('subagent_tool_result', (data) => {
      appendSubagentEvent(data.sessionId, { type: 'tool_result', id: data.id, success: data.success, output: data.output });
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
    catch (err) { messages = [...messages, { _id:mid(), type:'error', content:'Compaction failed: ' + err.toString() }]; messageQueue = []; }
    finally { busy = false; }
    await flushQueue();
  }

  async function handleSubmit(e) {
    const content = e.detail;
    if (busy) {
      messageQueue = [...messageQueue, { _id: mid(), content }];
      return;
    }
    busy = true;
    streamingIdx = -1;
    try {
      const turn = await SendPrompt(content);
      currentTurn = turn;
      messages = [...messages, { _id:mid(), type:'user', content, turn }];
    }
    catch (err) { messages = [...messages, { _id:mid(), type:'error', content:err.toString() }]; busy = false; }
  }

  async function flushQueue() {
    if (messageQueue.length === 0) return;
    const queued = messageQueue;
    messageQueue = [];
    busy = true;
    streamingIdx = -1;
    try {
      for (let i = 0; i < queued.length - 1; i++) {
        const turn = await AppendUserMessage(queued[i].content);
        messages = [...messages, { _id: queued[i]._id, type: 'user', content: queued[i].content, turn }];
      }
      const last = queued[queued.length - 1];
      const turn = await SendPrompt(last.content);
      currentTurn = turn;
      messages = [...messages, { _id: last._id, type: 'user', content: last.content, turn }];
    }
    catch (err) { messages = [...messages, { _id: mid(), type: 'error', content: err.toString() }]; busy = false; }
  }

  function handleModelSwitched(e) { provider = e.detail.provider; model = e.detail.model; showModelSelector = false; }

  async function handleRevertCode(e) {
    const { turn, alsoRevertCode } = e.detail;
    try { await RevertCode(turn); }
    catch (err) { messages = [...messages, { _id:mid(), type:'error', content:err.toString() }]; }
  }

  async function handleRevertHistory(e) {
    const { turn, content: msgContent, alsoRevertCode } = e.detail;
    try {
      if (alsoRevertCode) await RevertCode(turn);
      await RevertHistory(turn);
      const hist = await SessionMessages();
      messages = rebuildFromHistory(hist || []);
      inputArea?.prefill(msgContent || '');
    }
    catch (err) { messages = [...messages, { _id:mid(), type:'error', content:err.toString() }]; }
  }

  async function handleFork(e) {
    const { turn, content: msgContent, alsoRevertCode } = e.detail;
    try {
      if (alsoRevertCode) await RevertCode(turn + 1);
      await ForkSession(turn);
      const hist = await SessionMessages();
      messages = rebuildFromHistory(hist || []);
      inputArea?.prefill(msgContent || '');
    }
    catch (err) { messages = [...messages, { _id:mid(), type:'error', content:err.toString() }]; }
  }
  function handleKeydown(e) {
    if ((e.ctrlKey||e.metaKey) && e.key==='m') { e.preventDefault(); showModelSelector = !showModelSelector; }
    if ((e.ctrlKey||e.metaKey) && e.key==='s') { e.preventDefault(); showSessionSelector = !showSessionSelector; }
    if (e.key==='/' && document.activeElement?.tagName !== 'TEXTAREA') { e.preventDefault(); inputArea?.focus(); }
  }
</script>

<svelte:window on:keydown={handleKeydown} />

<main class="app" style="--scale:{$settings.fontScale / 100}">
  <Toolbar {provider} {model} {sessionId} {projectName} {tokens} {compacting} {busy} {warnings}
    on:openModelSelector={() => showModelSelector=true}
    on:openTokens={() => showTokens=true}
    on:compact={handleCompact}
    on:openSessionSelector={() => showSessionSelector=true}
    on:openProjectSelector={() => showProjectSelector=true}
    on:openSettings={() => showSettings=true}
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
  <InputArea bind:this={inputArea} {busy} on:submit={handleSubmit}>
    <StatusBar {provider} {model} on:openModelSelector={() => showModelSelector=true} />
  </InputArea>
  {#if showModelSelector}
    <ModelSelector currentProvider={provider} currentModel={model} on:switched={handleModelSwitched} on:close={() => showModelSelector=false} />
  {/if}

  {#if currentPermission}
    <PermissionPrompt permission={currentPermission} onDone={() => { permissionQueue = permissionQueue.slice(1); }} />
  {/if}
  {#if showTokens}
    <TokenDetails {tokens} on:close={() => showTokens=false} />
  {/if}
  {#if showSessionSelector}
    <SessionSelector on:close={() => showSessionSelector=false} />
  {/if}
  {#if showProjectSelector}
    <ProjectSelector on:close={() => showProjectSelector=false} />
  {/if}
  {#if showSettings}
    <Settings on:close={() => showSettings=false} />
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
