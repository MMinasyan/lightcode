<script>
  import { RespondPermission, PermissionSuggest, SaveProjectPermission } from '../../wailsjs/go/main/App';
  export let permission = null;
  export let onDone = () => {};

  let showSuggest = false;
  let suggestions = [];
  let selected = {};
  let saving = false;

  async function respond(action) {
    if (!permission) return;
    try {
      await RespondPermission(permission.id, action);
    } catch (e) {
      console.error(e);
    }
    reset();
  }

  async function openSuggest() {
    if (!permission) return;
    try {
      suggestions = await PermissionSuggest(permission.tool, permission.args) || [];
      selected = {};
      showSuggest = true;
    } catch (e) {
      console.error(e);
    }
  }

  function toggleSuggestion(rule) {
    if (selected[rule]) {
      delete selected[rule];
      selected = selected;
    } else {
      selected[rule] = true;
      selected = selected;
    }
  }

  async function saveSuggestions() {
    if (!permission) return;
    const patterns = Object.keys(selected);
    if (patterns.length === 0) return;
    saving = true;
    try {
      await SaveProjectPermission(permission.id, patterns);
    } catch (e) {
      console.error(e);
    }
    reset();
  }

  function reset() {
    showSuggest = false;
    suggestions = [];
    selected = {};
    saving = false;
    onDone();
  }
</script>

<!-- svelte-ignore a11y-click-events-have-key-events -->
<div class="backdrop">
  <!-- svelte-ignore a11y-click-events-have-key-events -->
  <div class="prompt" on:click|stopPropagation>
    <div class="hdr">Permission Required</div>
    <div class="tool-info">
      <span class="tool-badge">[{permission?.tool || 'tool'}]</span>
    </div>
    <pre class="args">{permission?.args || ''}</pre>
    {#if showSuggest}
      <div class="suggest-panel">
        <div class="suggest-hdr">Allow for this project:</div>
        {#each suggestions as s}
          <!-- svelte-ignore a11y-click-events-have-key-events -->
          <label class="suggest-row" on:click|stopPropagation>
            <input type="checkbox" checked={!!selected[s.rule]} on:change={() => toggleSuggestion(s.rule)} />
            <span class="suggest-label">{s.label}</span>
          </label>
        {/each}
        <div class="suggest-actions">
          <button class="btn cancel" on:click={() => { showSuggest = false; }}>Back</button>
          <button class="btn save" on:click={saveSuggestions} disabled={saving || Object.keys(selected).length === 0}>Save</button>
        </div>
      </div>
    {:else}
      <div class="actions">
        <button class="btn deny" on:click={() => respond('deny')}>Deny</button>
        <button class="btn project" on:click={openSuggest}>Allow for project</button>
        <button class="btn allow" on:click={() => respond('allow')}>Allow</button>
      </div>
    {/if}
  </div>
</div>

<style>
  .backdrop { position:fixed; inset:0; background:var(--overlay); z-index:300; display:flex; align-items:center; justify-content:center; }
  .prompt { background:var(--bg-elevated); border:1px solid var(--border-strong); min-width:360px; max-width:500px; }
  .hdr { padding:8px 12px; font-size:12px; font-weight:600; text-transform:uppercase; letter-spacing:.5px; border-bottom:1px solid var(--border); }
  .tool-info { padding:8px 12px 0; }
  .tool-badge { color:var(--text-dim); font-size:12px; font-family:var(--font-mono); }
  .args { margin:8px 12px; padding:8px; font-family:var(--font-mono); font-size:12px; color:var(--text); white-space:pre-wrap; word-break:break-all; max-height:160px; overflow-y:auto; }
  .actions { display:flex; gap:8px; padding:8px 12px; border-top:1px solid var(--border); justify-content:flex-end; }
  .btn { padding:4px 12px; font-size:12px; cursor:pointer; border:1px solid var(--border-button); background:none; color:var(--text-dim); font-family:var(--font-ui); }
  .deny:hover { border-color:var(--warn); color:var(--warn); }
  .allow { border-color:var(--accent); color:var(--accent); }
  .allow:hover { background:var(--accent-soft); }
  .project { border-color:var(--border-button); }
  .project:hover { border-color:var(--accent); color:var(--accent); }
  .suggest-panel { border-top:1px solid var(--border); padding:8px 12px; }
  .suggest-hdr { font-size:11px; color:var(--text-dim); font-family:var(--font-ui); margin-bottom:6px; text-transform:uppercase; letter-spacing:.5px; }
  .suggest-row { display:flex; align-items:center; gap:8px; padding:3px 0; cursor:pointer; font-family:var(--font-mono); font-size:12px; color:var(--text); }
  .suggest-row input { margin:0; appearance:none; -webkit-appearance:none; width:14px; height:14px; border:1px solid var(--border-button); border-radius:2px; background:var(--bg-input); flex-shrink:0; cursor:pointer; position:relative; }
  .suggest-row input:checked { background:var(--accent); border-color:var(--accent); }
  .suggest-row input:checked::after { content:''; position:absolute; left:3.5px; top:1px; width:4px; height:7px; border:solid var(--bg); border-width:0 1.5px 1.5px 0; transform:rotate(45deg); }
  .suggest-label { flex:1; }
  .suggest-actions { display:flex; gap:8px; justify-content:flex-end; margin-top:8px; }
  .cancel:hover { color:var(--text); }
  .save { border-color:var(--accent); color:var(--accent); }
  .save:hover { background:var(--accent-soft); }
  .save:disabled { opacity:.4; cursor:default; }
</style>
