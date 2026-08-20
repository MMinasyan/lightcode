<script>
  // Test-only harness for SessionSelector.svelte. Svelte 5 removed $set, so the only way to move a prop on a live legacy component is through its parent re-rendering: this wrapper owns nothing itself — it just forwards three presentation values down and relays events up. The generation arrives as a writable store from the test (auto-subscribed below), which the test updates between "mutation call issued" and "rejection settles", exactly like App.svelte's applySnapshot moving presentationGeneration in production. Everything under test stays SessionSelector's own code.
  import { createEventDispatcher } from 'svelte';
  import SessionSelector from './SessionSelector.svelte';

  const dispatch = createEventDispatcher();

  export let genStore; // writable<number> owned by the test — presentation generation
  export let seeded = false;
  export let currentSessionId = '';
</script>

<SessionSelector {seeded} {currentSessionId} generation={$genStore} on:close={() => dispatch('close')} on:error={(e) => dispatch('error', e.detail)} />
