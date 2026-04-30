<script>
  import { viewer, closeViewer } from '../lib/viewer.js';
  import { settings } from '../lib/settings.js';
  import ToolCall from './ToolCall.svelte';
</script>

{#if $viewer}
  <div class="viewer">
    <div class="hdr">
      <span class="title">{$viewer.title}</span>
      {#if $viewer.live}<span class="live-badge">live</span>{/if}
      <button class="close" on:click={closeViewer} title="Close">×</button>
    </div>
    {#if $viewer.live}
      <div class="live-content">
        {#each $viewer.messages as msg, i (i)}
          {#if msg.type === 'assistant'}
            <div class="sa-text">{msg.content}</div>
          {:else if msg.type === 'tool'}
            <ToolCall name={msg.name} args={msg.args} result={msg.result} success={msg.success} done={msg.done} />
          {/if}
        {/each}
      </div>
    {:else}
      <pre class="content" class:wrap={$settings.wrapCode}>{$viewer.content}</pre>
    {/if}
  </div>
{/if}

<style>
  .viewer { flex:1; min-width:0; display:flex; flex-direction:column; overflow:hidden; }
  .hdr { display:flex; align-items:center; gap:8px; padding:6px 12px; background:var(--bg-elevated); border-bottom:1px solid var(--border); min-height:36px; }
  .title { flex:1; color:var(--text); font-family:var(--font-mono); font-size:calc(12px * var(--scale, 1)); overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
  .live-badge { color:var(--accent); font-family:var(--font-ui); font-size:calc(10px * var(--scale, 1)); text-transform:uppercase; letter-spacing:0.05em; }
  .close { background:none; border:none; color:var(--text-dim); cursor:pointer; padding:2px 8px; font-family:var(--font-ui); font-size:calc(16px * var(--scale, 1)); line-height:1; }
  .close:hover { color:var(--accent); }
  .content { flex:1; overflow:auto; padding:12px; margin:0; background:var(--bg-code); color:var(--text); font-family:var(--font-mono); font-size:calc(12px * var(--scale, 1)); white-space:pre; }
  .content.wrap { white-space:pre-wrap; word-break:break-word; overflow-wrap:anywhere; }
  .live-content { flex:1; overflow:auto; padding:8px 12px; }
  .sa-text { color:var(--text); font-family:var(--font-mono); font-size:calc(12px * var(--scale, 1)); white-space:pre-wrap; word-break:break-word; padding:4px 0; }
</style>
