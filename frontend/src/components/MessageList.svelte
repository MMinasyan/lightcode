<script>
  import { afterUpdate, createEventDispatcher } from 'svelte';
  import Message from './Message.svelte';
  import ToolCall from './ToolCall.svelte';
  export let messages = [];
  export let busy = false;
  export let compacting = false;
  export let messageQueue = [];
  const dispatch = createEventDispatcher();

  let el;
  let userScrolled = false;

  function onScroll() {
    if (!el) return;
    userScrolled = el.scrollHeight - el.scrollTop - el.clientHeight > 50;
  }

  afterUpdate(() => { if (!userScrolled && el) el.scrollTop = el.scrollHeight; });

  $: activityLabel = compacting ? 'Compacting' : busy ? 'Thinking' : '';
</script>

<div class="message-list" bind:this={el} on:scroll={onScroll}>
  {#each messages as msg (msg._id)}
    {#if msg.type === 'user'}
      <Message role="user" content={msg.content} turn={msg.turn}
        on:revertcode={(e) => dispatch('revertcode', e.detail)}
        on:reverthistory={(e) => dispatch('reverthistory', e.detail)}
        on:fork={(e) => dispatch('fork', e.detail)} />
    {:else if msg.type === 'assistant'}
      <Message role="assistant" content={msg.content} turn={msg.turn} partial={msg.partial} />
    {:else if msg.type === 'tool'}
      <ToolCall name={msg.name} args={msg.args} result={msg.result||''} success={msg.success!==false} done={msg.done} subagentSessionIds={msg.subagentSessionIds || []} />
    {:else if msg.type === 'system'}
      <div class="system-msg">{msg.content}</div>
    {:else if msg.type === 'error'}
      <div class="error-msg">{msg.content}</div>
    {/if}
  {/each}
  {#if activityLabel}
    <div class="activity">{activityLabel}<span class="dots"><span>.</span><span>.</span><span>.</span></span></div>
  {/if}
  {#each messageQueue as qmsg (qmsg._id)}
    <div class="queued-msg">{qmsg.content}</div>
  {/each}
</div>

<style>
  .message-list { flex:1; overflow-y:auto; padding:8px 0; min-width:0; }
  .system-msg { padding:4px 12px 4px 24px; margin:4px 12px; color:var(--text-dim); font-size:calc(13px * var(--scale, 1)); font-style:italic; }
  .error-msg { padding:8px 12px; margin:4px 12px; background:var(--error-soft); border-left:3px solid var(--error); border-radius:var(--radius); color:var(--error); font-size:calc(12px * var(--scale, 1)); white-space:pre-wrap; }
  .activity { padding:6px 12px 6px 24px; color:var(--text-dim); font-family:var(--font-ui); font-size:calc(13px * var(--scale, 1)); }
  .queued-msg { margin:10px 12px; padding:8px 12px; background:var(--bg-user-msg); border:1px solid var(--border); border-radius:var(--radius); font-family:var(--font-ui); font-size:calc(13px * var(--scale, 1)); line-height:1.6; white-space:pre-wrap; word-break:break-word; opacity:0.45; }
  .dots span { animation:blink 1.4s infinite both; }
  .dots span:nth-child(2) { animation-delay:0.2s; }
  .dots span:nth-child(3) { animation-delay:0.4s; }
  @keyframes blink { 0%,80%,100% { opacity:0; } 40% { opacity:1; } }
</style>
