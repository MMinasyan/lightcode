<script>
  import { onDestroy, onMount } from 'svelte';
  import { get, writable } from 'svelte/store';
  import { PickDirectory, SetTitle } from '../../client-bindings/wailsjs/go/desktopclient/Host';
  import { errorText } from '../lib/errors.js';
  import { settings } from '../lib/settings.js';
  import { ProvisionalActivity } from './activity';
  import { AttemptLedger } from './attempts';
  import { invokeNative } from './results';
  import { classifyRestore, restoreLines } from './restore';
  import { composeForkBody, composeObjectPatch, parseExactJSON, presentNumber } from './exactjson';
  import { SessionReader, okRead, failRead, sameStamp } from './reader';
  import { sortOpenRoots } from './sessions';
  import { compareConfigurationRevision, compareWarningsRevision } from './projection';
  import Conversation from './Conversation.svelte';
  import Composer from './Composer.svelte';
  import SessionPicker from './SessionPicker.svelte';
  import ModelDialog from './ModelDialog.svelte';
  import BackgroundPanel from './BackgroundPanel.svelte';
  import WorkspacePicker from './WorkspacePicker.svelte';
  import AgentPicker from './AgentPicker.svelte';
  import SettingsEditor from './SettingsEditor.svelte';
  import ProviderEditor from './ProviderEditor.svelte';
  import FileViewer from './FileViewer.svelte';
  import SnapshotsDialog from './SnapshotsDialog.svelte';
  import UsageDialog from './UsageDialog.svelte';
  import ForkDialog from './ForkDialog.svelte';
  import { openDialog } from './dialogs';

  export let connection = null;

  // Root and inspector are independent authorities; every completion captures its stamp before the call.
  let instance = '';
  let generation = '';
  let workspace = '';
  let rootSelection = 0;
  let childSelection = 0;

  let rootID = '';
  let childID = '';
  let rootReader = null;
  let childReader = null;
  const rootActivity = new ProvisionalActivity();
  const childActivity = new ProvisionalActivity();
  const attempts = new AttemptLedger();

  // Accepted global snapshots: each pairs the accepted revision with its payload.
  let configuration = null;
  let warnings = null;

  // Every edit advances the draft revision; only the exact submitted revision is cleared.
  let draft = '';
  let draftRevision = 0;
  let draftShadow = '';

  let openError = '';
  let outcomeNote = '';
  let repairNote = '';
  let repairTask = null;
  let disposed = false;
  // ONE active first-party modal as a reactive scalar; named-close handlers
  // compare through a synchronous get.
  const modal = writable('');
  let modelLoading = false;
  let modelError = '';
  let models = [];
  // Closing the model dialog invalidates its outstanding read.
  let modelToken = 0;
  let backgroundChildren = [];
  let backgroundJobs = [];
  let composer;
  let childPaneEl;
  let warningsDialog;

  // Navigation/editor/viewer state; every async read carries its token plus the connection/selection stamp.
  let workspaces = [];
  let pickerOpen = [];
  let pickerArchived = [];
  let pickerLoading = false;
  let listToken = 0;
  let forkItem = null;
  let forkSourceSession = '';
  let forkSourceStamp = null;
  let forkText = '';
  let forkShadow = '';
  let forkRevision = 0;
  let forkRewind = false;
  let forkAttempt = null;
  let forkNote = '';
  let forkBusy = false;

  // ONE addressed outcome rule: rejection/unknown is reported against the captured target.
  function reportAddressed(classified, label, target) {
    if (classified.kind === 'rejection') {
      outcomeNote = `${label} failed for ${target}: ${classified.message}`;
      return false;
    }
    if (classified.kind !== 'ok') {
      outcomeNote = `${label} outcome unknown for ${target}; nothing was retransmitted.`;
      return false;
    }
    return true;
  }

  function createUnknownNote(addressed) {
    return 'Session create outcome unknown for workspace ' + addressed + '; not retransmitted.';
  }

  function openModal(name) {
    modal.set(name);
  }

  function closeModal(name) {
    if (get(modal) === name) {
      modal.set('');
    }
  }

  // Two derived render records over the actual subscribed owners: each pane
  // depends on its own reader/activity references plus the shared attempt
  // owner and the authoritative warnings it reads.
  $: root = viewOf($rootReader, $rootActivity, rootID, $attempts, warnings);
  $: child = viewOf($childReader, $childActivity, childID, $attempts, warnings);

  function viewOf(reader, activity, sessionID, ledger, globalWarnings) {
    const hydration = reader ? reader.getHydration() : null;
    const operation = hydration && hydration.active_operation ? hydration.active_operation : null;
    return {
      reader,
      hydration,
      operation,
      running: operation !== null && operation.status === 'running',
      items: reader ? reader.getItems() : [],
      error: reader ? reader.getError() : null,
      busy: reader ? reader.isBusy() : false,
      older: reader ? reader.hasOlder() : false,
      pending: hydration ? hydration.pending : { steering: [], queued: [] },
      usage: usageTotals(hydration),
      // With no hydration the global indicator shows the accepted global snapshot.
      ownWarnings: hydration ? hydration.warnings.filter((warning) => warning.session_id === sessionID) : [],
      globalWarnings: hydration ? hydration.warnings.filter((warning) => !warning.session_id) : (globalWarnings ? globalWarnings.payload : []),
      model: hydration && hydration.selected_model ? hydration.selected_model : '',
      agentType: hydration ? hydration.session.agent_type : '',
      activity: activity.getState(),
      attempts: ledger.outstanding(sessionID),
    };
  }

  $: if (draft !== draftShadow) {
    draftRevision += 1;
    draftShadow = draft;
  }

  // The Fork replacement text has its own revision; only the exact submitted revision is cleared.
  $: if (forkText !== forkShadow) {
    forkRevision += 1;
    forkShadow = forkText;
  }

  // Usage totals stay exact decimal strings summed through BigInt.
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
      window: presentNumber(hydration.usage.context.context_window),
    };
  }

  function clientNow() {
    return connection.getClient();
  }

  function stampFor(sessionID, view) {
    const selection = view === 'child' ? childSelection : rootSelection;
    return () => ({ instance, generation, selection, sessionID });
  }

  // One invocation boundary; this binding only supplies the App's client accessor.
  const invoke = (label, operation, params, decode) => invokeNative(clientNow, label, operation, params, decode);

  // A received non-2xx/absent response is an ordinary read failure; an undecodable success is invalid delivery.
  function readOutcome(classified) {
    if (classified.kind === 'ok') return okRead(classified.data);
    if (classified.kind === 'invalid') return failRead('invalid', classified.message);
    return failRead('ordinary', classified.message);
  }

  function readerRepair(reason) {
    repairNote = reason;
    void requestRepair();
  }

  function makeReader(sessionID, view) {
    const reader = new SessionReader(
      {
        async hydration() {
          return readOutcome(await invoke('hydration', 'GetSessionHydration', JSON.stringify({ session_id: sessionID }), parseExactJSON));
        },
        async history(_stamp, cursor) {
          return readOutcome(await invoke('history', 'GetSessionHistory', JSON.stringify(cursor ? { session_id: sessionID, query: { cursor } } : { session_id: sessionID }), parseExactJSON));
        },
      },
      stampFor(sessionID, view),
      readerRepair,
      (hydration) => adoptHydration(sessionID, hydration),
    );
    reader.setFloors(configuration ? configuration.revision : null, warnings ? warnings.revision : null);
    return reader;
  }

  // An accepted hydration replaces provisional activity and drives draft clearing.
  function adoptHydration(sessionID, hydration) {
    if (sessionID === rootID) rootActivity.clear();
    if (sessionID === childID) childActivity.clear();
    for (const attempt of attempts.adoptHydration(sessionID, hydration)) {
      if (attempt.kind === 'submit' && attempt.draftRevision === draftRevision) {
        draft = '';
      }
      // A Fork accepted by caller ID clears the dialog draft only at its submitted revision.
      if (attempt.kind === 'fork' && forkAttempt && forkAttempt.id === attempt.id) {
        if (forkRevision === attempt.draftRevision) forkText = '';
        forkAttempt = null;
      }
    }
  }

  // One repair owner; a current-generation loss during it coalesces one more serialized reopen.
  let repairPending = false;

  function requestRepair() {
    if (disposed) return undefined;
    if (repairTask !== null) {
      // Coalesce a current-generation loss into the running owner.
      repairPending = true;
      return repairTask;
    }
    repairTask = runRepair().finally(() => { repairTask = null; });
    return repairTask;
  }

  async function runRepair() {
    do {
      repairPending = false;
      await performRepair();
    } while (!disposed && repairPending);
    repairPending = false;
  }

  // Repair start discards every runtime-derived dialog and payload; the draft and attempt ledger survive.
  function discardRuntimeProjection() {
    modal.set('');
    modelToken += 1;
    forkItem = null;
    forkAttempt = null;
    forkNote = '';
    listToken += 1;
    pickerOpen = [];
    pickerArchived = [];
    pickerLoading = false;
    workspaces = [];
    models = [];
    modelLoading = false;
    modelError = '';
    backgroundChildren = [];
    backgroundJobs = [];
    // Repair clears both snapshots once; its own fresh reads replace them.
    configuration = null;
    warnings = null;
  }

  async function performRepair() {
    if (disposed) return;
    discardRuntimeProjection();
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
      // A repair preserves the selected lexical Workspace; native cwd applies at boot only.
      applyTitle(workspace);
      const tasks = [refreshGlobalFloors()];
      if (rootID) {
        rootReader = makeReader(rootID, 'root');
        rootReader.markDirty();
        tasks.push(rootReader.currentChain());
      }
      if (childID) {
        childReader = makeReader(childID, 'child');
        childReader.markDirty();
        tasks.push(childReader.currentChain());
      }
      await Promise.all(tasks);
    } catch (error) {
      // The repair's own read failure never recursively reopens.
      openError = 'Runtime reconnect failed: ' + errorText(error);
    } finally {
    }
  }

  // --- Accepted global snapshots --------------------------------------------

  function pushFloors() {
    const configurationFloor = configuration ? configuration.revision : null;
    const warningsFloor = warnings ? warnings.revision : null;
    if (rootReader) rootReader.setFloors(configurationFloor, warningsFloor);
    if (childReader) childReader.setFloors(configurationFloor, warningsFloor);
  }

  async function refreshGlobalFloors() {
    await Promise.all([refreshGlobal('configuration'), refreshGlobal('warnings')]);
  }

  // ONE global read/adoption path for the two actual generated kinds. Validity
  // is the installed connection object that served the read plus its own
  // revision comparison: never a cached App field and never the local Session
  // selection, so a navigation cannot drop a completed global payload.
  async function refreshGlobal(kind) {
    const info = connection.getInfo();
    const classified = kind === 'configuration'
      ? await invoke('configuration', 'GetConfiguration', '{}', JSON.parse)
      : await invoke('warnings', 'GetWarnings', '{}', JSON.parse);
    // A read started with no installed connection, or whose installed
    // connection was retired or superseded meanwhile, is dropped whole.
    if (info === null || connection.getInfo() !== info) return;
    if (classified.kind !== 'ok') {
      if (classified.kind === 'invalid') { repairNote = classified.message; void requestRepair(); }
      else outcomeNote = classified.message;
      return;
    }
    if (kind === 'configuration') {
      acceptGlobal('configuration', classified.data.configuration_revision, classified.data.agents, info);
    } else {
      acceptGlobal('warnings', classified.data.warnings_revision, classified.data.warnings.filter((warning) => !warning.session_id), info);
    }
  }

  // Malformed/foreign repairs; older body rejected whole; equal adopts; newer advances.
  // The initial document is validated against its serving instance too.
  function acceptGlobal(kind, revision, payload, info) {
    const accepted = kind === 'configuration' ? configuration : warnings;
    const floor = accepted !== null
      ? accepted.revision
      : (kind === 'configuration' ? { instance_id: info.instance_id, generation: '0' } : { instance_id: info.instance_id, revision: '0' });
    let comparison = null;
    try {
      comparison = kind === 'configuration'
        ? compareConfigurationRevision(floor, revision)
        : compareWarningsRevision(floor, revision);
    } catch {
      repairNote = `${kind} revision is malformed or foreign`;
      void requestRepair();
      return;
    }
    if (accepted !== null && comparison === 1) {
      pushFloors();
      return;
    }
    if (kind === 'configuration') configuration = { revision, payload };
    else warnings = { revision, payload };
    pushFloors();
    if (comparison !== 0) {
      if (rootReader) rootReader.markDirty();
      if (childReader) childReader.markDirty();
    }
  }

  // --- Composition and selection -------------------------------------------

  // ONE serialized initial-selection owner; a superseded completion re-runs it.
  let selectionTask = null;
  // A superseded completion re-runs inside the same serialized owner.
  let selectionPending = false;

  function ensureSelection() {
    if (selectionTask !== null) {
      selectionPending = true;
      return selectionTask;
    }
    selectionTask = (async () => {
      do {
        selectionPending = false;
        await runSelection();
      } while (!disposed && selectionPending);
      selectionPending = false;
      selectionTask = null;
    })();
    return selectionTask;
  }

  // ONE Create task for a captured Workspace address; callers keep their own adoption.
  async function createSessionIn(addressed) {
    return invoke('session create', 'CreateSession', JSON.stringify({
      body: { workspace: addressed, agent_type: 'primary' },
    }), JSON.parse);
  }

  async function runSelection() {
    const cap = stampFor('', 'root')();
    // Capture the addressed Workspace before the first await.
    const addressed = workspace;
    const classified = await invoke('session list', 'ListSessions', JSON.stringify({ query: { workspace: addressed, lifecycle: 'open' } }), JSON.parse);
    if (classified.kind !== 'ok') {
      if (!sameStamp(cap, stampFor('', 'root')())) return;
      openError = classified.message;
      return;
    }
    if (!sameStamp(cap, stampFor('', 'root')())) return;
    const roots = sortOpenRoots(classified.data);
    if (roots.length > 0) {
      outcomeNote = '';
      resetRoot(roots[0].session_id);
      return;
    }
    const created = await createSessionIn(addressed);
    // Report the result against its captured address; only adoption is stamped.
    if (created.kind === 'rejection') {
      if (!sameStamp(cap, stampFor('', 'root')())) return;
      openError = 'Session creation failed: ' + created.message;
      return;
    }
    if (created.kind !== 'ok') {
      if (!sameStamp(cap, stampFor('', 'root')())) return;
      outcomeNote = createUnknownNote(addressed);
      return;
    }
    if (!sameStamp(cap, stampFor('', 'root')())) return;
    resetRoot(created.data.session_id);
  }

  // ONE selected-root reset; adoption, Workspace change and removal reuse it.
  function resetRoot(id) {
    rootSelection += 1;
    childSelection += 1;
    rootID = id;
    childID = '';
    if (rootReader) rootReader.dispose();
    if (childReader) childReader.dispose();
    rootReader = null;
    childReader = null;
    rootActivity.clear();
    childActivity.clear();
    if (id === '') return;
    rootReader = makeReader(id, 'root');
    rootReader.markDirty();
  }

  // The inspector resets independently and never invalidates the root generation.
  function resetChild(id) {
    childSelection += 1;
    childID = id;
    if (childReader) childReader.dispose();
    childReader = null;
    childActivity.clear();
    if (id === '') return;
    childReader = makeReader(id, 'child');
    childReader.markDirty();
  }

  function openChild(id) {
    if (id === childID) return;
    resetChild(id);
  }

  function closeChild() {
    if (!childID) return;
    resetChild('');
  }

  // --- Event dispatch -------------------------------------------------------

  function handleEvent(event) {
    const kind = event.kind;
    if (kind === 'session_changed' || kind === 'configuration_changed' || kind === 'warning_changed') {
      if (kind === 'configuration_changed') {
        void refreshGlobal('configuration');
      } else if (kind === 'warning_changed') {
        void refreshGlobal('warnings');
      }
      for (const [reader, sessionID] of [[rootReader, rootID], [childReader, childID]]) {
        if (!reader) continue;
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

  // The outcome always lands on the captured address; Submit and Compact share this path.
  async function settleAttempt(attempt) {
    const cap = stampFor(attempt.sessionID, 'root')();
    const classified = attempt.kind === 'compact'
      ? await invoke('compact', 'CompactSession', JSON.stringify({
        session_id: attempt.sessionID,
        body: { operation_id: attempt.id },
      }), JSON.parse)
      : await invoke('submit', 'SubmitSession', JSON.stringify({
        session_id: attempt.sessionID,
        body: { operation_id: attempt.id, mode: attempt.mode, content: attempt.content },
      }), JSON.parse);
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

  function clearMatchingDraft(attempt) {
    if (attempt.kind !== 'submit') return;
    if (attempt.draftRevision === draftRevision) draft = '';
  }

  function retryAttempt(attempt) {
    if (attempt.kind === 'fork') {
      // No Retry while a rewind outcome is unknown; a retry repeats no handled stage.
      if (attempt.restore?.kind === 'unknown') return;
      attempts.markRetrying(attempt.id);
      void settleForkAttempt(attempt, stampFor(attempt.sessionID, 'root')(), attempt.rewind);
      return;
    }
    attempts.markRetrying(attempt.id);
    void settleAttempt(attempt);
  }

  function abandonAttempt(attempt) {
    attempts.remove(attempt.id);
    if (forkAttempt && forkAttempt.id === attempt.id) forkAttempt = null;
  }

  function compactNow() {
    if (!rootID) return;
    const attempt = attempts.create('compact', 'regular', [], rootID, draftRevision);
    void settleAttempt(attempt);
  }

  async function addressControl(operation, label, sessionID) {
    const cap = stampFor(sessionID, 'root')();
    const classified = await invoke(label, operation, JSON.stringify({ session_id: sessionID }), JSON.parse);
    if (!reportAddressed(classified, label, `session ${sessionID}`)) {
      return;
    }
    if (!sameStamp(cap, stampFor(sessionID, 'root')())) return;
    if (rootReader) rootReader.markDirty();
  }

  function interruptNow() {
    if (!rootID) return;
    void addressControl('InterruptSession', 'Interrupt', rootID);
  }

  function stopNow() {
    if (!rootID) return;
    void addressControl('StopSession', 'Stop', rootID);
  }

  // --- Replacement Fork and ordered code rewind -------------------------------

  // Replacement Fork opens for one user-origin input; rewind exists only for an owned operation_id.
  function openFork(item) {
    forkItem = item;
    // Capture the initiating source identity and stamp with the form.
    forkSourceSession = rootID;
    forkSourceStamp = stampFor(rootID, 'root')();
    forkText = JSON.stringify(item.content || [], null, 2);
    forkShadow = forkText;
    forkRevision = 0;
    forkRewind = false;
    forkAttempt = null;
    forkNote = '';
    openModal('fork');
  }

  function submitFork() {
    if (!forkItem || !forkSourceSession || !forkSourceStamp || forkBusy) return;
    const rewind = forkRewind && !!forkItem.operation_id;
    const attempt = attempts.createFork(
      forkSourceSession,
      forkItem.item_id,
      forkItem.operation_id || '',
      forkText,
      rewind,
      forkRevision,
    );
    forkAttempt = attempt;
    forkNote = '';
    void settleForkAttempt(attempt, forkSourceStamp, rewind);
  }

  // One ordered sequence per Fork attempt: optional rewind first, then the replacement.
  async function settleForkAttempt(attempt, cap, rewind) {
    if (attempt.restore?.kind === 'unknown') return;
    forkBusy = true;
    try {
      if (rewind && attempt.restore === null) {
        const classified = await invoke('code rewind', 'RevertSessionCode', JSON.stringify({
          session_id: attempt.sessionID,
          body: { boundary_operation_id: attempt.boundaryOperationID },
        }), JSON.parse);
        const outcome = classifyRestore(classified, 'code rewind');
        attempts.markRestore(attempt.id, outcome);
        forkAttempt = attempts.get(attempt.id);
        if (outcome.kind === 'unknown') {
          forkNote = outcome.message;
              return;
        }
      }
      const bodyText = composeForkBody(attempt.boundaryItemID, attempt.id, attempt.replacementText);
      const classified = await invoke('fork', 'ForkSession', composeObjectPatch([
        ['session_id', JSON.stringify(attempt.sessionID)],
        ['body', bodyText],
      ]), JSON.parse);
      if (classified.kind === 'rejection') {
        attempts.markRejected(attempt.id, classified.message);
        forkAttempt = attempts.get(attempt.id);
          return;
      }
      if (classified.kind !== 'ok') {
        attempts.markUnknown(attempt.id);
        forkAttempt = attempts.get(attempt.id);
          return;
      }
      // Publish the ledger-held rewind outcome with the addressed Fork acceptance.
      const handled = attempts.get(attempt.id);
      const restoreNote = handled && handled.restore ? restoreLines(handled.restore).join('; ') : '';
      attempts.remove(attempt.id);
      if (forkAttempt && forkAttempt.id === attempt.id) {
        if (forkRevision === attempt.draftRevision) forkText = '';
        forkAttempt = null;
      }
      const destination = classified.data.session.session_id;
      let forkOutcome;
      if (sameStamp(cap, stampFor(attempt.sessionID, 'root')()) && rootID === attempt.sessionID) {
        closeModal('fork');
        forkItem = null;
        forkOutcome = `Fork admitted to session ${destination}.`;
        resetRoot(destination);
      } else {
        forkOutcome = `Fork admitted to session ${destination}; the selection moved, so it was not adopted.`;
      }
      outcomeNote = restoreNote === '' ? forkOutcome : `${restoreNote}; ${forkOutcome}`;
    } finally {
      forkBusy = false;
    }
  }

  // --- Navigation and pickers -------------------------------------------------

  // ListWorkspaces choices and native directory picks both re-run initial composition.
  async function openWorkspacePicker() {
    const cap = stampFor(rootID, 'root')();
    const classified = await invoke('workspace list', 'ListWorkspaces', '{}', JSON.parse);
    if (!sameStamp(cap, stampFor(rootID, 'root')())) return;
    if (classified.kind !== 'ok') {
      outcomeNote = classified.message;
      return;
    }
    workspaces = classified.data;
    openModal('workspace');
  }

  async function pickWorkspaceDirectory() {
    // A superseded native pick composes and reports nothing.
    const cap = stampFor(rootID, 'root')();
    let directory = '';
    try {
      directory = await PickDirectory();
    } catch (error) {
      if (!sameStamp(cap, stampFor(rootID, 'root')())) return;
      outcomeNote = 'Directory pick failed: ' + errorText(error);
      return;
    }
    if (!directory) return;
    if (!sameStamp(cap, stampFor(rootID, 'root')())) return;
    closeModal('workspace');
    await chooseWorkspace(directory);
  }

  // A Workspace change resets the selected view, then composes initial selection afresh.
  async function chooseWorkspace(path) {
    if (path === workspace) return;
    resetRoot('');
    workspace = path;
    closeModal('session');
    closeModal('agent');
    pickerOpen = [];
    pickerArchived = [];
    listToken += 1;
    applyTitle(path);
    await ensureSelection();
  }

  // SetTitle is host-local presentation; failures are not retried.
  function applyTitle(title) {
    void Promise.resolve().then(() => SetTitle(title)).catch(() => {});
  }

  // Both lifecycle lists are read through this ONE owner: the addressed
  // Workspace, the combined non-ok error and the two actual list calls.
  async function readLifecycleLists() {
    const [openList, archivedList] = await Promise.all([
      invoke('session list', 'ListSessions', JSON.stringify({ query: { workspace, lifecycle: 'open' } }), JSON.parse),
      invoke('session list', 'ListSessions', JSON.stringify({ query: { workspace, lifecycle: 'archived' } }), JSON.parse),
    ]);
    if (openList.kind !== 'ok' || archivedList.kind !== 'ok') {
      return { error: (openList.kind !== 'ok' ? openList : archivedList).message };
    }
    return { open: openList.data, archived: archivedList.data };
  }

  // One list token invalidates older in-flight lists after every request/deletion.
  async function refreshPicker() {
    const token = ++listToken;
    const cap = stampFor(rootID, 'root')();
    pickerLoading = true;
    try {
      const lists = await readLifecycleLists();
      if (token !== listToken || !sameStamp(cap, stampFor(rootID, 'root')())) return;
      if (lists.error !== undefined) {
        outcomeNote = lists.error;
        return;
      }
      const row = (session) => ({ id: session.session_id, agentType: session.agent_type, lastActivity: session.last_activity });
      pickerOpen = sortOpenRoots(lists.open).map(row);
      pickerArchived = sortOpenRoots(lists.archived).map(row);
    } finally {
      if (token === listToken) pickerLoading = false;
    }
  }

  function toggleSessionPicker() {
    if (get(modal) === 'session') {
      closeModal('session');
      return;
    }
    openModal('session');
    void refreshPicker();
  }

  function pickerSelect(id) {
    closeModal('session');
    resetRoot(id);
  }

  // New uses the ordinary Create composition; an unknown Create stands as a note.
  async function pickerNew() {
    const cap = stampFor(rootID, 'root')();
    const addressed = workspace;
    const classified = await createSessionIn(addressed);
    if (classified.kind === 'rejection') {
      outcomeNote = 'Session creation failed: ' + classified.message;
      return;
    }
    if (classified.kind !== 'ok') {
      outcomeNote = createUnknownNote(addressed);
      void refreshPicker();
      return;
    }
    if (!sameStamp(cap, stampFor(rootID, 'root')())) {
      void refreshPicker();
      return;
    }
    closeModal('session');
    resetRoot(classified.data.session_id);
  }

  // Resume advances the root generation before capturing its stamp; a failed
  // resume that still addresses the retained root dirties its reader.
  async function pickerResume(id) {
    rootSelection += 1;
    const cap = stampFor(rootID, 'root')();
    const classified = await invoke('session reopen', 'ReopenSession', JSON.stringify({ session_id: id }), JSON.parse);
    if (!reportAddressed(classified, 'Resume', `session ${id}`)) {
      if (sameStamp(cap, stampFor(rootID, 'root')()) && rootReader) rootReader.markDirty();
      return;
    }
    if (!sameStamp(cap, stampFor(rootID, 'root')())) {
      void refreshPicker();
      return;
    }
    closeModal('session');
    resetRoot(id);
  }

  // The one addressed Archive stage shared by standalone archive and the
  // archive-first Delete sequence: a known success on the addressed selected
  // root clears only that matching selection; every outcome is reported.
  async function archiveAddressed(id, cap) {
    const classified = await invoke('session archive', 'ArchiveSession', JSON.stringify({ session_id: id }), JSON.parse);
    if (!reportAddressed(classified, 'Archive', `session ${id}`)) return false;
    if (id === rootID && sameStamp(cap, stampFor(rootID, 'root')())) {
      resetRoot('');
    }
    return true;
  }

  async function pickerArchive(id) {
    await archiveAddressed(id, stampFor(rootID, 'root')());
    await refreshPicker();
  }

  // Archive→Delete: a non-ok Archive stops before Delete; archive clears a matching selection first.
  async function pickerRemove(id) {
    if (!(await archiveAddressed(id, stampFor(rootID, 'root')()))) {
      await refreshPicker();
      return;
    }
    void refreshPicker();
    const removed = await invoke('session delete', 'DeleteSession', JSON.stringify({ session_id: id }), JSON.parse);
    reportAddressed(removed, 'Delete', `session ${id}`);
    await refreshPicker();
  }

  // --- Agent type ------------------------------------------------------------

  // SetSessionAgentType is reported against its captured Session; the stamped refresh is gated.
  async function pickAgentType(type) {
    closeModal('agent');
    const addressed = rootID;
    if (!addressed || !type) return;
    const cap = stampFor(addressed, 'root')();
    const classified = await invoke('agent type', 'SetSessionAgentType', JSON.stringify({
      session_id: addressed,
      body: { agent_type: type },
    }), JSON.parse);
    if (!reportAddressed(classified, 'Agent type change', `session ${addressed}`)) {
      return;
    }
    if (sameStamp(cap, stampFor(addressed, 'root')()) && rootReader) rootReader.markDirty();
  }

  async function openModelDialog() {
    const token = ++modelToken;
    openModal('model');
    modelLoading = true;
    modelError = '';
    const cap = stampFor(rootID, 'root')();
    const classified = await invoke('model list', 'ListModels', JSON.stringify({ query: { all: false } }), JSON.parse);
    try {
      if (token !== modelToken || !sameStamp(cap, stampFor(rootID, 'root')())) return;
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
    } finally {
      if (token === modelToken) modelLoading = false;
    }
  }

  function closeModelDialog() {
    modelToken += 1;
    closeModal('model');
  }

  function toggleModelDialog() {
    if (get(modal) === 'model') {
      closeModelDialog();
    } else {
      void openModelDialog();
    }
  }

  // SetAgentTypeModel is type-wide and reports against the captured agent type.
  async function pickModel(ref) {
    const addressedType = root.agentType;
    if (!addressedType) return;
    const cap = stampFor(rootID, 'root')();
    const classified = await invoke('model selection', 'SetAgentTypeModel', JSON.stringify({
      query: { agent_type: addressedType },
      body: { model: ref },
    }), JSON.parse);
    if (!reportAddressed(classified, 'Model selection', `agent type ${addressedType}`)) {
      modelError = classified.message;
      return;
    }
    outcomeNote = `Model selection applied for agent type ${addressedType}.`;
    if (!sameStamp(cap, stampFor(rootID, 'root')())) {
      void refreshGlobal('configuration');
      return;
    }
    closeModal('model');
    // Refresh configuration through its own read, not the mutation's clock.
    void refreshGlobal('configuration');
  }

  // One ID-keyed union of lineage headers from both lists plus live membership; jobs stay separate.
  async function openBackgroundPanel() {
    const cap = stampFor(rootID, 'root')();
    const lists = await readLifecycleLists();
    if (!sameStamp(cap, stampFor(rootID, 'root')())) return;
    if (lists.error !== undefined) {
      outcomeNote = lists.error;
      return;
    }
    const hydration = rootReader ? rootReader.getHydration() : null;
    const byId = new Map();
    for (const header of [...lists.open, ...lists.archived]) {
      if (header.parent_session_id !== rootID) continue;
      const existing = byId.get(header.session_id);
      if (existing) {
        // Overlapping captions merge into one ID-keyed row.
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
    for (const member of hydration ? hydration.background : []) {
      if (member.kind !== 'child') continue;
      const existing = byId.get(member.id);
      if (existing) {
        existing.live = true;
      } else {
        byId.set(member.id, { id: member.id, agentType: '', lifecycle: 'live', lastActivity: '', live: true, unknownHeader: true });
      }
    }
    backgroundChildren = [...byId.values()];
    backgroundJobs = (hydration ? hydration.background : [])
      .filter((member) => member.kind === 'job')
      .map((member) => ({ id: member.id }));
    openModal('background');
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
      // A modal closes first; then Escape interrupts; the child pane closes only on inside focus.
      const openName = get(modal);
      if (openName !== '') { closeModal(openName); return; }
      if (document.activeElement?.tagName === 'TEXTAREA') { e.preventDefault(); void interruptNow(); return; }
      if (childID && childPaneEl && childPaneEl.contains(document.activeElement)) closeChild();
    }
  }

  // --- Lifecycle ---------------------------------------------------------------

  onMount(async () => {
    connection.setEventHandler(handleEvent);
    connection.setFailureHandler((error, lostGeneration) => {
      // A loss superseded by a newer Open is subsumed; the repair owner coalesces the current one.
      if (connection.isCurrentGeneration && !connection.isCurrentGeneration(lostGeneration)) {
        return;
      }
      repairNote = 'Event stream lost: ' + errorText(error);
      void requestRepair();
    });
    try {
      const info = await connection.open();
      if (disposed) return;
      instance = info.instance_id;
      generation = info.generation;
      workspace = info.workspace;
      applyTitle(workspace);
    } catch (error) {
      openError = 'Runtime connection failed: ' + errorText(error);
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
    <button class="label" on:click={() => void openWorkspacePicker()} title={workspace || 'workspace'} aria-label="Select workspace">
      <span class="project">{workspace || '-'}</span>
    </button>
    <button class="label" on:click={toggleSessionPicker} title={rootID || 'No session'} aria-label="Select session">
      {rootID || 'no session'}
    </button>
    {#if root.agentType}<span class="agent">{root.agentType}</span>{/if}
    <span class="spacer"></span>
    {#if root.ownWarnings.length > 0 || root.globalWarnings.length > 0}
      <button class="warn" on:click={() => openModal('warnings')} aria-label="Show warnings"
        title="{root.ownWarnings.length} session warning{root.ownWarnings.length === 1 ? '' : 's'}, {root.globalWarnings.length} global">
        <span>⚠ {root.ownWarnings.length + root.globalWarnings.length}</span>
      </button>
    {/if}
    <button class="bg-btn" on:click={() => void openBackgroundPanel()} aria-label="Background sessions">Background</button>
    <button class="tokens" on:click={() => openModal('usage')} title="Exact token usage" aria-label="Show token usage">⚡ {root.usage.cache} ↑ {root.usage.input} ↓ {root.usage.output}</button>
    <span class="context" title="Context used">{root.usage.used} / {root.usage.window}</span>
    <button class="settings-btn" on:click={() => openModal('settings')} title="Settings" aria-label="Settings">
      <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="3"/><path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 0 1 0 2.83 2 2 0 0 1-2.83 0l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 0 1-2 2 2 2 0 0 1-2-2v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 0 1-2.83 0 2 2 0 0 1 0-2.83l.06-.06a1.65 1.65 0 0 0 .33-1.82 1.65 1.65 0 0 0-1.51-1H3a2 2 0 0 1-2-2 2 2 0 0 1 2-2h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 0 1 0-2.83 2 2 0 0 1 2.83 0l.06.06a1.65 1.65 0 0 0 1.82.33H9a1.65 1.65 0 0 0 1-1.51V3a2 2 0 0 1 2-2 2 2 0 0 1 2 2v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 0 1 2.83 0 2 2 0 0 1 0 2.83l-.06.06a1.65 1.65 0 0 0-.33 1.82V9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 0 1 2 2 2 2 0 0 1-2 2h-.09a1.65 1.65 0 0 0-1.51 1z"/></svg>
    </button>
  </div>
  <div class="content">
    <Conversation
      items={root.items}
      activity={root.activity}
      activeOperation={root.operation}
      busy={root.busy || (root.running && !root.activity)}
      pending={root.pending}
      error={root.error || ''}
      olderAvailable={root.older}
      viewKey={rootID + '@' + generation}
      forksEnabled={true}
      on:fork={(e) => openFork(e.detail)}
      on:older={() => rootReader?.requestOlder()} />
    {#if childID && childReader}
      <div class="child-pane" bind:this={childPaneEl} role="region" aria-label="Child session inspector">
        <div class="child-hdr">
          <span class="child-title">Child {childID}</span>
          <button class="child-close" on:click={closeChild} aria-label="Close child inspector">×</button>
        </div>
        <div class="child-body">
          <Conversation
            items={child.items}
            activity={child.activity}
            activeOperation={child.operation}
            busy={false}
            pending={child.pending}
            error={child.error || ''}
            olderAvailable={child.older}
            viewKey={childID + '@' + generation}
            on:older={() => childReader?.requestOlder()} />
        </div>
      </div>
    {/if}
  </div>
  {#if openError || outcomeNote || repairNote}
    <!-- Notes stay in normal flow above the Composer so they never overlay a control. -->
    <div class="status-region">
      {#if openError}<div class="note error">{openError}</div>{/if}
      {#if outcomeNote}<div class="note outcome">{outcomeNote}</div>{/if}
      {#if repairNote}<div class="note warn">{repairNote}</div>{/if}
    </div>
  {/if}
  <Composer
    bind:this={composer}
    bind:text={draft}
    active={root.running}
    hasSession={!!root.reader}
    attempts={root.attempts}
    on:send={(e) => submit(e.detail)}
    on:interrupt={() => void interruptNow()}
    on:stop={() => void stopNow()}
    on:compact={compactNow}
    on:retry={(e) => retryAttempt(e.detail)}
    on:abandon={(e) => abandonAttempt(e.detail)} />
  {#if $modal === 'session'}
    <SessionPicker openRoots={pickerOpen} archivedRoots={pickerArchived} current={rootID} {workspace} loading={pickerLoading}
      on:select={(e) => pickerSelect(e.detail)}
      on:new={() => void pickerNew()}
      on:resume={(e) => void pickerResume(e.detail)}
      on:archive={(e) => void pickerArchive(e.detail)}
      on:remove={(e) => void pickerRemove(e.detail)}
      on:close={() => closeModal('session')} />
  {/if}
  {#if $modal === 'workspace'}
    <WorkspacePicker workspaces={workspaces} current={workspace}
      on:select={(e) => { closeModal('workspace'); void chooseWorkspace(e.detail); }}
      on:pick={() => void pickWorkspaceDirectory()}
      on:close={() => closeModal('workspace')} />
  {/if}
  {#if $modal === 'agent'}
    <AgentPicker agents={configuration ? configuration.payload : []} current={root.agentType}
      on:select={(e) => void pickAgentType(e.detail)}
      on:close={() => closeModal('agent')} />
  {/if}
  {#if $modal === 'settings'}
    <SettingsEditor getClient={clientNow} stamp={() => stampFor('', 'root')()}
      on:note={(e) => { outcomeNote = e.detail; }}
      on:changed={() => void refreshGlobal('configuration')}
      on:agent={() => openModal('agent')}
      on:providers={() => openModal('providers')}
      on:snapshots={() => openModal('snapshots')}
      on:viewer={() => openModal('viewer')}
      on:close={() => closeModal('settings')} />
  {/if}
  {#if $modal === 'providers'}
    <ProviderEditor getClient={clientNow} stamp={() => stampFor('', 'root')()}
      on:note={(e) => { outcomeNote = e.detail; }}
      on:changed={() => void refreshGlobal('configuration')}
      on:close={() => closeModal('providers')} />
  {/if}
  {#if $modal === 'viewer'}
    <FileViewer getClient={clientNow} stamp={() => stampFor('', 'root')()} {workspace}
      on:close={() => closeModal('viewer')} />
  {/if}
  {#if $modal === 'snapshots'}
    <SnapshotsDialog getClient={clientNow} stamp={() => stampFor('', 'root')()} {workspace} sessionID={rootID}
      on:note={(e) => { outcomeNote = e.detail; }}
      on:close={() => closeModal('snapshots')} />
  {/if}
  {#if $modal === 'usage'}
    <UsageDialog usage={root.hydration ? root.hydration.usage : { totals: { by_model: [] }, context: { used_tokens: '0', context_window: 0 } }}
      on:close={() => closeModal('usage')} />
  {/if}
  {#if $modal === 'fork' && forkItem}
    <ForkDialog item={forkItem} bind:text={forkText} bind:rewind={forkRewind}
      attempt={forkAttempt ? $attempts.get(forkAttempt.id) : null} note={forkNote} busy={forkBusy}
      on:submit={submitFork}
      on:retry={(e) => retryAttempt(e.detail)}
      on:abandon={(e) => abandonAttempt(e.detail)}
      on:close={() => closeModal('fork')} />
  {/if}
  {#if $modal === 'model'}
    <ModelDialog agentType={root.agentType} selectedModel={root.model} {models} loading={modelLoading} error={modelError}
      on:select={(e) => void pickModel(e.detail)}
      on:close={() => closeModelDialog()} />
  {/if}
  {#if $modal === 'background'}
    <BackgroundPanel children={backgroundChildren} jobs={backgroundJobs} currentChild={childID}
      on:open={(e) => { closeModal('background'); openChild(e.detail); }}
      on:close={() => closeModal('background')} />
  {/if}
  {#if $modal === 'warnings'}
    <dialog class="modal" bind:this={warningsDialog} use:openDialog
      on:click={(e) => { if (e.target === warningsDialog) warningsDialog.close(); }}
      on:close={() => closeModal('warnings')}
      aria-labelledby="warnings-title">
      <div class="panel">
        <div id="warnings-title" class="hdr"><span>Warnings</span>
          <button class="panel-close" on:click={() => warningsDialog.close()} aria-label="Close warnings">×</button>
        </div>
        <div class="list">
          {#if root.globalWarnings.length > 0}
            <div class="group-label">Global</div>
            {#each root.globalWarnings as warning}
              <div class="warning global"><span class="kind">{warning.source} · {warning.kind}</span>{warning.message}</div>
            {/each}
          {/if}
          {#if root.ownWarnings.length > 0}
            <div class="group-label">This session</div>
            {#each root.ownWarnings as warning}
              <div class="warning own"><span class="kind">{warning.source} · {warning.kind}</span>{warning.message}</div>
            {/each}
          {/if}
          {#if root.ownWarnings.length === 0 && root.globalWarnings.length === 0}
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
  .tokens { background:none; border:none; cursor:pointer; padding:0; }
  .tokens:hover { color:var(--accent); }
  .settings-btn { background:none; border:none; color:var(--text-dim); cursor:pointer; padding:0 2px; display:flex; align-items:center; }
  .settings-btn:hover { color:var(--accent); }
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
  .panel { position:absolute; top:40px; right:12px; background:var(--bg-elevated); border:1px solid var(--border-strong); min-width:320px; max-width:480px; max-height:400px; display:flex; flex-direction:column; box-shadow:var(--shadow-menu); }
  .hdr { display:flex; justify-content:space-between; align-items:center; padding:6px 12px; font-size:12px; color:var(--text-dim); text-transform:uppercase; letter-spacing:.5px; border-bottom:1px solid var(--border); }
  .list { flex:1; overflow:auto; padding:4px 0; }
  .group-label { padding:6px 12px 2px; font-size:11px; color:var(--text-dim); text-transform:uppercase; letter-spacing:.5px; }
  .warning { margin:2px 12px; padding:6px 10px; border-radius:var(--radius); font-size:12px; font-family:var(--font-ui); white-space:pre-wrap; overflow-wrap:anywhere; }
  .warning.global { background:var(--accent-soft); color:var(--text); }
  .warning.own { background:var(--error-soft); color:var(--error); }
  .kind { display:block; color:var(--text-dim); font-family:var(--font-mono); font-size:11px; }
  .empty { padding:8px 12px; color:var(--text-dim); font-family:var(--font-ui); font-size:12px; }
</style>
