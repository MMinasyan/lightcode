// Ambient module declarations for the Vite toolchain: stylesheets are
// handled by the bundler, not by the TypeScript compiler.
declare module '*.css';

// JSON.parse source-text access and JSON.rawJSON are ordinary installed-JS
// engine APIs (proven in the native WebKit/Node runtimes); the pinned
// TypeScript lib does not declare them yet, so the exact-JSON editor helper
// declares only the two members it actually calls.
interface JSONRawJSON {
  readonly rawJSON: string;
}

interface JSON {
  rawJSON(text: string): JSONRawJSON;
  // The raw-number predicate the exact-object replacement uses to exclude
  // numeric raw wrappers from object roots.
  isRawJSON(value: unknown): boolean;
  parse(
    text: string,
    reviver: (this: unknown, key: string, value: unknown, context: { source?: string }) => unknown,
  ): unknown;
}
