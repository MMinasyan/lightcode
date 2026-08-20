<script>
  import { SessionList, SessionSwitch, SessionNew, SessionCurrent, SessionArchive, SessionDelete } from '../../wailsjs/go/main/App';
  import { createEventDispatcher, onMount } from 'svelte';
  import { errorText } from '../lib/errors.js';
  const dispatch = createEventDispatcher();

  // App passes the presented state down: whether a snapshot has seeded the view,
  // which session it presents, and at what presentation generation. Every mutation
  // captures these before calling the backend so its rejection can be classified
  // against whatever owns presentation by settle time — not against live props a
  // boundary may already have moved on.
  export let seeded = false;
  export let currentSessionId = '';
  export let generation = 0;

  let state = 'active';
  let sessions = [];
  let loading = true;
  // Unseeded views read the presented session from the owner at load — this is the
  // only SessionCurrent call in steady state. Seeded views use the presented id App
  // passes down, so an open selector never re-resolves identity out of band.
  let liveCurrentId = '';
  $: currentId = seeded ? (currentSessionId || '') : liveCurrentId;

  async function load() {
    loading = true;
    try {
      sessions = await SessionList(state) || [];
      if (!seeded) {
        const cur = await SessionCurrent();
        liveCurrentId = cur?.id || '';
      }
    } catch (e) { dispatch('error', errorText(e)); }
    loading = false;
  }

  onMount(load);

  function setState(s) { if (s !== state) { state = s; load(); } }

  // Every mutation captures the presented state into its OWN lexical scope before calling
  // the backend, so its rejection classifies against what owned presentation at ITS call time —
  // not live props a boundary may already have moved on, and not whatever an overlapping action
  // captured later. Only ever one surface may show a given failure: this catch or the backend
  // frames it owns.

  // settleRemoval disposes one rejected archive/delete against that action's own capture cap =
  // {seeded, gen, currentId}. The selector closes on every error: its list is stale against
  // whatever ran durably, or about to be replaced by a boundary either way. Visibility then
  // follows ownership of presentation at settle time — only ever one surface may show the failure:
  // - generation advanced since THIS action's capture ⇒ some boundary applied (a committed outcome
  //   or a newer navigation) and owns the view; suppress.
  // - seeded at this call time ⇒ App shows this path's own unsequenced backend error frame
  //   (every rejected removal carries one, plain or committed); showing here would double it —
  //   suppress. Noncurrent outcomes are generation-neutral, so only the seed gate can tell their
  //   frames apart from a stale catch.
  // - an unseeded rejection has no visible surface at all until some snapshot seeds the view (App
  //   drops its error frames meanwhile): the gated catch stays sole — except for one mandated
  //   distinction, below.
    async function settleRemoval(id, err, cap) {
      dispatch('close');
      if (generation !== cap.gen || cap.seeded) return;
      // The rejection's own text is derived from this action's error: it is what this catch shows
      // whenever no frame owns the failure — including when the distinction read below fails under
      // it. Only ever one surface may show a given failure.
      const text = errorText(err);
      if (id === cap.currentId && id !== '') {
        try {
          // Rejected removal of this action's presented session: one owner read — the only one any
          // mutation may trigger, and it is never retried — tells a precommit failure, where the
          // target is still current and this catch stays sole, from a committed detach or newer
          // navigation that already left it, whose frames own the error once they land. A read that
          // fails under the rejection settles nothing: its own error would be a second surface for
          // this one failure, so it falls through to this action's text and shows as-is.
          const cur = await SessionCurrent();
          liveCurrentId = cur?.id || '';
          if (liveCurrentId !== id) { sessions = sessions.filter(s => s.id !== id); return; }
        } catch (e) { /* the distinction itself failed: retain this same-generation error below */ }
      }
      // The awaited read above yields to the event loop, and a boundary can apply in that gap —
      // A→B→A included, where even an owner still reporting the target has moved on underneath.
      // Recheck THIS action's capture against what is presented now: any applied snapshot advanced
      // the generation (and re-seeded views move their presented session with it), so a capture no
      // longer matching presentation owns this error through its own frames, not this catch. An
      // overlapping action settling in between changes nothing here — cap is lexical to this call.
      if (generation !== cap.gen || currentSessionId !== '' && currentSessionId !== cap.currentId) return;
      dispatch('error', text);
    }

  // settleNavigation disposes one rejected open/new against that action's own capture {gen}. These
  // paths emit no backend frame for a plain failure — only committed outcomes carry boundary+error
  // pairs — so the generation alone settles them: same-generation rejections surface here as the sole
  // visible error; an advanced generation means the committed pair (or newer navigation) owns
  // presentation, and its stateful snapshot wipes any transient shown first.
  function settleNavigation(err, cap) {
    dispatch('close');
    if (generation === cap.gen) dispatch('error', errorText(err));
  }

  async function archive(id) {
    const cap = { seeded, gen: generation, currentId }; // this action's own capture — an overlapping call cannot overwrite it
    try { await SessionArchive(id); sessions = sessions.filter(s => s.id !== id); } // local removal on success — never a reload: every other outcome is owned by backend frames, and a re-list could race them or resurrect a row a boundary already removed.
    catch (e) { settleRemoval(id, e, cap); }
  }

  async function remove(id) {
    const cap = { seeded, gen: generation, currentId }; // this action's own capture — an overlapping call cannot overwrite it
    try { await SessionDelete(id); sessions = sessions.filter(s => s.id !== id); }
    catch (e) { settleRemoval(id, e, cap); }
  }

  async function switchRow(mutate) {
    const cap = { gen: generation }; // navigation paths need no read and classify on generation alone; the capture is still lexical to this action
    try { await mutate(); dispatch('close'); }
    catch (e) { settleNavigation(e, cap); }
  }

  const pick = id => switchRow(() => SessionSwitch(id));
  const newSession = () => switchRow(SessionNew);

  function fmtTs(unix) {
    if (!unix) return '';
    const d = new Date(unix * 1000);
    const mon = ['Jan','Feb','Mar','Apr','May','Jun','Jul','Aug','Sep','Oct','Nov','Dec'][d.getMonth()];
    const pad = n => n<10 ? '0'+n : n;
    return `${mon} ${d.getDate()} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
  }
</script>

<div class="layer">
  <button type="button" class="backdrop" tabindex="-1" aria-label="Close session selector" on:click={() => dispatch('close')}></button>
  <div class="menu" role="dialog" aria-modal="true" aria-labelledby="session-selector-title" tabindex="-1">
    <div id="session-selector-title" class="sr-only">Sessions</div>
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
  .layer { position:fixed; inset:0; z-index:100; }
  .backdrop { position:absolute; inset:0; border:0; padding:0; margin:0; background:transparent; cursor:default; }
  .menu { position:absolute; z-index:1; top:40px; left:160px; background:var(--bg-elevated); border:1px solid var(--border-strong); min-width:260px; max-height:360px; display:flex; flex-direction:column; box-shadow:var(--shadow-menu); }
  .sr-only { position:absolute; width:1px; height:1px; padding:0; margin:-1px; overflow:hidden; clip:rect(0, 0, 0, 0); white-space:nowrap; border:0; }
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
