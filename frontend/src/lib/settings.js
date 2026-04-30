import { writable } from 'svelte/store';

const KEY = 'lightcode.settings';

const defaults = { wrapCode: false, fontScale: 100, viewerFraction: 0.5 };

function load() {
  try {
    const raw = localStorage.getItem(KEY);
    if (raw) return { ...defaults, ...JSON.parse(raw) };
  } catch (e) {}
  return { ...defaults };
}

export const settings = writable(load());

settings.subscribe((v) => {
  try { localStorage.setItem(KEY, JSON.stringify(v)); } catch (e) {}
});
