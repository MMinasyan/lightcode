// @vitest-environment happy-dom
// The composer's input-driven auto-resize keeps its 132px ceiling (the retained
// components/InputArea.svelte behavior); the textarea is focusable and the
// Retry-before-click warning stays beside each attempt.
import { afterEach, describe, expect, it } from 'vitest';
import { mount, tick, unmount } from 'svelte';

const mounted: Array<ReturnType<typeof mount>> = [];

async function mountComposer() {
  const { default: Composer } = await import('./Composer.svelte');
  const target = document.createElement('div');
  document.body.appendChild(target);
  const component = mount(Composer, { target, props: { text: 'x', active: false, hasSession: true, attempts: [] } });
  mounted.push(component);
  await tick();
  return { component, target };
}

afterEach(() => {
  for (const component of mounted.splice(0)) unmount(component);
  document.body.innerHTML = '';
});

describe('Composer auto-size', () => {
  it('auto-sizes on input with a normal height and the 132px ceiling', async () => {
    const { target } = await mountComposer();
    const textarea = target.querySelector('textarea') as HTMLTextAreaElement;
    // Normal case: content shorter than the ceiling.
    Object.defineProperty(textarea, 'scrollHeight', { configurable: true, get: () => 60 });
    textarea.dispatchEvent(new Event('input', { bubbles: true }));
    await tick();
    expect(textarea.style.height).toBe('60px');
    // Taller content clamps at the 132px ceiling.
    Object.defineProperty(textarea, 'scrollHeight', { configurable: true, get: () => 400 });
    textarea.dispatchEvent(new Event('input', { bubbles: true }));
    await tick();
    expect(textarea.style.height).toBe('132px');
  });

  it('is focusable', async () => {
    const { target } = await mountComposer();
    const textarea = target.querySelector('textarea') as HTMLTextAreaElement;
    textarea.focus();
    await tick();
    expect(document.activeElement).toBe(textarea);
  });

  it('shows the duplication warning beside each Retry before it is invoked', async () => {
    const { default: Composer } = await import('./Composer.svelte');
    const target = document.createElement('div');
    document.body.appendChild(target);
    const attempt = {
      id: 'a'.repeat(32), kind: 'submit', mode: 'regular', content: [],
      sessionID: 's1', draftRevision: 1, status: 'unknown', error: null,
    };
    const component = mount(Composer, { target, props: { text: '', active: false, hasSession: true, attempts: [attempt] } });
    mounted.push(component);
    await tick();
    const row = target.querySelector('.attempt.unknown') as HTMLElement;
    // The warning lives inside the row (before any Retry click), not in a
    // transient global notice.
    expect(row.querySelector('.retry-warning')?.textContent).toContain('may be duplicated');
    expect(row.querySelector('.attempt-btn')?.getAttribute('aria-label')).toBe('Retry');
  });
});
