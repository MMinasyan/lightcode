import { defineConfig } from 'vite';
import { svelte } from '@sveltejs/vite-plugin-svelte';

// The target client build: the same Svelte/Vite toolchain as the default
// build, over the separate client entry. The root is the client directory so
// its index.html is the build's default entry and the emitted HTML sits at
// the dist-client root with /assets beside it — the exact layout Wails'
// asset handler serves from a directory FS.
export default defineConfig({
  root: 'src/client',
  plugins: [svelte()],
  build: {
    outDir: '../../dist-client',
    emptyOutDir: true,
  },
});
