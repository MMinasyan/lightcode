// The single target boot/mount entry: it constructs the one native
// connection and mounts the target App on it. The connection class itself is
// side-effect free and lives in ./connection; no component imports this
// module, so importing UI code can never boot a second connection or App.
import '../styles/global.css';
import { mount } from 'svelte';
import App from './App.svelte';
import { RuntimeConnection } from './connection';

if (typeof window !== 'undefined') {
  try {
    const target = document.getElementById('app');
    if (target === null) {
      throw new Error('lightcode: desktop bootstrap: app mount target is missing');
    }
    const connection = new RuntimeConnection();
    mount(App, { target, props: { connection } });
  } catch (error) {
    console.error('lightcode: desktop bootstrap:', error);
  }
}
