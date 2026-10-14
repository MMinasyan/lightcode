<script>
  import { openDialog } from './dialogs';
  // Runtime settings editor: Settings stay the exact JSON text read from the
  // generated ConfigurationView and are edited one member at a time through
  // native PatchJSON. A save is one complete-body PUT (no revision
  // precondition), so a stale save intentionally overwrites newer changes.
  // ONE pending value (idle/read/edit/save) gates every input and action.
  import { createEventDispatcher, onDestroy, onMount } from 'svelte';
  import { AppVersion, PatchJSON } from '../../client-bindings/wailsjs/go/desktopclient/Host';
  import { errorText } from '../lib/errors.js';
  import { settings } from '../lib/settings.js';
  import { invokeNative } from './results';
  import { sameStamp } from './reader';
  import {
    composeMemberPatch,
    composeObjectPatch,
    composePluginsPatch,
    composeSessionsPatch,
    extractSettingsText,
    parseExactJSON,
    parseSettingsObject,
  } from './exactjson';
  export let getClient;
  export let stamp; // () => ReaderStamp
  const dispatch = createEventDispatcher();

  // First-party numeric limit controls; every other plugin entry is raw inspect/edit/remove.
  const PLUGIN_FIELDS = {
    tools: ['max_output_bytes', 'read_max_lines', 'read_line_max_chars', 'command_timeout'],
    jobs: ['max_background_processes', 'max_output_bytes', 'read_line_max_chars'],
    tasks: ['max_concurrent', 'max_output_bytes'],
  };
  const SESSION_FIELDS = ['archive_after_days', 'delete_after_archive_days'];

  let dialog;

  // The retained native version row starts from its fallback and keeps it when
  // the native read fails, reporting that failure through the existing note
  // surface; the appearance rows below are host-local.
  let appVersion = '';

  onMount(() => {
    void load();
    AppVersion().then((value) => { appVersion = value; }).catch((error) => {
      dispatch('note', 'Version read failed: ' + errorText(error));
    });
  });
  let settingsText = null;
  let parsed = null;
  let readError = '';
  let noteText = '';
  // The one local operation owner.
  let pending = 'idle';
  let disposed = false;
  let newPluginID = '';
  let newPluginDocument = '';

  $: busy = pending !== 'idle';
  $: parsed = settingsText === null ? null : safeParse(settingsText);
  $: sessions = parsed && parsed.sessions && typeof parsed.sessions === 'object' ? parsed.sessions : {};
  $: plugins = parsed && parsed.plugins && typeof parsed.plugins === 'object' ? parsed.plugins : {};
  $: authoredIDs = Object.keys(plugins);
  $: otherAuthoredIDs = authoredIDs.filter((id) => !Object.hasOwn(PLUGIN_FIELDS, id));

  function safeParse(text) {
    try {
      return parseSettingsObject(text);
    } catch {
      return null;
    }
  }


  onDestroy(() => {
    disposed = true;
  });

  const invoke = (label, operation, params, decode) => invokeNative(getClient, label, operation, params, decode);

  // One local edit unit: compose against the current document, publish only
  // while not disposed; a native failure leaves the document unchanged.
  async function applyEdit(compose) {
    if (disposed || pending !== 'idle') return;
    pending = 'edit';
    try {
      const current = settingsText;
      if (current === null) return;
      const next = await compose(current);
      if (disposed) return;
      settingsText = next;
      noteText = '';
    } catch (e) {
      noteText = 'Edit rejected: ' + errorText(e);
    } finally {
      pending = 'idle';
    }
  }

  function editSession(member, valueText) {
    void applyEdit(async (current) => {
      const sessionsNow = parseSettingsObject(current).sessions || {};
      return PatchJSON(current, composeMemberPatch('sessions', composeSessionsPatch(sessionsNow, member, valueText)));
    });
  }

  // A plugin edit patches the member, then places the document back into `plugins`.
  function patchPlugin(pluginID, member, valueText) {
    void applyEdit(async (current) => {
      const pluginsNow = parseSettingsObject(current).plugins || {};
      const document = typeof pluginsNow[pluginID] === 'string' ? pluginsNow[pluginID] : '{}';
      const nextDocument = await PatchJSON(document, composeMemberPatch(member, valueText));
      return PatchJSON(current, composeMemberPatch('plugins', composePluginsPatch(pluginsNow, pluginID, nextDocument)));
    });
  }

  // Raw authoring/edit/remove for any plugin entry; the server decides validity.
  function editDocument(pluginID, document) {
    void applyEdit(async (current) => {
      const pluginsNow = parseSettingsObject(current).plugins || {};
      return PatchJSON(current, composeMemberPatch('plugins', composePluginsPatch(pluginsNow, pluginID, document)));
    });
  }

  function authorPlugin() {
    const id = newPluginID.trim();
    if (id === '') return;
    void editDocument(id, newPluginDocument);
    newPluginDocument = '';
  }

  // One stamped exact read for the initial load and a successful save's refresh.
  // The exact-text settings decoder consumes the accepted body at the boundary.
  async function readSettings() {
    const cap = stamp();
    const classified = await invoke('configuration', 'GetConfiguration', '{}', extractSettingsText);
    if (disposed || !sameStamp(cap, stamp())) return;
    if (classified.kind !== 'ok') {
      readError = classified.message;
      return;
    }
    settingsText = classified.data;
    readError = '';
  }

  async function load() {
    if (disposed || pending !== 'idle') return;
    pending = 'read';
    try {
      await readSettings();
    } finally {
      pending = 'idle';
    }
  }

  function sessionValue(member) {
    return member in sessions ? JSON.stringify(sessions[member]) : '';
  }

  function documentText(pluginID) {
    const document = plugins[pluginID];
    return typeof document === 'string' ? document : null;
  }

  function documentValues(pluginID) {
    const document = documentText(pluginID);
    if (document === null) return {};
    try {
      const value = parseExactJSON(document);
      return value !== null && typeof value === 'object' && !Array.isArray(value) ? value : {};
    } catch {
      return {};
    }
  }

  function memberValue(pluginID, member) {
    const values = documentValues(pluginID);
    return member in values ? JSON.stringify(values[member]) : '';
  }

  async function save() {
    if (disposed || pending !== 'idle') return;
    // Capture the text once; inputs are gated for the whole save unit.
    const text = settingsText;
    if (text === null) return;
    pending = 'save';
    try {
      // The authored settings text is composed into the body byte-exact.
      const classified = await invoke('settings save', 'UpdateConfigurationSettings', composeObjectPatch([
        ['body', `{"settings":${text}}`],
      ]), JSON.parse);
      if (classified.kind === 'rejection') {
        dispatch('note', 'Settings save failed: ' + classified.message);
        return;
      }
      if (classified.kind !== 'ok') {
        dispatch('note', 'Settings save outcome unknown; nothing was retransmitted.');
        return;
      }
      dispatch('note', 'Settings saved.');
      dispatch('changed');
      await readSettings();
    } finally {
      pending = 'idle';
    }
  }
</script>

<dialog class="modal" bind:this={dialog} use:openDialog aria-labelledby="settings-title"
  on:click={(e) => { if (e.target === dialog) dialog.close(); }}
  on:close={() => dispatch('close')}>
  <div class="panel">
    <div id="settings-title" class="hdr"><span>Settings</span>
      <button class="panel-close" on:click={() => dialog.close()} aria-label="Close settings">×</button>
    </div>
    <!-- The retained desktop reached provider/model configuration from inside
         this settings surface; these are that surface's own nav entries, not a
         toolbar strip. -->
    <div class="nav">
      <button class="navitem" on:click={() => dispatch('agent')} aria-label="Agent type">Agent type</button>
      <button class="navitem" on:click={() => dispatch('providers')} aria-label="Providers">Providers</button>
      <button class="navitem" on:click={() => dispatch('snapshots')} aria-label="Snapshots">Snapshots</button>
      <button class="navitem" on:click={() => dispatch('viewer')} aria-label="File viewer">File viewer</button>
    </div>
    <div class="body">
      {#if readError}
        <div class="error">{readError}</div>
      {/if}
      <!-- Appearance and the native version are host-local UI state: they stay
           usable while the Runtime settings read is pending or unavailable and
           never author Runtime configuration. -->
      <div class="sec">Appearance</div>
      <label class="row"><span class="label">Wrap code blocks</span>
        <input class="check" type="checkbox" checked={$settings.wrapCode} aria-label="Wrap code blocks"
          on:change={(e) => settings.update((s) => ({ ...s, wrapCode: e.target.checked }))} />
      </label>
      <label class="row"><span class="label">Font scale</span>
        <input class="num" type="number" min="50" max="200" step="5" value={$settings.fontScale} aria-label="Font scale"
          on:change={(e) => settings.update((s) => ({ ...s, fontScale: Math.max(50, Math.min(200, Number(e.target.value) || 100)) }))} />
      </label>
      <div class="row"><span class="label">Version</span><span class="readonly" aria-label="Version">{appVersion}</span></div>
      {#if settingsText === null}
        <div class="empty">{pending === 'read' ? 'Loading…' : '(no settings read yet)'}</div>
      {:else}
        <div class="sec">Sessions</div>
        <div class="row"><span class="label">Auto archive</span><span class="readonly">{'auto_archive' in sessions ? JSON.stringify(sessions.auto_archive) : ''}</span></div>
        {#each SESSION_FIELDS as member (member)}
          <label class="row"><span class="label">{member}</span>
            <input class="num" type="text" value={sessionValue(member)} aria-label={member} disabled={busy}
              on:change={(e) => void editSession(member, e.target.value)} />
          </label>
        {/each}

        {#each Object.keys(PLUGIN_FIELDS) as pluginID (pluginID)}
          <div class="sec">{pluginID} plugin</div>
          {#if documentText(pluginID) === null}
            <div class="hint">Uses plugin defaults</div>
          {/if}
          {#each PLUGIN_FIELDS[pluginID] as member (member)}
            <label class="row"><span class="label">{member}</span>
              <input class="num" type="text" value={memberValue(pluginID, member)} aria-label={`${pluginID} ${member}`} disabled={busy}
                on:change={(e) => void patchPlugin(pluginID, member, e.target.value)} />
            </label>
          {/each}
          {#if documentText(pluginID) !== null}
            <label class="row raw"><span class="label">Raw document</span>
              <textarea class="doc" rows="3" value={documentText(pluginID)} aria-label={`${pluginID} raw document`} disabled={busy}
                on:change={(e) => void editDocument(pluginID, e.target.value)}></textarea>
            </label>
            <div class="row"><span class="label"></span>
              <button class="ghost" disabled={busy} on:click={() => void editDocument(pluginID, null)} aria-label={`Remove ${pluginID} document`}>Remove authored document</button>
            </div>
          {/if}
        {/each}

        {#if otherAuthoredIDs.length > 0}
          <div class="sec">Other plugin documents</div>
          {#each otherAuthoredIDs as pluginID (pluginID)}
            <label class="row raw"><span class="label">{pluginID}</span>
              <textarea class="doc" rows="3" value={documentText(pluginID)} aria-label={`${pluginID} raw document`} disabled={busy}
                on:change={(e) => void editDocument(pluginID, e.target.value)}></textarea>
            </label>
            <div class="row"><span class="label"></span>
              <button class="ghost" disabled={busy} on:click={() => void editDocument(pluginID, null)} aria-label={`Remove ${pluginID} document`}>Remove authored document</button>
            </div>
          {/each}
        {/if}

        <div class="sec">Author a plugin document</div>
        <label class="row"><span class="label">Plugin ID</span>
          <input class="num" type="text" bind:value={newPluginID} disabled={busy} aria-label="Plugin id" />
        </label>
        <label class="row raw"><span class="label">Raw document</span>
          <textarea class="doc" rows="3" bind:value={newPluginDocument} disabled={busy} aria-label="Plugin document" placeholder={'{}'}></textarea>
        </label>
        <div class="row"><span class="label"></span>
          <button class="ghost" disabled={busy || !newPluginID.trim()} on:click={authorPlugin} aria-label="Author plugin document">Add document</button>
        </div>

        {#if noteText}<div class="note">{noteText}</div>{/if}
        <div class="actions">
          <span class="save-note">Saving sends the complete Settings body; newer concurrent changes are overwritten.</span>
          <button class="primary" disabled={busy} on:click={() => void save()} aria-label="Save settings">{pending === 'save' ? 'Saving…' : 'Save'}</button>
        </div>
      {/if}
    </div>
  </div>
</dialog>

<style>
  .panel { position:absolute; top:40px; left:50%; transform:translateX(-50%); width:620px; max-width:94vw; max-height:86vh; background:var(--bg-elevated); border:1px solid var(--border-strong); box-shadow:var(--shadow-lg); display:flex; flex-direction:column; }
  .hdr { display:flex; justify-content:space-between; align-items:center; padding:6px 12px; font-size:12px; color:var(--text-dim); text-transform:uppercase; letter-spacing:.5px; border-bottom:1px solid var(--border); }
  .nav { display:flex; gap:0; border-bottom:1px solid var(--border); }
  .navitem { background:none; border:none; border-right:1px solid var(--border); color:var(--text-dim); font-family:var(--font-ui); font-size:12px; padding:5px 14px; cursor:pointer; }
  .navitem:hover { color:var(--accent); }
  .body { padding:10px 12px; overflow-y:auto; display:flex; flex-direction:column; gap:12px; }
  .sec { color:var(--text-dim); font-family:var(--font-ui); font-size:11px; text-transform:uppercase; letter-spacing:.5px; border-top:1px solid var(--border); padding-top:8px; }
  .row { display:grid; grid-template-columns:200px 1fr; align-items:center; gap:8px; }
  .label { color:var(--text-dim); font-family:var(--font-ui); font-size:12px; }
  .readonly { color:var(--text); font-family:var(--font-mono); font-size:12px; }
  .num, .doc { width:100%; box-sizing:border-box; padding:5px 8px; border:1px solid var(--border-button); border-radius:var(--radius); background:var(--bg-input); color:var(--text); font-family:var(--font-mono); font-size:12px; outline:none; }
  .num:focus, .doc:focus { border-color:var(--accent); background:var(--bg-input-focus); }
  .doc { resize:vertical; }
  .hint { color:var(--text-dim); font-family:var(--font-ui); font-size:11px; font-style:italic; }
  .error { color:var(--error); font-family:var(--font-ui); font-size:12px; white-space:pre-wrap; }
  .note { color:var(--text); font-family:var(--font-ui); font-size:12px; }
  .empty { color:var(--text-dim); font-family:var(--font-ui); font-size:12px; }
  .actions { display:flex; align-items:center; justify-content:flex-end; gap:10px; border-top:1px solid var(--border); padding-top:8px; }
  .save-note { flex:1; color:var(--text-dim); font-family:var(--font-ui); font-size:11px; }
  .ghost, .primary { padding:5px 12px; border:1px solid var(--border-button); border-radius:var(--radius); background:none; color:var(--text-dim); font-family:var(--font-ui); font-size:12px; cursor:pointer; }
  .ghost:hover:not(:disabled), .primary:hover:not(:disabled) { color:var(--accent); border-color:var(--accent); }
  .ghost:disabled, .primary:disabled { opacity:.45; cursor:default; }
</style>
