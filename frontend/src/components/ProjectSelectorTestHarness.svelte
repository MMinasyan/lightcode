<script>
  // Test-only harness for ProjectSelector.svelte — same shape as SessionSelectorTestHarness: Svelte 5 removed $set, so a live legacy component's props only move when its parent re-renders. The generation and presented-session values arrive as writable stores owned by the test (auto-subscribed below), which the test updates between "switch call issued" and "rejection settles", exactly like App.svelte's applySnapshot would in production. Everything under test stays ProjectSelector's own code; it must never touch SessionCurrent, so no session-identity binding is stubbed for it to reach here either — only what this component imports.
  import { createEventDispatcher } from 'svelte';
  import ProjectSelector from './ProjectSelector.svelte';

  const dispatch = createEventDispatcher();

  export let genStore; // writable<number> owned by the test — presentation generation
  export let sessStore; // writable<string> owned by the test — presented session id
  export let seeded = true; // forwarded as App would (snapshotApplied); ProjectSelector settles on generation+session, so this only documents the shared three-prop interface
</script>

<ProjectSelector {seeded} currentSessionId={$sessStore} generation={$genStore} on:switched={() => dispatch('switched')} on:close={() => dispatch('close')} on:error={(e) => dispatch('error', e.detail)} />
