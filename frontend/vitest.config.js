import { defineConfig } from 'vitest/config';
import { svelte } from '@sveltejs/vite-plugin-svelte';

export default defineConfig({
  plugins: [svelte()],
  // Svelte 5 component tests run in a DOM environment and mount the client
  // build; resolve the `browser` entry points even though the tests run in
  // Node. See https://svelte.dev/docs/svelte/testing
  resolve: {
    conditions: process.env.VITEST ? ['browser'] : undefined,
  },
  test: {
    environment: 'node',
    globals: true,
  },
});
