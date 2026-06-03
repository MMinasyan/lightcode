<script>
  import { createEventDispatcher } from 'svelte';
  import { fmtTokens } from '../lib/format.js';
  export let tokens = { total: { cache:0, input:0, output:0, known:true }, perModel: [] };
  const dispatch = createEventDispatcher();

  function pad(s, w) { s = String(s); return s.length >= w ? s : s + ' '.repeat(w - s.length); }
  function padL(s, w) { s = String(s); return s.length >= w ? s : ' '.repeat(w - s.length) + s; }

  $: lines = (() => {
    const per = tokens?.perModel || [];
    const items = per.flatMap(e => [
      { col0: e.model, c1: fmtTokens(e.cache, e.known), c2: fmtTokens(e.input, e.known), c3: fmtTokens(e.output, e.known) },
      { col0: e.provider, c1: '', c2: '', c3: '' },
    ]);
    const showTotal = per.length > 1;
    if (showTotal) {
      items.push({ col0: 'total', c1: fmtTokens(tokens.total.cache, tokens.total.known), c2: fmtTokens(tokens.total.input, tokens.total.known), c3: fmtTokens(tokens.total.output, tokens.total.known) });
    }
    const w0 = Math.max(5, ...items.map(r => r.col0.length));
    const w1 = Math.max(7, ...items.map(r => r.c1.length));
    const w2 = Math.max(7, ...items.map(r => r.c2.length));
    const w3 = Math.max(8, ...items.map(r => r.c3.length));
    const header = `${pad('model', w0)}  ${padL('⚡ cache', w1)}  ${padL('↑ input', w2)}  ${padL('↓ output', w3)}`;
    const body = items.map(r => `${pad(r.col0, w0)}  ${padL(r.c1, w1)}  ${padL(r.c2, w2)}  ${padL(r.c3, w3)}`);
    return [header, ...(per.length === 0 ? ['', '(no usage recorded yet)'] : body)].join('\n');
  })();
</script>

<div class="layer">
  <button type="button" class="backdrop" tabindex="-1" aria-label="Close token usage" on:click={() => dispatch('close')}></button>
  <div class="prompt" role="dialog" aria-modal="true" aria-labelledby="token-details-title" tabindex="-1">
    <div class="hdr" id="token-details-title">Token Usage</div>
    <pre class="args">{lines}</pre>
    <div class="actions">
      <button class="btn" on:click={() => dispatch('close')}>Close</button>
    </div>
  </div>
</div>

<style>
  .layer { position:fixed; inset:0; z-index:300; display:flex; align-items:center; justify-content:center; }
  .backdrop { position:absolute; inset:0; border:0; padding:0; margin:0; background:var(--overlay); cursor:default; }
  .prompt { position:relative; z-index:1; background:var(--bg-elevated); border:1px solid var(--border-strong); min-width:480px; max-width:720px; }
  .hdr { padding:8px 12px; font-size:12px; font-weight:600; text-transform:uppercase; letter-spacing:.5px; border-bottom:1px solid var(--border); }
  .args { margin:8px 12px; padding:8px; font-family:var(--font-mono); font-size:12px; color:var(--text); white-space:pre; max-height:320px; overflow:auto; }
  .actions { display:flex; gap:8px; padding:8px 12px; border-top:1px solid var(--border); justify-content:flex-end; }
  .btn { padding:4px 12px; font-size:12px; cursor:pointer; border:1px solid var(--border-button); background:none; color:var(--text-dim); font-family:var(--font-ui); }
  .btn:hover { border-color:var(--accent); color:var(--text); }
</style>
