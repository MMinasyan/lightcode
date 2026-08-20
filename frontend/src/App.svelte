<script>
  import { onMount } from 'svelte';
  import { EventsOn } from '../wailsjs/runtime/runtime';
  import { Submit, ApplyTurnAction, CurrentModel, ProjectName, SessionCurrent, CompactNow, HydrateSession, SwitchModel } from '../wailsjs/go/main/App';
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
  import { viewer, appendSubagentEvent, closeViewer } from './lib/viewer.js';
  import { settings } from './lib/settings.js';
  import { errorText } from './lib/errors.js';
  import {
    mergeSubagentLinks,
    subagentLinksFromMetadata,
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
  // presentationGeneration advances on every complete-state snapshot applied
  // (hydration, navigation, turn_action). Promise continuations — submit,
  // compact, model switch, revert, fork — capture {sessionId, generation}
  // before the call and may only touch the view while both still match, so a
  // completion that settles after a navigation can never mutate the newer view.
  let presentationGeneration = 0;

  // Complete-state hydration: listeners register before the snapshot is read, so
  // transcript frames arriving during the read are buffered and replayed after the
  // snapshot is applied. The gate then drops any replayed or live transcript item
  // already represented in the snapshot (sequence at or below its high-water).
  let hydrated = false;
  // A snapshot being applied is what seeds the transcript gate: on a hydration
  // failure or an empty startup nothing was loaded, so the gate stays closed
  // and the composer stays disabled. readOnly comes from the hydration state:
  // a session another process is driving renders its durable transcript but
  // must not admit a new turn here.
  let snapshotApplied = false;
  let readOnly = false;
  let pendingFrames = [];
  let gate = { highWater: 0 };

  function mid() { return nextId++; }

  function defaultTokens() {
    return { total: { cache:0, input:0, output:0, known:true }, perModel: [], contextUsed: 0, contextWindow: 0 };
  }

  // buffered wraps a delivered-frame handler so it holds until the snapshot is
  // applied, then replays in delivery order. Applying the snapshot first and every
  // buffered frame after it keeps a frame delivered during the read from being
  // overwritten by the older captured state. stateful marks a frame that seeds
  // the view (navigation, a state-carrying turn_action): on a failed hydration
  // the frames before the first stateful boundary are discarded, the boundary
  // and everything after it replay, and the gate stays closed only when no
  // boundary seeded the view.
  function buffered(handler, stateful = false) {
    return (data) => {
      if (!hydrated) {
        const marked = typeof stateful === 'function' ? stateful(data) : stateful;
        pendingFrames.push({ handler, data, stateful: marked });
        return;
      }
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

  // A boundary replaces the view wholesale, so a permission prompt open for the
  // session being left closes unanswered. The request is not lost — it stays
  // pending for its own session — so name what was dismissed instead of letting
  // it vanish without a trace.
  function appendPermissionDismissedNotice(p) {
    if (!p) return;
    messages = [...messages, {
      _id: mid(),
      type: 'system',
      content: `System: Dismissed a pending permission request for [${p.tool}] ${p.args} from session ${p.sessionId}; it is still pending for that session.`,
    }];
  }

  function rebuildFromHistory(persisted) {
    currentTurn = 0;
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
    gate = { highWater: 0 };
    continueStreamingRow(data.assistantOpen);
  }

  // applySnapshot renders one complete-state hydration as the whole live view and
  // seeds the transcript gate at its high-water. The queue guard resets to the
  // snapshot's session and version; later same-session versions must increase.
  // Every applied snapshot advances presentationGeneration: the newest applied
  // boundary always wins, and promise continuations captured earlier reject.
  function applySnapshot(hs) {
    if (!hs) return;
    presentationGeneration++;
    // A snapshot replaces the root view wholesale (navigation, detach, the
    // failed-hydration recovery path), so a child viewer open for the previous
    // session closes here: the backend stops delivering that child's frames
    // once the root moves on, and leaving it would freeze a live badge over a
    // view that can no longer receive anything.
    closeViewer();
    snapshotApplied = true;
    readOnly = !!hs.readOnly;
    const destinationId = hs.session?.id || '';
    // A boundary also replaces the pending permission map, so a prompt open for
    // the session being left is dismissed unanswered. The request is not lost —
    // it stays pending for its own session — and the notice below says so when
    // the reseed dismisses one.
    const dismissedPrompt = permissionList(permissions)[0];
    sessionId = destinationId;
    // The toolbar project label follows the destination: the snapshot's session
    // carries the project record's normalized path, and the toolbar shows its
    // basename, so a session switch into another project moves the label with
    // it. A project switch delivers an ordered navigation boundary carrying the
    // destination's model and project name, which this snapshot applies — the
    // model below, the label here — so no out-of-band fetch remains for a
    // switch; the onMount ProjectName() call stays only for the no-session
    // startup, which no snapshot can answer.
    const projectPath = hs.session?.projectPath || '';
    if (projectPath) {
      const base = projectPath.split(/[\\/]+/).filter(Boolean).pop();
      // A root path splits into no segment; the backend's basename of a root
      // directory is the root itself, so the label follows the path then
      // instead of keeping the previous project.
      projectName = base || projectPath;
    }
    messages = rebuildFromHistory(snapshotMessages(hs));
    gate = newTranscriptGate(hs);
    continueStreamingRow(hs.assistantOpen);
    tokens = hs.tokens || defaultTokens();
    busy = !!hs.busy;
    compacting = !!hs.compacting;
    lastQueueVersion = hs.queue?.version || 0;
    messageQueue = (hs.queue?.items || []).map((it) => ({ _id: it.id, content: it.content }));
    warnings = hs.warnings || [];
    permissions = seedPermissions(hs.permissions);
    // The captured state carries the resolved model (identifier plus display
    // name, matching the live model item), so the selector follows the
    // destination session's model; it degrades to the bare ref when the
    // catalog has no entry.
    const m = hs.model || {};
    modelRef = m.ref || ((m.provider && m.model) ? `${m.provider}/${m.model}` : '');
    modelName = m.displayName || modelRef;
    if (dismissedPrompt && dismissedPrompt.sessionId !== destinationId) {
      appendPermissionDismissedNotice(dismissedPrompt);
    }
  }

  async function hydrate() {
    let id = sessionId;
    if (!id) {
      try { const cur = await SessionCurrent(); id = cur?.id || ''; }
      catch (e) { showError(e, 'Load session failed'); }
    }
    if (id) {
      try { applySnapshot(await HydrateSession(id)); }
      catch (e) { showError(e, 'Load session failed'); }
    }
    if (!snapshotApplied) {
      // A failed hydration or an empty startup: frames buffered before the
      // first stateful boundary describe a session the view never loaded, so
      // they are discarded; the first stateful boundary (a navigation or
      // state-carrying turn_action frame) and everything after it seed the
      // view instead. The gate closes below only when no boundary seeded it.
      const firstStateful = pendingFrames.findIndex((f) => f.stateful);
      pendingFrames = firstStateful === -1 ? [] : pendingFrames.slice(firstStateful);
    }
    const buffered = pendingFrames;
    pendingFrames = [];
    hydrated = true;
    for (const frame of buffered) frame.handler(frame.data);
    if (!snapshotApplied) {
      // No boundary seeded the view: close the gate so replayed or live
      // transcript frames cannot render into the empty view. A boundary
      // applied above re-seeded the gate from its own cursor.
      gate.highWater = Infinity;
    }
  }

  // continueStreamingRow resumes a turn that was streaming when a snapshot or
  // resync boundary was captured: when the published fact is true and the last
  // rebuilt row is an assistant row, point the streaming index at it and mark
  // it partial, so the next token extends that row instead of opening a second
  // one. The published fact is the signal; the row-type check is a structural
  // guard. The index is set whenever the row is marked, or the partial flag
  // would be nothing the finalisation can clear; the array is reassigned when
  // it is marked so the view reacts.
  function continueStreamingRow(assistantOpen) {
    const lastRow = messages[messages.length - 1];
    if (assistantOpen && lastRow && lastRow.type === 'assistant') {
      streamingIdx = messages.length - 1;
      messages[streamingIdx] = { ...messages[streamingIdx], partial: true };
      messages = messages;
    } else {
      streamingIdx = -1;
    }
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
    // Child-session links arrive id-keyed afterwards: the backend folds the
    // association into the row before the start frame, so no pending storage
    // is needed here.
    messages = [...messages, { _id: mid(), type: 'tool', id: data.id, name: data.name, args: data.args, done: false, success: true, result: '', subagentSessionIds: [] }];
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
    EventsOn('warnings', buffered((data) => { if (!snapshotApplied) return; if (data) warnings = data; }));
    EventsOn('usage', buffered((data) => { if (!snapshotApplied) return; if (data) tokens = data; }));

    // A resync boundary carries a compacted session's transcript and tokens;
    // applying it refreshes the view without touching activity or queue that
    // compaction does not change.
    EventsOn('resync', buffered((data) => { applyResync(data); }));

    // A navigation boundary carries the destination session's complete state;
    // applying it replaces the whole live view (messages, gate, tokens, activity,
    // queue, warnings, permissions). An empty state is a detach. It is a
    // stateful frame: buffered during a failed hydration, it seeds the view.
    EventsOn('navigation', buffered((data) => { applySnapshot(data); }, true));

    // A turn-action boundary carries the fork/revert destination's complete state,
    // the history revert's input prefill (a nonnull value even when empty, so an
    // empty string still clears the composer; fork and code revert carry none),
    // any code-revert skip notice, and a fork's failed-code-revert warning as one
    // ordered frame; applying the snapshot, the prefill, and the notices together
    // in that order keeps either notice from being clobbered by the replace. A
    // state-carrying frame is stateful: buffered during a failed hydration, it
    // seeds the view.
    EventsOn('turn_action', buffered((data) => {
      applySnapshot(data?.state);
      // A notice-only frame over a view no state has seeded must not apply its
      // notices either: without a seed the notices describe a session the view
      // never shows. A stateful frame may seed the view and then apply them.
      if (!data?.state && !snapshotApplied) return;
      // The prefill is applied before the notices: null/undefined means "do not
      // touch the composer" (fork, code revert), while an explicit empty string
      // still clears it.
      if (data?.prefill != null) inputArea?.prefill(data.prefill);
      appendRevertSkipNotice({ skippedFiles: data?.skippedFiles });
      if (data?.warning) showError(data.warning);
    }, (data) => !!data?.state));

    // A root-model item carries the committed model tagged with the root it
    // switched; apply it to the selector only while that root is the presented
    // session, so a switch never shows on another session or out of order.
    EventsOn('model', buffered((data) => {
      if (!data || data.rootId !== sessionId) return;
      const m = data.model || {};
      modelRef = m.ref || ((m.provider && m.model) ? `${m.provider}/${m.model}` : '');
      modelName = m.displayName || modelRef;
    }));

    EventsOn('token', buffered(applyToken));
    EventsOn('tool_start', buffered(applyToolStart));
    EventsOn('tool_result', buffered(applyToolResult));
    EventsOn('background_process_complete', buffered(applyBackgroundProcess));
    EventsOn('user_message', buffered(applyUserMessage));
    EventsOn('system_signal', buffered(applySystemSignal));

    EventsOn('queue_changed', buffered((data) => {
      if (!snapshotApplied) return;
      if (!data) return;
      const version = data.version || 0;
      if (version <= lastQueueVersion) return; // drop stale/reordered snapshots
      lastQueueVersion = version;
      messageQueue = (data.items || []).map(it => ({ _id: it.id, content: it.content }));
    }));

    // turn_start, turn_end, permission_request and permission_resolved carry
    // no gating sequence, so each is gated on a snapshot having been applied:
    // after a failed hydration or an empty startup nothing is loaded, and a
    // live frame must not raise a busy spinner or an answerable prompt over a
    // session the view never shows.
    EventsOn('turn_start', buffered((data) => {
      if (!snapshotApplied) return;
      busy = true;
      const turn = data?.turn || 0;
      // A turn start names the turn its rows carry. A continuation's row
      // already names the same turn, so a same-turn start replayed from the
      // hydration buffer must not disturb it; a different turn closes it
      // through the close helper. The comparison reads the row's turn before
      // the arriving turn overwrites the current-turn tracking.
      if (streamingIdx !== -1 && messages[streamingIdx] && messages[streamingIdx].turn !== turn) {
        closeStreaming();
      }
      if (turn) currentTurn = turn;
    }));

    EventsOn('turn_end', buffered((data) => {
      if (!snapshotApplied) return;
      closeStreaming();
      busy = false;
      // A pending request blocks its turn, so a turn end (including a cancel) leaves
      // no request that still needs an answer.
      permissions = new Map();
      // Session identity is owned by the ordered navigation/turn_action/hydration
      // boundaries; a turn end never re-resolves it. An out-of-band current-session
      // lookup here could restore a session a concurrent switch already left.
      // Queue draining is backend-owned: the agent auto-drains after the turn
      // ends and emits turn_start + queue_changed. No frontend flush.
    }));

    EventsOn('error', buffered((data) => {
      if (!snapshotApplied) return;
      if (!admitSequenced(gate, data?.seq)) return;
      showError(data?.message || data);
      // A lifecycle error carries no sequence and must never clear an unrelated
      // busy turn; only a sequenced turn error (or the ordered turn_end frame)
      // clears busy.
      if (data?.seq) busy = false;
    }));

    EventsOn('status', buffered((data) => { status = data.state; }));

    EventsOn('permission_request', buffered((data) => {
      if (!snapshotApplied) return;
      permissions = upsertPermission(permissions, data);
    }));

    EventsOn('permission_resolved', buffered((data) => {
      if (!snapshotApplied) return;
      // A pending request was answered or cancelled; drop it from the map so a
      // cancelled prompt is not still shown (and answerable) until turn end.
      if (data?.id) permissions = removePermission(permissions, data.id);
    }));

    EventsOn('compaction_start', buffered(() => { if (!snapshotApplied) return; compacting = true; }));
    EventsOn('compaction_end', buffered(() => { if (!snapshotApplied) return; compacting = false; }));

    // A subagent frame is gated per child by the child's transcript high-water
    // in viewer.js: a frame already represented in the child's snapshot is
    // dropped, everything above it replays in arrival order. Tool results are
    // the exception — they update a row in place without advancing its
    // sequence, so they are delivered id-keyed and applied idempotently.
    EventsOn('subagent_token', buffered((data) => {
      appendSubagentEvent(data.sessionId, { type: 'token', seq: data.seq, content: data.content });
    }));
    EventsOn('subagent_tool_start', buffered((data) => {
      appendSubagentEvent(data.sessionId, { type: 'tool_start', seq: data.seq, id: data.id, name: data.name, args: data.args });
    }));
    EventsOn('subagent_tool_result', buffered((data) => {
      appendSubagentEvent(data.sessionId, { type: 'tool_result', id: data.id, success: data.success, output: data.output, name: data.name, args: data.args, metadata: data.metadata });
    }));
    EventsOn('subagent_background_process_complete', buffered((data) => {
      appendSubagentEvent(data.sessionId, { type: 'background_process_complete', seq: data.seq, id: data.id, command: data.command, reason: data.reason, exitCode: data.exitCode, success: data.success, output: data.output });
    }));
    EventsOn('subagent_user_message', buffered((data) => {
      appendSubagentEvent(data.sessionId, { type: 'user_message', seq: data.seq, turn: data.turn, content: data.content });
    }));
    EventsOn('subagent_system_signal', buffered((data) => {
      appendSubagentEvent(data.sessionId, { type: 'system_signal', seq: data.seq, content: data.content });
    }));
    EventsOn('subagent_session_start', buffered((data) => {
      // The backend folds the association into the parent tool row before the
      // start frame is emitted, so the row exists by the time this id-keyed
      // update arrives; the merge is idempotent either way. No pending-link
      // storage exists: the backend owns the ordering.
      const link = { index: Number(data.taskIndex), sessionId: data.sessionId };
      messages = messages.map(m =>
        m.type === 'tool' && m.id === data.taskToolCallId
          ? { ...m, subagentSessionIds: mergeSubagentLinks(m.subagentSessionIds, [link]) }
          : m
      );
    }));

    // Non-transcript scalars not carried by the snapshot: fetched directly. The
    // empty expected session preserves mount-time routing behavior.
    try {
      const r = await CurrentModel('');
      modelRef = r.model?.ref || ((r.model?.provider && r.model?.model) ? `${r.model.provider}/${r.model.model}` : '');
      modelName = r.model?.displayName || modelRef;
    } catch (e) { showError(e, 'Load model failed'); }
    try { projectName = await ProjectName(); } catch (e) { showError(e, 'Load project failed'); }

    await hydrate();
  });

  async function handleCompact() {
    // The compaction indicator is owned by the ordered compaction_start/end
    // frames; the promise mutates no activity state. Its error shows only
    // while the captured view is still the presented one.
    const opSession = sessionId;
    const opGen = presentationGeneration;
    try { await CompactNow(); }
    catch (err) {
      if (sessionId !== opSession || presentationGeneration !== opGen) return;
      showError(err, 'Compaction failed');
    }
    // Compaction does not change the queue (backend-owned); nothing to flush.
  }

  async function handleSubmit(e) {
    const content = e.detail;
    if (!modelRef) {
      showError("Connect a provider or pick a model before sending.");
      inputArea?.prefill(content);
      return;
    }
    if (!snapshotApplied || readOnly) {
      // The composer gate (send button and Enter) already blocks this; the
      // handler refuses as well, keeping the draft, so a stale dispatch cannot
      // submit into a session the view never loaded or one driven elsewhere.
      showError("No session is open for sending messages.");
      inputArea?.prefill(content);
      return;
    }
    // busy, streaming, and the turn counter are owned by the ordered
    // turn_start/turn_end frames; the submit promise mutates none of them.
    // Its error and prefill continuation apply only while the captured view
    // is still the presented one.
    const opSession = sessionId;
    const opGen = presentationGeneration;
    try {
      await Submit(content);
      // If queued (!started), the queue_changed event drives the dimmed
      // indicator; do not touch busy here.
    }
    catch (err) {
      if (sessionId !== opSession || presentationGeneration !== opGen) return;
      // Rejected (e.g. a session change is in flight): keep the draft so the
      // user can resubmit. InputArea cleared its text on dispatch.
      showError(err);
      inputArea?.prefill(content);
    }
  }

  function handleManageSettings() { showModelSelector = false; settingsSection = 'models'; showSettings = true; }
  // The backend appends an ordered model item that updates the selector; the
  // switch handler only invokes the switch against the captured view and
  // closes the picker, never sets the model out of band.
  async function handleModelSelect(e) {
    const opSession = sessionId;
    const opGen = presentationGeneration;
    try { await SwitchModel(e.detail.ref); }
    catch (err) {
      if (sessionId !== opSession || presentationGeneration !== opGen) return;
      showError(err);
      return;
    }
    if (sessionId !== opSession || presentationGeneration !== opGen) return;
    showModelSelector = false;
  }
  async function refreshCurrentModel() {
    // The settings-close reload must not overwrite a newer presentation: capture
    // the session, generation, and both model fields, expect the captured session
    // from the backend, and mutate only while all four still match. This blocks
    // backend-route-before-boundary, A→B, a detach, a re-selected A (A→B→A), and
    // a newer same-session model frame delivered before this promise resolves.
    const opSession = sessionId;
    const opGen = presentationGeneration;
    const opRef = modelRef;
    const opName = modelName;
    try {
      const r = await CurrentModel(opSession);
      // The backend marked the result superseded when the routed session no
      // longer matches the expected one; ignore it entirely.
      if (r?.superseded) return;
      if (sessionId !== opSession || presentationGeneration !== opGen || modelRef !== opRef || modelName !== opName) return;
      const m = r?.model || {};
      modelRef = m.ref || ((m.provider && m.model) ? `${m.provider}/${m.model}` : '');
      modelName = m.displayName || modelRef;
    } catch (e) {
      if (sessionId !== opSession || presentationGeneration !== opGen || modelRef !== opRef || modelName !== opName) return;
      showError(e, 'Load model failed');
    }
  }

  async function handleRevertCode(e) {
    const { turn } = e.detail;
    const opSession = sessionId;
    const opGen = presentationGeneration;
    try {
      // The backend appends the skip notice as an ordered turn_action frame; no
      // out-of-band append here that a queued refresh could clobber.
      await ApplyTurnAction(turn, 'revert_code', false);
    }
    catch (err) {
      if (sessionId !== opSession || presentationGeneration !== opGen) return;
      showError(err);
    }
  }

  async function handleRevertHistory(e) {
    const { turn, alsoRevertCode } = e.detail;
    const opSession = sessionId;
    const opGen = presentationGeneration;
    try {
      // The backend appends the reverted session's complete state, the input
      // prefill, any skip notice, and the warning as one ordered turn_action
      // boundary; the prefill is applied only there, never from this promise,
      // so its delivery cannot reorder against the boundary.
      await ApplyTurnAction(turn, 'revert_history', !!alsoRevertCode);
    }
    catch (err) {
      if (sessionId !== opSession || presentationGeneration !== opGen) return;
      showError(err);
    }
  }

  async function handleFork(e) {
    const { turn, alsoRevertCode } = e.detail;
    const opSession = sessionId;
    const opGen = presentationGeneration;
    try {
      // The backend appends the forked session's complete state, any skip
      // notice, and a failed code revert's warning as one ordered turn_action
      // frame; nothing is applied here.
      await ApplyTurnAction(turn, 'fork', !!alsoRevertCode);
    }
    catch (err) {
      if (sessionId !== opSession || presentationGeneration !== opGen) return;
      showError(err);
    }
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
  <InputArea bind:this={inputArea} busy={busy || compacting} hasActiveModel={!!modelRef && snapshotApplied && !readOnly} on:submit={handleSubmit} on:error={(e) => showError(e.detail)}>
    <StatusBar {modelName} on:openModelSelector={() => showModelSelector=true} />
  </InputArea>
  {#if showModelSelector}
    <ModelSelector currentRef={modelRef} on:select={handleModelSelect} on:close={() => showModelSelector=false} on:manageSettings={handleManageSettings} on:error={(e) => showError(e.detail)} />
  {/if}

  {#if showSettings}
    <Settings initialSection={settingsSection} on:close={() => { showSettings = false; refreshCurrentModel(); }} on:error={(e) => showError(e.detail)} />
  {/if}

  {#if currentPermission}
    {#key currentPermission.id}
      <PermissionPrompt permission={currentPermission} onDone={(id) => { permissions = removePermission(permissions, id); }} on:error={(e) => showError(e.detail)} />
    {/key}
  {/if}
  {#if showTokens}
    <TokenDetails {tokens} on:close={() => showTokens=false} />
  {/if}
  {#if showSessionSelector}
    <SessionSelector seeded={snapshotApplied} currentSessionId={sessionId} generation={presentationGeneration} on:close={() => showSessionSelector=false} on:error={(e) => showError(e.detail)} />
  {/if}
  {#if showProjectSelector}
    <ProjectSelector seeded={snapshotApplied} currentSessionId={sessionId} generation={presentationGeneration} on:close={() => showProjectSelector=false} on:error={(e) => showError(e.detail)} />
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
