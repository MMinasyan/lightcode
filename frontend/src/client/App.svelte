<script>
  import { onDestroy, onMount } from 'svelte';
  import {
    compactSession,
    createSession,
    getConfiguration,
    getSessionHistory,
    getSessionHydration,
    getWarnings,
    interruptSession,
    listModels,
    listSessions,
    setAgentTypeModel,
    stopSession,
    submitSession,
  } from '../generated/protocol/sdk.gen';
  import { errorText } from '../lib/errors.js';
  import { settings } from '../lib/settings.js';
  import { ProvisionalActivity } from './activity';
  import { AttemptLedger } from './attempts';
  import { classifyResult } from './results';
  import { SessionReader, okRead, failRead, sameStamp } from './reader';
  import { sortOpenRoots } from './sessions';
  import { compareConfigurationRevision, compareWarningsRevision } from './projection';
  import Conversation from './Conversation.svelte';
  import Composer from './Composer.svelte';
  import SessionPicker from './SessionPicker.svelte';
  import ModelDialog from './ModelDialog.svelte';
  import BackgroundPanel from './BackgroundPanel.svelte';

  export let connection = null;

  // Connection and per-view selection stamps. The root view and the inspector
  // are independent authorities: opening or closing the inspector invalidates
  // only the inspector generation, while a root switch or a repair invalidates
  // both. Every asynchronous completion captures its stamp before the call.
  let instance = '';
  let generation = '';
  let workspace = '';
  let rootSelection = 0;
  let childSelection = 0;

  let rootID = '';
  let childID = '';
  let rootReader = null;
  let childReader = null;
  let rootActivity = new ProvisionalActivity(bump);
  let childActivity = new ProvisionalActivity(bump);
  let attempts = new AttemptLedger(bump);

  // Independently accepted global clocks. They come from the host's own
  // stamped GetConfiguration/GetWarnings reads and gate hydration adoption.
  let configurationFloor = null;
  let warningsFloor = null;

  // Draft state owned by the App: every edit advances the revision, so a
  // disposition only ever clears the exact submitted revision.
  let draft = '';
  let draftRevision = 0;
  let draftShadow = '';

  // Display state.
  let tick = 0;
  let openError = '';
  let outcomeNote = '';
  let repairNote = '';
  let repairTask = null;
  let disposed = false;
  let showSessionPicker = false;
  let pickerRoots = [];
  let showModelDialog = false;
  let modelLoading = false;
  let modelError = '';
  let models = [];
  let showBackgroundPanel = false;
  let backgroundChildren = [];
  let backgroundJobs = [];
  let showWarnings = false;
  let composer;
  let childPaneEl;
  let warningsDialog;

  function bump() { tick += 1; }

  // A contained native dialog: showModal on mount, close on backdrop click or
  // Escape, and destroy() returns focus by closing whatever is still open.
  function openDialog(node) {
    node.showModal();
    return {
      destroy() {
        if (node.open) node.close();
      },
    };
  }

  let viewItems = [];
  let viewHydration = null;
  let viewError = null;
  let viewBusy = false;
  let viewHasOlder = false;
  let childItems = [];
  let childHydration = null;
  let childError = null;
  let childHasOlder = false;
  let activeOperation = null;
  let activeRunning = false;
  let pending = { steering: [], queued: [] };
  let usage = { cache: '0', input: '0', output: '0', used: '0', window: 0 };
  let ownWarnings = [];
  let globalWarnings = [];
  let selectedModel = '';
  let agentType = '';
  let rootActivityState = null;
  let childActivityState = null;
  let outstandingAttempts = [];

  $: {
    void tick;
    viewItems = rootReader ? rootReader.getItems() : [];
    viewHydration = rootReader ? rootReader.getHydration() : null;
    viewError = rootReader ? rootReader.getError() : null;
    viewBusy = rootReader ? rootReader.isBusy() : false;
    viewHasOlder = rootReader ? rootReader.hasOlder() : false;
    childItems = childReader ? childReader.getItems() : [];
    childHydration = childReader ? childReader.getHydration() : null;
    childError = childReader ? childReader.getError() : null;
    childHasOlder = childReader ? childReader.hasOlder() : false;
    activeOperation = viewHydration && viewHydration.active_operation ? viewHydration.active_operation : null;
    activeRunning = activeOperation !== null && activeOperation.status === 'running';
    pending = viewHydration ? viewHydration.pending : { steering: [], queued: [] };
    usage = usageTotals(viewHydration);
    ownWarnings = viewHydration ? viewHydration.warnings.filter((warning) => warning.session_id === rootID) : [];
    globalWarnings = viewHydration ? viewHydration.warnings.filter((warning) => !warning.session_id) : [];
    selectedModel = viewHydration && viewHydration.selected_model ? viewHydration.selected_model : '';
    agentType = viewHydration ? viewHydration.session.agent_type : '';
    rootActivityState = rootActivity.getState();
    childActivityState = childActivity.getState();
    outstandingAttempts = attempts.outstanding(rootID);
  }

  $: if (draft !== draftShadow) {
    draftRevision += 1;
    draftShadow = draft;
  }

  // Every outstanding unknown/rejected attempt shows its own duplication
  // warning beside its Retry affordance before it is invoked; there is no
  // retry-registry lifetime state.

  // Usage totals stay exact decimal strings summed through BigInt; the
  // Runtime's FormatInt values are always signed decimal digit strings.
  function usageTotals(hydration) {
    if (!hydration) return { cache: '0', input: '0', output: '0', used: '0', window: 0 };
    let cache = 0n;
    let input = 0n;
    let output = 0n;
    for (const entry of hydration.usage.totals.by_model) {
      cache += BigInt(entry.usage.cached_input_tokens);
      input += BigInt(entry.usage.input_tokens);
      output += BigInt(entry.usage.output_tokens);
    }
    return {
      cache: cache.toString(),
      input: input.toString(),
      output: output.toString(),
      used: hydration.usage.context.used_tokens,
      window: hydration.usage.context.context_window,
    };
  }

  function clientNow() {
    return connection.getClient();
  }

  function stampFor(sessionID, view) {
    const selection = view === 'child' ? childSelection : rootSelection;
    return () => ({ instance, generation, selection, sessionID });
  }

  // The one shared generated-SDK rule for the actual consumer: the client
  // runs with throwOnError=false, so a received HTTP status always arrives as
  // a result and only a binding/fetch failure (including a connection handle
  // that can no longer serve the call) leaves the response absent. classifyResult
  // then distinguishes loss, known rejection and invalid delivery without any
  // Error-instanceof or catch-only guesswork.
  async function invoke(label, call) {
    try {
      return classifyResult(await call(clientNow()), label);
    } catch (error) {
      return { kind: 'loss', message: `${label}: ${errorText(error)}` };
    }
  }

  // Reads: a received non-2xx or an absent response is an ordinary (stamped)
  // read failure; a received success without a decodable result is invalid
  // delivery, which the reader routes to its existing repair/reset path.
  function readOutcome(classified) {
    if (classified.kind === 'ok') return okRead(classified.data);
    if (classified.kind === 'invalid') return failRead('invalid', classified.message);
    return failRead('ordinary', classified.message);
  }

  function readerTransport(sessionID) {
    return {
      async hydration() {
        return readOutcome(await invoke('hydration', (client) => getSessionHydration({ client, path: { id: sessionID } })));
      },
      async history(_stamp, cursor) {
        return readOutcome(await invoke('history', (client) => getSessionHistory({
          client,
          path: { id: sessionID },
          query: cursor ? { cursor } : undefined,
        })));
      },
    };
  }

  function readerRepair(reason) {
    repairNote = reason;
    tick += 1;
    void requestRepair();
  }

  function makeReader(sessionID, view) {
    const reader = new SessionReader(
      readerTransport(sessionID),
      stampFor(sessionID, view),
      readerRepair,
      bump,
      (hydration) => adoptHydration(sessionID, hydration),
    );
    reader.setFloors(configurationFloor, warningsFloor);
    return reader;
  }

  // adoptHydration runs when a displayed Session's hydration is accepted:
  // the provisional activity is replaced by the authoritative projection, and
  // the returned newly-accepted descriptors drive draft clearing by their own
  // captured revisions. The ledger already removed them.
  function adoptHydration(sessionID, hydration) {
    if (sessionID === rootID) rootActivity.clear();
    if (sessionID === childID) childActivity.clear();
    for (const attempt of attempts.adoptHydration(sessionID, hydration)) {
      if (attempt.kind === 'submit' && attempt.draftRevision === draftRevision) {
        draft = '';
      }
    }
  }

  // One repair owner spans cancel/discard, Open/SSE establishment and the
  // COMPLETION of the fresh global/root/child reads (and the shared selection
  // owner when no root is adopted yet). A loss of the CURRENT generation that
  // arrives while an owner is running is coalesced into that owner — it re-
  // drives ONE more serialized reopen after the running task settles, instead
  // of being swallowed or starting an overlapping owner. A loss already
  // superseded by a pending/newer Open is subsumed by the caller's generation
  // check. Disposal never reconnects; there is no backoff, retry cap or timer.
  let repairPending = false;

  function requestRepair() {
    if (disposed) return undefined;
    if (repairTask !== null) {
      // A current-generation loss during an in-flight owner: record the need
      // for a replacement the same owner will honour when it settles.
      repairPending = true;
      return repairTask;
    }
    repairTask = runRepair().finally(() => { repairTask = null; });
    return repairTask;
  }

  async function runRepair() {
    // One serialized owner may re-drive itself for a coalesced current loss.
    do {
      repairPending = false;
      await performRepair();
      // A loss observed against the generation this owner installed re-enters
      // here; the loop re-checks the coalesced flag set during the fresh reads.
    } while (!disposed && repairPending);
    repairPending = false;
  }

  async function performRepair() {
    if (disposed) return;
    // Discard the old generation: dispose the readers and take them out of
    // event/command reach before anything else can observe the gap.
    if (rootReader) { rootReader.dispose(); rootReader = null; }
    if (childReader) { childReader.dispose(); childReader = null; }
    rootSelection += 1;
    childSelection += 1;
    rootActivity.clear();
    childActivity.clear();
    try {
      const info = await connection.open();
      if (disposed) return;
      instance = info.instance_id;
      generation = info.generation;
      workspace = info.workspace;
      // Accepted global floors are per-instance state: a repair discards them
      // alongside projections and cursors, and this owner's own fresh global
      // reads replace them before rendering resumes.
      configurationFloor = null;
      warningsFloor = null;
      const tasks = [refreshGlobalFloors()];
      if (rootID) {
        rootReader = makeReader(rootID, 'root');
        rootReader.markDirty();
        tasks.push(rootReader.currentChain());
      } else {
        // No root adopted yet: the repair runs the SAME shared selection owner
        // rather than a second initialization path, so a superseded boot
        // list/create is finished and re-read here.
        tasks.push(ensureSelection());
      }
      if (childID) {
        childReader = makeReader(childID, 'child');
        childReader.markDirty();
        tasks.push(childReader.currentChain());
      }
      // Await the completion of every fresh read this owner launched before it
      // releases; a current-generation SSE loss during them sets repairPending
      // and re-drives the same serialized owner.
      await Promise.all(tasks);
    } catch (error) {
      // The failure of the repair's own read stays inside this owner: no
      // recursive open, no retry ladder.
      openError = 'Runtime reconnect failed: ' + errorText(error);
    } finally {
      tick += 1;
    }
  }

  // --- Global configuration/warning floors ---------------------------------

  function pushFloors() {
    if (rootReader) rootReader.setFloors(configurationFloor, warningsFloor);
    if (childReader) childReader.setFloors(configurationFloor, warningsFloor);
  }

  async function refreshGlobalFloors() {
    await Promise.all([refreshConfigurationFloor(), refreshWarningsFloor()]);
  }

  async function refreshConfigurationFloor() {
    const cap = stampFor(rootID, 'root')();
    const classified = await invoke('configuration', (client) => getConfiguration({ client }));
    if (!sameStamp(cap, stampFor(rootID, 'root')())) return;
    if (classified.kind !== 'ok') {
      if (classified.kind === 'invalid') { repairNote = classified.message; void requestRepair(); }
      else outcomeNote = classified.message;
      tick += 1;
      return;
    }
    void acceptConfigurationFloor(classified.data.configuration_revision);
  }

  async function refreshWarningsFloor() {
    const cap = stampFor(rootID, 'root')();
    const classified = await invoke('warnings', (client) => getWarnings({ client }));
    if (!sameStamp(cap, stampFor(rootID, 'root')())) return;
    if (classified.kind !== 'ok') {
      if (classified.kind === 'invalid') { repairNote = classified.message; void requestRepair(); }
      else outcomeNote = classified.message;
      tick += 1;
      return;
    }
    void acceptWarningsFloor(classified.data.warnings_revision);
  }

  // acceptConfigurationFloor advances the independent floor only when the
  // scalar is newer; an equal or older completion changes nothing; a foreign
  // or malformed revision requests repair. Displayed views refresh when the
  // floor advances.
  function acceptConfigurationFloor(revision) {
    let advanced = false;
    if (configurationFloor === null) {
      configurationFloor = revision;
      advanced = true;
    } else {
      let comparison;
      try {
        comparison = compareConfigurationRevision(configurationFloor, revision);
      } catch {
        repairNote = 'configuration revision is malformed or foreign';
        void requestRepair();
        return;
      }
      if (comparison === -1) {
        configurationFloor = revision;
        advanced = true;
      }
    }
    pushFloors();
    if (advanced) {
      if (rootReader) rootReader.markDirty();
      if (childReader) childReader.markDirty();
    }
    tick += 1;
  }

  function acceptWarningsFloor(revision) {
    let advanced = false;
    if (warningsFloor === null) {
      warningsFloor = revision;
      advanced = true;
    } else {
      let comparison;
      try {
        comparison = compareWarningsRevision(warningsFloor, revision);
      } catch {
        repairNote = 'warnings revision is malformed or foreign';
        void requestRepair();
        return;
      }
      if (comparison === -1) {
        warningsFloor = revision;
        advanced = true;
      }
    }
    pushFloors();
    if (advanced) {
      if (rootReader) rootReader.markDirty();
      if (childReader) childReader.markDirty();
    }
    tick += 1;
  }

  // --- Composition and selection -------------------------------------------

  // ONE initial-selection owner, invoked from boot and from a repair that has
  // not yet adopted a root. A list/create completion that a repair superseded
  // (stamp moved) never leaves the app permanently sessionless: the owner stops
  // adopting and the repair that superseded it re-runs a fresh current attempt.
  let selectionTask = null;
  // An unknown Create is never blind-retransmitted: a fresh list may discover
  // its real header. Until a header proves it, the unknown creation is kept.
  let unknownCreation = false;
  // A completion superseded by a newer request records the need for a fresh
  // attempt the same serialized owner honours when the running one settles.
  let selectionPending = false;

  function ensureSelection() {
    if (selectionTask !== null) {
      selectionPending = true;
      return selectionTask;
    }
    selectionTask = driveSelection().finally(() => { selectionTask = null; });
    return selectionTask;
  }

  async function driveSelection() {
    do {
      selectionPending = false;
      await runSelection();
    } while (!disposed && selectionPending);
    selectionPending = false;
  }

  async function runSelection() {
    const cap = stampFor('', 'root')();
    // invoke() is the one catch/classification boundary (including a throwing
    // clientNow()); no outer try/catch wraps it here.
    const classified = await invoke('session list', (client) => listSessions({ client, query: { workspace, lifecycle: 'open' } }));
    if (classified.kind !== 'ok') {
      if (!sameStamp(cap, stampFor('', 'root')())) return;
      openError = classified.message;
      tick += 1;
      return;
    }
    const sessions = classified.data;
    if (!sameStamp(cap, stampFor('', 'root')())) return;
    const roots = sortOpenRoots(sessions);
    if (roots.length > 0) {
      // A discovered header (including one a prior unknown Create actually
      // produced) settles the unknown-creation note.
      unknownCreation = false;
      outcomeNote = '';
      selectRoot(roots[0].session_id);
      return;
    }
    if (unknownCreation) {
      // The list proved no header exists for the lost Create; keep showing the
      // unknown creation rather than send a second Create blindly.
      openError = '';
      outcomeNote = 'Session create outcome unknown for workspace ' + workspace + '; not retransmitted.';
      tick += 1;
      return;
    }
    // invoke() is the one catch/classification boundary; no outer try/catch.
    const created = await invoke('session create', (client) => createSession({
      client,
      body: { workspace, agent_type: 'primary' },
    }));
    // The Create result/error is reported against its captured address even if
    // the view moved; only the stamped selection adoption below is gated.
    if (created.kind === 'rejection') {
      if (!sameStamp(cap, stampFor('', 'root')())) return;
      openError = 'Session creation failed: ' + created.message;
      tick += 1;
      return;
    }
    if (created.kind !== 'ok') {
      unknownCreation = true;
      if (!sameStamp(cap, stampFor('', 'root')())) return;
      outcomeNote = 'Session create outcome unknown for workspace ' + workspace + '; not retransmitted.';
      tick += 1;
      return;
    }
    if (!sameStamp(cap, stampFor('', 'root')())) return;
    selectRoot(created.data.session_id);
  }

  // A root switch invalidates both views: the inspector closes and any
  // in-flight read of either generation is superseded.
  function selectRoot(id) {
    rootSelection += 1;
    childSelection += 1;
    rootID = id;
    if (childReader) { childReader.dispose(); childReader = null; }
    childID = '';
    childActivity.clear();
    if (rootReader) rootReader.dispose();
    rootReader = makeReader(id, 'root');
    rootActivity.clear();
    rootReader.markDirty();
    tick += 1;
  }

  // Opening or closing the inspector invalidates only the inspector
  // generation, so an in-flight root read is never stranded by it.
  function openChild(id) {
    if (id === childID) return;
    childSelection += 1;
    childID = id;
    if (childReader) childReader.dispose();
    childReader = makeReader(id, 'child');
    childActivity.clear();
    childReader.markDirty();
    tick += 1;
  }

  function closeChild() {
    if (!childID) return;
    childSelection += 1;
    childID = '';
    if (childReader) { childReader.dispose(); childReader = null; }
    childActivity.clear();
    tick += 1;
  }

  // --- Event dispatch -------------------------------------------------------

  function handleEvent(event) {
    const kind = event.kind;
    if (kind === 'session_changed' || kind === 'configuration_changed' || kind === 'warning_changed') {
      if (kind === 'configuration_changed') {
        void refreshConfigurationFloor();
      } else if (kind === 'warning_changed') {
        void refreshWarningsFloor();
      }
      for (const [reader, sessionID] of [[rootReader, rootID], [childReader, childID]]) {
        if (!reader) continue;
        // Configuration and warning clocks are global: their hints dirty
        // every displayed view even under an absent or foreign Session scope.
        if (kind === 'session_changed' && event.scope && event.scope.session_id && event.scope.session_id !== sessionID) continue;
        const hintKind = kind === 'session_changed' ? 'session' : (kind === 'configuration_changed' ? 'configuration' : 'warning');
        const revision = kind === 'session_changed'
          ? event.session_revision
          : (kind === 'configuration_changed' ? event.configuration_revision : event.warnings_revision);
        if (reader.hint(revision, hintKind) === 'repair') return;
      }
      return;
    }
    if (kind === 'text_delta' || kind === 'refusal_delta' || kind === 'tool_started' || kind === 'tool_finished') {
      const sessionID = event.scope && event.scope.session_id;
      if (sessionID === rootID && rootReader) {
        if (!rootActivity.accept(acceptedActiveID(rootReader), event)) rootReader.markDirty();
      } else if (sessionID === childID && childReader) {
        if (!childActivity.accept(acceptedActiveID(childReader), event)) childReader.markDirty();
      }
      return;
    }
    // scope_opened / scope_closed carry no display behavior.
  }

  function acceptedActiveID(reader) {
    const active = reader ? reader.getHydration()?.active_operation : null;
    return active && active.status === 'running' ? active.operation_id : null;
  }

  // --- Composer and controls -------------------------------------------------

  function submit(mode) {
    if (!rootID || !rootReader) return;
    const content = draft.trim();
    if (!content) return;
    const revision = draftRevision;
    const attempt = attempts.create('submit', mode, [{ kind: 'text', text: content }], rootID, revision);
    void settleAttempt(attempt);
  }

  // The outcome ALWAYS lands on the captured address: delivered, known
  // rejection or unknown transport outcome are recorded against the attempt
  // regardless of any later selection/connection change. Only the stale reader
  // refresh and the draft-clearing rule stay gated by their own captured state.
  // Submit and Compact share this ONE settlement path; only the generated call
  // and body differ by attempt.kind.
  async function settleAttempt(attempt) {
    const cap = stampFor(attempt.sessionID, 'root')();
    const classified = attempt.kind === 'compact'
      ? await invoke('compact', (client) => compactSession({
        client,
        path: { id: attempt.sessionID },
        body: { operation_id: attempt.id },
      }))
      : await invoke('submit', (client) => submitSession({
        client,
        path: { id: attempt.sessionID },
        body: { operation_id: attempt.id, mode: attempt.mode, content: attempt.content },
      }));
    if (classified.kind === 'rejection') {
      attempts.markRejected(attempt.id, classified.message);
      return;
    }
    if (classified.kind !== 'ok') {
      attempts.markUnknown(attempt.id);
      if (sameStamp(cap, stampFor(attempt.sessionID, 'root')()) && rootReader) rootReader.markDirty();
      return;
    }
    clearMatchingDraft(attempt);
    attempts.remove(attempt.id);
    if (sameStamp(cap, stampFor(attempt.sessionID, 'root')()) && rootReader) rootReader.markDirty();
  }

  // Draft clearing compares the attempt's captured revision with the current
  // draft; later-typed text is never cleared, and compact never touches it.
  function clearMatchingDraft(attempt) {
    if (attempt.kind !== 'submit') return;
    if (attempt.draftRevision === draftRevision) draft = '';
  }

  function retryAttempt(attempt) {
    attempts.markRetrying(attempt.id);
    void settleAttempt(attempt);
  }

  function abandonAttempt(attempt) {
    attempts.remove(attempt.id);
  }

  function compactNow() {
    if (!rootID) return;
    const attempt = attempts.create('compact', 'regular', [], rootID, draftRevision);
    void settleAttempt(attempt);
  }

  async function addressControl(operation, label, sessionID) {
    const cap = stampFor(sessionID, 'root')();
    const classified = await invoke(label, (client) => operation({ client, path: { id: sessionID } }));
    if (classified.kind === 'rejection') {
      // The addressed failure is reported even after the view moved on.
      outcomeNote = `${label} failed for session ${sessionID}: ${classified.message}`;
      tick += 1;
      return;
    }
    if (classified.kind !== 'ok') {
      outcomeNote = `${label} outcome unknown for session ${sessionID}; nothing was retransmitted.`;
      tick += 1;
      return;
    }
    if (!sameStamp(cap, stampFor(sessionID, 'root')())) return;
    if (rootReader) rootReader.markDirty();
    tick += 1;
  }

  function interruptNow() {
    if (!rootID) return;
    void addressControl(interruptSession, 'Interrupt', rootID);
  }

  function stopNow() {
    if (!rootID) return;
    void addressControl(stopSession, 'Stop', rootID);
  }

  // --- Navigation and pickers -------------------------------------------------

  async function openSessionPicker() {
    const cap = stampFor(rootID, 'root')();
    const classified = await invoke('session list', (client) => listSessions({ client, query: { workspace, lifecycle: 'open' } }));
    if (!sameStamp(cap, stampFor(rootID, 'root')())) return;
    if (classified.kind !== 'ok') {
      outcomeNote = classified.message;
      tick += 1;
      return;
    }
    pickerRoots = sortOpenRoots(classified.data).map((session) => ({
      id: session.session_id,
      agentType: session.agent_type,
      lastActivity: session.last_activity,
    }));
    showSessionPicker = true;
  }

  function toggleSessionPicker() {
    if (showSessionPicker) {
      showSessionPicker = false;
    } else {
      void openSessionPicker();
    }
  }

  async function openModelDialog() {
    showModelDialog = true;
    modelLoading = true;
    modelError = '';
    const cap = stampFor(rootID, 'root')();
    const classified = await invoke('model list', (client) => listModels({ client, query: { all: false } }));
    if (!sameStamp(cap, stampFor(rootID, 'root')())) return;
    if (classified.kind === 'ok') {
      models = classified.data.models
        .filter((entry) => !entry.incomplete)
        .map((entry) => ({
          ref: entry.ref,
          provider: entry.provider,
          providerName: entry.provider_name,
          displayName: entry.display_name,
          model: entry.model,
        }));
    } else {
      modelError = classified.message;
    }
    modelLoading = false;
    tick += 1;
  }

  function toggleModelDialog() {
    if (showModelDialog) {
      showModelDialog = false;
    } else {
      void openModelDialog();
    }
  }

  // SetAgentTypeModel is type-wide: the completion reports against the
  // captured agent type even after a selection move, and its published
  // configuration revision advances the independent floor.
  async function pickModel(ref) {
    const addressedType = agentType;
    if (!addressedType) return;
    const cap = stampFor(rootID, 'root')();
    const classified = await invoke('model selection', (client) => setAgentTypeModel({
      client,
      query: { agent_type: addressedType },
      body: { model: ref },
    }));
    if (classified.kind === 'rejection') {
      modelError = classified.message;
      outcomeNote = `Model selection failed for agent type ${addressedType}: ${classified.message}`;
      tick += 1;
      return;
    }
    if (classified.kind !== 'ok') {
      outcomeNote = `Model selection outcome unknown for agent type ${addressedType}; nothing was retransmitted.`;
      tick += 1;
      return;
    }
    // The success is reported against the captured type unconditionally...
    outcomeNote = `Model selection applied for agent type ${addressedType}.`;
    if (!sameStamp(cap, stampFor(rootID, 'root')())) {
      // ...but a response from an old connection/instance/selection advances no
      // current floor, closes no modal and triggers no repair: the current
      // global views re-read through their normal generated owners instead.
      void refreshConfigurationFloor();
      tick += 1;
      return;
    }
    showModelDialog = false;
    // A current result refreshes the configuration view through its own read
    // rather than force-adopting the mutation's clock as the floor.
    void refreshConfigurationFloor();
    tick += 1;
  }

  // The child panel is one ID-keyed union of lineage headers from BOTH
  // lifecycle lists plus the live hydration membership. Live children missing
  // from the lists remain inspectable by their actual ID with no fabricated
  // header detail; jobs stay a separate section.
  async function openBackgroundPanel() {
    const cap = stampFor(rootID, 'root')();
    // invoke() is the one catch/classification boundary for both reads.
    const results = await Promise.all([
      invoke('session list', (client) => listSessions({ client, query: { workspace, lifecycle: 'open' } })),
      invoke('session list', (client) => listSessions({ client, query: { workspace, lifecycle: 'archived' } })),
    ]);
    const openList = results[0];
    const archivedList = results[1];
    if (!sameStamp(cap, stampFor(rootID, 'root')())) return;
    if (openList.kind !== 'ok' || archivedList.kind !== 'ok') {
      outcomeNote = (openList.kind !== 'ok' ? openList : archivedList).message;
      tick += 1;
      return;
    }
    const byId = new Map();
    for (const header of [...openList.data, ...archivedList.data]) {
      if (header.parent_session_id !== rootID) continue;
      const existing = byId.get(header.session_id);
      if (existing) {
        // Independently captured lists can overlap during a lifecycle change;
        // one ID-keyed row wins and the live flag merges.
        existing.lifecycle = existing.lifecycle === 'open' || header.lifecycle === 'open' ? 'open' : 'archived';
        continue;
      }
      byId.set(header.session_id, {
        id: header.session_id,
        agentType: header.agent_type,
        lifecycle: header.lifecycle,
        lastActivity: header.last_activity,
        live: false,
        unknownHeader: false,
      });
    }
    for (const member of viewHydration ? viewHydration.background : []) {
      if (member.kind !== 'child') continue;
      const existing = byId.get(member.id);
      if (existing) {
        existing.live = true;
      } else {
        byId.set(member.id, { id: member.id, agentType: '', lifecycle: 'live', lastActivity: '', live: true, unknownHeader: true });
      }
    }
    backgroundChildren = [...byId.values()];
    backgroundJobs = (viewHydration ? viewHydration.background : [])
      .filter((member) => member.kind === 'job')
      .map((member) => ({ id: member.id }));
    showBackgroundPanel = true;
  }

  // --- Keyboard ---------------------------------------------------------------

  function handleKeydown(e) {
    if ((e.ctrlKey || e.metaKey) && e.key === 'm') { e.preventDefault(); toggleModelDialog(); }
    if ((e.ctrlKey || e.metaKey) && e.key === 's') { e.preventDefault(); toggleSessionPicker(); }
    if (e.key === '/' && document.activeElement?.tagName !== 'TEXTAREA' && document.activeElement?.tagName !== 'INPUT') {
      e.preventDefault();
      composer?.focus();
    }
    if (e.key === 'Escape') {
      // An actual modal/menu closes first (the native dialog closes on its
      // own and reports through on:close); then the composer alone interrupts
      // and leaves a nonmodal child pane open; the pane closes only when
      // focus is inside it.
      if (showModelDialog) { showModelDialog = false; return; }
      if (showSessionPicker) { showSessionPicker = false; return; }
      if (showBackgroundPanel) { showBackgroundPanel = false; return; }
      if (showWarnings) { showWarnings = false; return; }
      if (document.activeElement?.tagName === 'TEXTAREA') { e.preventDefault(); void interruptNow(); return; }
      if (childID && childPaneEl && childPaneEl.contains(document.activeElement)) closeChild();
    }
  }

  // --- Lifecycle ---------------------------------------------------------------

  onMount(async () => {
    connection.setEventHandler(handleEvent);
    connection.setFailureHandler((error, lostGeneration) => {
      // A loss already superseded by a newer Open is subsumed: it must not
      // invalidate the freshly installed views. Only the genuinely current
      // generation's loss is recorded; while an owner runs requestRepair
      // coalesces the replacement instead of swallowing it.
      if (connection.isCurrentGeneration && !connection.isCurrentGeneration(lostGeneration)) {
        return;
      }
      repairNote = 'Event stream lost: ' + errorText(error);
      tick += 1;
      void requestRepair();
    });
    try {
      const info = await connection.open();
      if (disposed) return;
      instance = info.instance_id;
      generation = info.generation;
      workspace = info.workspace;
    } catch (error) {
      openError = 'Runtime connection failed: ' + errorText(error);
      tick += 1;
      return;
    }
    await refreshGlobalFloors();
    await ensureSelection();
  });

  onDestroy(() => {
    disposed = true;
    connection.dispose();
    if (rootReader) rootReader.dispose();
    if (childReader) childReader.dispose();
  });
</script>

<svelte:window on:keydown={handleKeydown} />

<main class="app" style="--scale:{$settings.fontScale / 100}">
  <div class="toolbar">
    <button class="label" on:click={toggleSessionPicker} title={workspace || 'workspace'} aria-label="Select session">
      <span class="project">{workspace || '-'}</span>
    </button>
    <button class="label" on:click={toggleSessionPicker} title={rootID || 'No session'} aria-label="Select session">
      {rootID || 'no session'}
    </button>
    {#if agentType}<span class="agent">{agentType}</span>{/if}
    <span class="spacer"></span>
    {#if ownWarnings.length > 0 || globalWarnings.length > 0}
      <button class="warn" on:click={() => (showWarnings = true)} aria-label="Show warnings"
        title="{ownWarnings.length} session warning{ownWarnings.length === 1 ? '' : 's'}, {globalWarnings.length} global">
        <span>⚠ {ownWarnings.length + globalWarnings.length}</span>
      </button>
    {/if}
    <button class="bg-btn" on:click={() => void openBackgroundPanel()} aria-label="Background sessions">Background</button>
    <span class="tokens" title="Exact token usage">⚡ {usage.cache} ↑ {usage.input} ↓ {usage.output}</span>
    <span class="context" title="Context used">{usage.used} / {usage.window}</span>
  </div>
  <div class="content">
    <Conversation
      items={viewItems}
      activity={rootActivityState}
      activeOperation={activeOperation}
      busy={viewBusy || (activeRunning && !rootActivityState)}
      pending={pending}
      error={viewError || ''}
      olderAvailable={viewHasOlder}
      viewKey={rootID + '@' + generation}
      on:older={() => rootReader?.requestOlder()} />
    {#if childID && childReader}
      <div class="child-pane" bind:this={childPaneEl} role="region" aria-label="Child session inspector">
        <div class="child-hdr">
          <span class="child-title">Child {childID}</span>
          <button class="child-close" on:click={closeChild} aria-label="Close child inspector">×</button>
        </div>
        <div class="child-body">
          <Conversation
            items={childItems}
            activity={childActivityState}
            activeOperation={childHydration && childHydration.active_operation ? childHydration.active_operation : null}
            busy={false}
            pending={childHydration ? childHydration.pending : { steering: [], queued: [] }}
            error={childError || ''}
            olderAvailable={childHasOlder}
            viewKey={childID + '@' + generation}
            on:older={() => childReader?.requestOlder()} />
        </div>
      </div>
    {/if}
  </div>
  {#if openError || outcomeNote || repairNote}
    <!-- Status/outcome/repair notes live in NORMAL flow above the Composer so a
         note never overlays or intercepts a control at any width. -->
    <div class="status-region">
      {#if openError}<div class="note error">{openError}</div>{/if}
      {#if outcomeNote}<div class="note outcome">{outcomeNote}</div>{/if}
      {#if repairNote}<div class="note warn">{repairNote}</div>{/if}
    </div>
  {/if}
  <Composer
    bind:this={composer}
    bind:text={draft}
    active={activeRunning}
    hasSession={!!rootID && !!rootReader}
    attempts={outstandingAttempts}
    on:send={(e) => submit(e.detail)}
    on:interrupt={() => void interruptNow()}
    on:stop={() => void stopNow()}
    on:compact={compactNow}
    on:retry={(e) => retryAttempt(e.detail)}
    on:abandon={(e) => abandonAttempt(e.detail)} />
  {#if showSessionPicker}
    <SessionPicker roots={pickerRoots} current={rootID} {workspace}
      on:select={(e) => { showSessionPicker = false; selectRoot(e.detail); }}
      on:close={() => (showSessionPicker = false)} />
  {/if}
  {#if showModelDialog}
    <ModelDialog {agentType} selectedModel={selectedModel} {models} loading={modelLoading} error={modelError}
      on:select={(e) => void pickModel(e.detail)}
      on:close={() => (showModelDialog = false)} />
  {/if}
  {#if showBackgroundPanel}
    <BackgroundPanel children={backgroundChildren} jobs={backgroundJobs} currentChild={childID}
      on:open={(e) => { showBackgroundPanel = false; openChild(e.detail); }}
      on:close={() => (showBackgroundPanel = false)} />
  {/if}
  {#if showWarnings}
    <dialog class="modal" bind:this={warningsDialog} use:openDialog
      on:click={(e) => { if (e.target === warningsDialog) warningsDialog.close(); }}
      on:close={() => (showWarnings = false)}
      aria-labelledby="warnings-title">
      <div class="panel">
        <div id="warnings-title" class="hdr"><span>Warnings</span>
          <button class="panel-close" on:click={() => warningsDialog.close()} aria-label="Close warnings">×</button>
        </div>
        <div class="list">
          {#if globalWarnings.length > 0}
            <div class="group-label">Global</div>
            {#each globalWarnings as warning}
              <div class="warning global"><span class="kind">{warning.source} · {warning.kind}</span>{warning.message}</div>
            {/each}
          {/if}
          {#if ownWarnings.length > 0}
            <div class="group-label">This session</div>
            {#each ownWarnings as warning}
              <div class="warning own"><span class="kind">{warning.source} · {warning.kind}</span>{warning.message}</div>
            {/each}
          {/if}
          {#if ownWarnings.length === 0 && globalWarnings.length === 0}
            <div class="empty">(no warnings)</div>
          {/if}
        </div>
      </div>
    </dialog>
  {/if}
</main>

<style>
  .app { height:100vh; display:flex; flex-direction:column; overflow:hidden; }
  .toolbar { display:flex; align-items:center; gap:10px; flex-wrap:wrap; padding:6px 12px; background:var(--bg-elevated); border-bottom:1px solid var(--border); min-height:36px; font-family:var(--font-ui); font-size:12px; }
  .label { background:none; border:none; border-right:1px solid var(--border); color:var(--text-dim); font-family:var(--font-ui); font-size:12px; cursor:pointer; padding:0 10px; min-width:0; flex:0 1 auto; overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
  .label:hover { color:var(--text); }
  .project { color:var(--text); }
  .agent { color:var(--text-dim); font-family:var(--font-mono); flex:0 1 auto; min-width:0; overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
  .spacer { flex:1 1 40px; }
  .warn { background:none; border:none; color:var(--warn); font-family:var(--font-ui); font-size:12px; cursor:pointer; white-space:nowrap; flex-shrink:0; }
  .bg-btn { background:none; border:1px solid var(--border-button); border-radius:var(--radius); color:var(--text-dim); font-family:var(--font-ui); font-size:12px; cursor:pointer; padding:3px 10px; white-space:nowrap; flex-shrink:0; }
  .bg-btn:hover { color:var(--accent); border-color:var(--accent); }
  .tokens, .context { color:var(--text-dim); font-family:var(--font-mono); font-size:11px; white-space:nowrap; flex-shrink:0; }
  .content { flex:1; display:flex; flex-direction:row; overflow:hidden; position:relative; min-height:0; }
  .content > :global(.message-list) { flex:1; }
  .child-pane { flex:0 0 42%; display:flex; flex-direction:column; min-width:0; border-left:1px solid var(--border-strong); background:var(--bg-elevated); }
  .child-hdr { display:flex; align-items:center; justify-content:space-between; padding:6px 12px; border-bottom:1px solid var(--border); }
  .child-title { color:var(--text-dim); font-family:var(--font-mono); font-size:12px; }
  .child-close { background:none; border:none; color:var(--text-dim); font-size:16px; cursor:pointer; padding:0 4px; }
  .child-close:hover { color:var(--accent); }
  .child-body { flex:1; display:flex; min-height:0; overflow:hidden; }
  .child-body > :global(.message-list) { flex:1; }
  .status-region { display:flex; flex-direction:column; gap:4px; padding:0 12px; }
  .note { padding:6px 12px; border-radius:var(--radius); font-family:var(--font-ui); font-size:12px; overflow-wrap:anywhere; }
  .note.error { background:var(--error-soft); border:1px solid var(--error); color:var(--error); }
  .note.outcome { background:var(--bg-elevated); border:1px solid var(--border-strong); color:var(--text); }
  .note.warn { background:var(--accent-soft); border:1px solid var(--warn); color:var(--warn); }
  .modal { position:fixed; inset:0; width:100vw; height:100vh; max-width:none; max-height:none; margin:0; padding:0; border:0; background:transparent; }
  .modal::backdrop { background:transparent; }
  .panel { position:absolute; top:40px; right:12px; background:var(--bg-elevated); border:1px solid var(--border-strong); min-width:320px; max-width:480px; max-height:400px; display:flex; flex-direction:column; box-shadow:var(--shadow-menu); }
  .hdr { display:flex; justify-content:space-between; align-items:center; padding:6px 12px; font-size:12px; color:var(--text-dim); text-transform:uppercase; letter-spacing:.5px; border-bottom:1px solid var(--border); }
  .panel-close { background:none; border:none; color:var(--text-dim); font-size:16px; cursor:pointer; padding:0 4px; }
  .panel-close:hover { color:var(--accent); }
  .list { flex:1; overflow:auto; padding:4px 0; }
  .group-label { padding:6px 12px 2px; font-size:11px; color:var(--text-dim); text-transform:uppercase; letter-spacing:.5px; }
  .warning { margin:2px 12px; padding:6px 10px; border-radius:var(--radius); font-size:12px; font-family:var(--font-ui); white-space:pre-wrap; overflow-wrap:anywhere; }
  .warning.global { background:var(--accent-soft); color:var(--text); }
  .warning.own { background:var(--error-soft); color:var(--error); }
  .kind { display:block; color:var(--text-dim); font-family:var(--font-mono); font-size:11px; }
  .empty { padding:8px 12px; color:var(--text-dim); font-family:var(--font-ui); font-size:12px; }
</style>
