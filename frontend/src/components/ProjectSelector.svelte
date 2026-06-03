<script>
  import { ProjectList, ProjectSwitch, ProjectPickAndSwitch, ProjectCurrent } from '../../wailsjs/go/main/App';
  import { createEventDispatcher, onMount } from 'svelte';
  import { errorText } from '../lib/errors.js';
  const dispatch = createEventDispatcher();

  let projects = [];
  let loading = true;
  let currentPath = '';

  async function load() {
    loading = true;
    try {
      projects = await ProjectList() || [];
      const cur = await ProjectCurrent();
      currentPath = cur?.path || '';
    } catch (e) { dispatch('error', errorText(e)); }
    loading = false;
  }

  onMount(load);

  async function pick(path) {
    try { await ProjectSwitch(path); dispatch('close'); }
    catch (e) { dispatch('error', errorText(e)); }
  }

  async function addProject() {
    try { await ProjectPickAndSwitch(); dispatch('close'); }
    catch (e) { dispatch('error', errorText(e)); }
  }
</script>

<div class="layer">
  <button type="button" class="backdrop" tabindex="-1" aria-label="Close project selector" on:click={() => dispatch('close')}></button>
  <div class="menu" role="dialog" aria-modal="true" aria-labelledby="project-selector-title" tabindex="-1">
    <div class="tabs">
      <div class="title" id="project-selector-title">Projects</div>
      <button class="new" on:click={addProject} title="Open directory">+</button>
    </div>
    <div class="list">
      {#if loading}
        <div class="empty">loading...</div>
      {:else if projects.length === 0}
        <div class="empty">(no projects yet)</div>
      {:else}
        {#each projects as p}
          <!-- svelte-ignore a11y-click-events-have-key-events -->
          <div class="row" class:cur={p.path===currentPath} on:click={() => pick(p.path)} role="button" tabindex="0" title={p.path}>
            <span class="name">{p.name}</span>
            <span class="path">{p.path}</span>
          </div>
        {/each}
      {/if}
    </div>
  </div>
</div>

<style>
  .layer { position:fixed; inset:0; z-index:100; }
  .backdrop { position:absolute; inset:0; border:0; padding:0; margin:0; background:transparent; cursor:default; }
  .menu { position:absolute; z-index:1; top:40px; left:12px; background:var(--bg-elevated); border:1px solid var(--border-strong); min-width:360px; max-width:560px; max-height:360px; display:flex; flex-direction:column; box-shadow:var(--shadow-menu); }
  .tabs { display:flex; align-items:center; gap:0; border-bottom:1px solid var(--border); }
  .title { color:var(--text-dim); font-family:var(--font-ui); font-size:12px; padding:6px 14px; }
  .new { margin-left:auto; background:none; border:none; color:var(--text-dim); font-family:var(--font-ui); font-size:14px; padding:4px 10px; cursor:pointer; }
  .new:hover { color:var(--accent); }
  .list { flex:1; overflow:auto; }
  .empty { padding:8px 12px; color:var(--text-dim); font-family:var(--font-ui); font-size:12px; }
  .row { display:flex; justify-content:space-between; gap:16px; padding:4px 12px; font-family:var(--font-ui); font-size:12px; color:var(--text); cursor:pointer; }
  .row:hover { background:var(--accent-soft); }
  .row.cur { color:var(--accent); }
  .name { white-space:nowrap; }
  .path { color:var(--text-dim); overflow:hidden; text-overflow:ellipsis; white-space:nowrap; font-family:var(--font-mono); }
</style>
