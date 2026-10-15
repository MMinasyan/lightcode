<script>
  import { openDialog } from './dialogs';
  import { createEventDispatcher } from 'svelte';
  import ModelPicker from '../components/ModelPicker.svelte';
  export let agentType = '';
  export let selectedModel = ''; // the Session's selected_model, shown separately
  export let models = []; // [{ ref, provider, providerName, displayName, model }]
  export let loading = false;
  export let error = '';
  const dispatch = createEventDispatcher();
  let dialog;


  function pick(ref) {
    dispatch('select', ref);
  }
</script>

<dialog class="modal" bind:this={dialog} use:openDialog aria-labelledby="model-dialog-title"
  on:click={(e) => { if (e.target === dialog) dialog.close(); }}
  on:close={() => dispatch('close')}>
  <div class="dialog-body">
    <div id="model-dialog-title" class="hdr">
      <span>Model for agent type <span class="type">{agentType || '-'}</span></span>
      <button class="close" on:click={() => dialog.close()} aria-label="Close model picker">×</button>
    </div>
    <div class="scope">
      Changes the model for future messages across <strong>all sessions using this agent type</strong>,
      not only the current session. The selected model is shown separately from any model an
      active operation captured.
    </div>
    {#if selectedModel}
      <div class="selected">Currently selected model: <span class="ref">{selectedModel}</span></div>
    {/if}
    {#if loading}
      <div class="loading">Loading models...</div>
    {:else if error}
      <div class="error">{error}</div>
    {:else}
      <div class="picker">
        <ModelPicker value={selectedModel} {models} on:change={(e) => pick(e.detail)} />
      </div>
    {/if}
  </div>
</dialog>

<style>
  .dialog-body { position:absolute; z-index:1; bottom:60px; left:12px; background:var(--bg-elevated); border:1px solid var(--border-strong); border-radius:var(--radius); min-width:320px; max-width:420px; box-shadow:var(--shadow-lg); padding:12px; display:flex; flex-direction:column; gap:8px; }
  .hdr { display:flex; justify-content:space-between; align-items:center; font-size:12px; color:var(--text-dim); text-transform:uppercase; letter-spacing:.5px; }
  .type { color:var(--accent); text-transform:none; font-family:var(--font-mono); }
  .close { background:none; border:none; color:var(--text-dim); font-size:16px; cursor:pointer; padding:0 4px; }
  .close:hover { color:var(--accent); }
  .scope { font-size:12px; color:var(--text-dim); font-family:var(--font-ui); }
  .selected { font-size:12px; color:var(--text-dim); font-family:var(--font-ui); }
  .ref { color:var(--text); font-family:var(--font-mono); }
  .loading { padding:12px; color:var(--text-dim); font-size:13px; }
  .error { padding:8px; color:var(--error); font-size:12px; }
  .picker { width:100%; }
</style>
