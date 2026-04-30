import MarkdownIt from 'markdown-it';

const md = new MarkdownIt({ html: false, linkify: false, typographer: false });

export function renderMarkdown(text) {
  return md.render(text);
}
