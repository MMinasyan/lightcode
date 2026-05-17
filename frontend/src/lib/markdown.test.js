import { describe, expect, it } from 'vitest';

import { renderMarkdown } from './markdown.js';

describe('renderMarkdown', () => {
  it('renders headings', () => {
    expect(renderMarkdown('# Hello')).toContain('<h1>Hello</h1>');
  });

  it('renders inline formatting', () => {
    expect(renderMarkdown('**bold**')).toContain('<strong>bold</strong>');
  });

  it('renders fenced code blocks', () => {
    const html = renderMarkdown('```js\nconsole.log("hi")\n```');
    expect(html).toContain('<pre><code class="language-js">');
    expect(html).toContain('console.log(&quot;hi&quot;)');
  });

  it('escapes raw HTML instead of rendering it', () => {
    const html = renderMarkdown('<script>alert("x")</script>');
    expect(html).not.toContain('<script>');
    expect(html).toContain('&lt;script&gt;alert(&quot;x&quot;)&lt;/script&gt;');
  });

  it('returns an empty string for empty input', () => {
    expect(renderMarkdown('')).toBe('');
  });
});
