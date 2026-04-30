<script>
  import { SessionList, SessionSwitch, SessionNew, SessionCurrent, SessionArchive, SessionDelete } from '../../wailsjs/go/main/App';
  import { createEventDispatcher, onMount } from 'svelte';
  const dispatch = createEventDispatcher();

  let state = 'active';
  let sessions = [];
  let loading = true;
  let currentId = '';

  async function load() {
    loading = true;
    try {
      sessions = await SessionList(state) || [];
      const cur = await SessionCurrent();
      currentId = cur?.id || '';
    } catch (e) { console.error(e); }
    loading = false;
  }

  onMount(load);

  function setState(s) { if (s !== state) { state = s; load(); } }

  async function pick(id) {
    try { await SessionSwitch(id); dispatch('close'); }
    catch (e) { console.error(e); }
  }

  async function newSession() {
    try { await SessionNew(); dispatch('close'); }
    catch (e) { console.error(e); }
  }

  async function archive(id) {
    try { await SessionArchive(id); await load(); }
    catch (e) { console.error(e); }
  }

  async function remove(id) {
    try { await SessionDelete(id); await load(); }
    catch (e) { console.error(e); }
  }

  function fmtTs(unix) {
    if (!unix) return '';
    const d = new Date(unix * 1000);
    const mon = ['Jan','Feb','Mar','Apr','May','Jun','Jul','Aug','Sep','Oct','Nov','Dec'][d.getMonth()];
    const pad = n => n<10 ? '0'+n : n;
    return `${mon} ${d.getDate()} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
  }
</script>

<!-- svelte-ignore a11y-click-events-have-key-events -->
<div class="backdrop" on:click={() => dispatch('close')}>
  <!-- svelte-ignore a11y-click-events-have-key-events -->
  <div class="menu" on:click|stopPropagation>
    <div class="tabs">
      <button class="tab" class:sel={state==='active'} on:click={() => setState('active')}>Active</button>
      <button class="tab" class:sel={state==='archived'} on:click={() => setState('archived')}>Archived</button>
      {#if state === 'active'}
        <button class="new" on:click={newSession} title="New session">+</button>
      {/if}
    </div>
    <div class="list">
      {#if loading}
        <div class="empty">loading...</div>
      {:else if sessions.length === 0}
        <div class="empty">(no {state} sessions)</div>
      {:else}
        {#each sessions as s}
          <!-- svelte-ignore a11y-click-events-have-key-events -->
          <div class="row" class:cur={s.id===currentId} on:click={() => pick(s.id)} role="button" tabindex="0">
            <span class="id">{s.id}</span>
            <span class="ts">{fmtTs(s.lastActivity)}</span>
            {#if state === 'active'}
              <button class="act" on:click|stopPropagation={() => archive(s.id)} title="Archive" aria-label="Archive">
                <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
                  <polyline points="21 8 21 21 3 21 3 8"/>
                  <rect x="1" y="3" width="22" height="5"/>
                  <line x1="10" y1="12" x2="14" y2="12"/>
                </svg>
              </button>
            {:else}
              <button class="act" on:click|stopPropagation={() => remove(s.id)} title="Delete" aria-label="Delete">
                <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
                  <polyline points="3 6 5 6 21 6"/>
                  <path d="M19 6l-1 14a2 2 0 0 1-2 2H8a2 2 0 0 1-2-2L5 6"/>
                  <path d="M10 11v6"/>
                  <path d="M14 11v6"/>
                  <path d="M9 6V4a2 2 0 0 1 2-2h2a2 2 0 0 1 2 2v2"/>
                </svg>
              </button>
            {/if}
          </div>
        {/each}
      {/if}
    </div>
  </div>
</div>

<style>
  .backdrop { position:fixed; inset:0; z-index:100; }
  .menu { position:absolute; top:40px; left:160px; background:var(--bg-elevated); border:1px solid var(--border-strong); min-width:260px; max-height:360px; display:flex; flex-direction:column; box-shadow:var(--shadow-menu); }
  .tabs { display:flex; align-items:center; gap:0; border-bottom:1px solid var(--border); }
  .tab { background:none; border:none; border-right:1px solid var(--border); color:var(--text-dim); font-family:var(--font-ui); font-size:12px; padding:6px 14px; cursor:pointer; }
  .tab.sel { background:var(--text-dim); color:var(--bg-elevated); }
  .new { margin-left:auto; background:none; border:none; color:var(--text-dim); font-family:var(--font-ui); font-size:14px; padding:4px 10px; cursor:pointer; }
  .new:hover { color:var(--accent); }
  .list { flex:1; overflow:auto; }
  .empty { padding:8px 12px; color:var(--text-dim); font-family:var(--font-ui); font-size:12px; }
  .row { display:flex; align-items:center; gap:12px; padding:4px 12px; font-family:var(--font-ui); font-size:12px; color:var(--text); cursor:pointer; }
  .row:hover { background:var(--accent-soft); }
  .row.cur { color:var(--accent); }
  .id { flex:1; }
  .ts { color:var(--text-dim); }
  .act { background:none; border:none; color:var(--text-dim); padding:0 2px; cursor:pointer; visibility:hidden; display:flex; align-items:center; }
  .row:hover .act { visibility:visible; }
  .act:hover { color:var(--accent); }
</style>
