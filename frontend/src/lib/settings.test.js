import { get } from 'svelte/store';
import { afterEach, describe, expect, it, vi } from 'vitest';

const KEY = 'lightcode.settings';

async function loadSettings(initialStore = {}) {
  vi.resetModules();
  const backing = { ...initialStore };
  vi.stubGlobal('localStorage', {
    getItem: vi.fn((key) => (key in backing ? backing[key] : null)),
    setItem: vi.fn((key, value) => {
      backing[key] = value;
    }),
  });
  const mod = await import('./settings.js');
  return { settings: mod.settings, backing };
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('settings store', () => {
  it('loads default values when storage is empty', async () => {
    const { settings } = await loadSettings();
    expect(get(settings)).toEqual({ wrapCode: false, fontScale: 100, viewerFraction: 0.5 });
  });

  it('merges partial stored settings with defaults', async () => {
    const { settings } = await loadSettings({ [KEY]: JSON.stringify({ wrapCode: true }) });
    expect(get(settings)).toEqual({ wrapCode: true, fontScale: 100, viewerFraction: 0.5 });
  });

  it('falls back to defaults for corrupt stored JSON', async () => {
    const { settings } = await loadSettings({ [KEY]: '{not json' });
    expect(get(settings)).toEqual({ wrapCode: false, fontScale: 100, viewerFraction: 0.5 });
  });

  it('publishes updates through the Svelte store', async () => {
    const { settings } = await loadSettings();
    settings.update((current) => ({ ...current, wrapCode: true }));
    expect(get(settings).wrapCode).toBe(true);
  });

  it('persists updates to localStorage', async () => {
    const { settings, backing } = await loadSettings();
    settings.set({ wrapCode: true, fontScale: 125, viewerFraction: 0.75 });
    expect(JSON.parse(backing[KEY])).toEqual({ wrapCode: true, fontScale: 125, viewerFraction: 0.75 });
  });
});
