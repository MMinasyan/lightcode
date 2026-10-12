// RuntimeConnection is one opened native connection: its generation stamp,
// the generated SDK client frozen to that connection, and the single pull
// loop. Opening again supersedes the previous generation; disposal and
// terminal stream failure stop the loop, and the caller repairs by reopening
// through the same owner and rehydrating before rendering further
// authoritative state. This module is side-effect free: the boot/mount entry
// lives only in main.ts.
import { NextEvent, Open, Request } from '../../client-bindings/wailsjs/go/desktopclient/Host';
import type { desktopclient } from '../../client-bindings/wailsjs/go/models';
import type { Client } from '../generated/protocol/client';
import type { Event } from '../generated/protocol/types.gen';
import { validateRevisionHint } from './projection';
import { createRuntimeClient } from './transport';

export type RuntimeEventHandler = (event: Event) => void;
// The failure carries the generation whose pull/establishment died, so the
// caller can tell a loss of the CURRENTLY installed generation (must be
// replaced) from a loss already superseded by a newer Open (subsumed).
export type RuntimeFailureHandler = (error: unknown, generation: string) => void;

// hintRevisionOf reads only the revision scalar of a revision-hint event (the
// three hint kinds), so the boundary validates the revision and never the rest
// of the event or DTO. Non-hint events have no revision to validate.
function hintRevisionOf(event: Event):
  | { kind: 'session' | 'configuration' | 'warning'; revision: unknown }
  | null {
  if (event.kind === 'session_changed') {
    return { kind: 'session', revision: event.session_revision };
  }
  if (event.kind === 'configuration_changed') {
    return { kind: 'configuration', revision: event.configuration_revision };
  }
  if (event.kind === 'warning_changed') {
    return { kind: 'warning', revision: event.warnings_revision };
  }
  return null;
}

export class RuntimeConnection {
  private infoValue: desktopclient.ConnectionInfo | null = null;
  private generationValue = '';
  private clientValue: Client | null = null;
  private handler: RuntimeEventHandler = () => {};
  private failureHandler: RuntimeFailureHandler = () => {};
  private openAttempt = 0;
  private disposedValue = false;
  private failureValue: unknown = null;

  getInfo(): desktopclient.ConnectionInfo | null {
    return this.infoValue;
  }

  getClient(): Client {
    if (this.disposedValue || this.clientValue === null) {
      throw new Error('runtime connection is not open');
    }
    return this.clientValue;
  }

  isDisposed(): boolean {
    return this.disposedValue;
  }

  lastFailure(): unknown {
    return this.failureValue;
  }

  setEventHandler(handler: RuntimeEventHandler): void {
    this.handler = handler;
  }

  // setFailureHandler receives every terminal delivery failure: a native
  // pull rejection, a malformed event document, or an event handler throw.
  // The caller repairs by reopening through this same owner; disposal and
  // supersession never signal it.
  setFailureHandler(handler: RuntimeFailureHandler): void {
    this.failureHandler = handler;
  }

  // isCurrentGeneration reports whether a callback's generation is the one the
  // connection currently presents; a superseded pull's failure is subsumed.
  isCurrentGeneration(generation: string): boolean {
    return !this.disposedValue && this.generationValue === generation;
  }

  async open(): Promise<desktopclient.ConnectionInfo> {
    if (this.disposedValue) {
      throw new Error('runtime connection is disposed');
    }
    const attempt = ++this.openAttempt;
    // The previous pull generation retires at open start, before the awaited
    // native open completes.
    this.generationValue = '';
    const info = await Open();
    if (this.disposedValue) {
      throw new Error('runtime connection open disposed');
    }
    if (attempt !== this.openAttempt) {
      throw new Error('runtime connection open superseded');
    }
    this.infoValue = info;
    this.generationValue = info.generation;
    this.failureValue = null;
    // The generated client is frozen to THIS connection's generation: a
    // retained handle never sends a newer stamp; the native host rejects it
    // as stale until the caller adopts the new client. The generated
    // Request binding matches the bridge function type directly.
    this.clientValue = createRuntimeClient(Request, info.generation);
    this.pull(info.generation);
    return info;
  }

  // The single pull loop: one outstanding NextEvent at a time, stopped by
  // disposal, supersession or a terminal delivery failure. ONE failure
  // boundary encloses the native pull, the post-await supersession guard, the
  // JSON parse, the revision-hint validation and the dispatch; it retains the
  // current-generation check, error recording, callback and return — no
  // error-policy change.
  private async pull(generation: string): Promise<void> {
    while (!this.disposedValue && this.generationValue === generation) {
      try {
        const raw = await NextEvent(generation);
        if (this.disposedValue || this.generationValue !== generation) {
          return;
        }
        const event = JSON.parse(raw) as Event;
        // A delivered revision-hint scalar is validated before dispatch: a
        // current malformed/out-of-range/foreign hint is an invalid delivery
        // and requests repair, even before a reader exists or when its Session
        // is unselected. A superseded delivery never reaches here.
        const hint = hintRevisionOf(event);
        if (hint !== null) {
          validateRevisionHint(hint.kind, hint.revision as never, this.infoValue?.instance_id ?? '');
        }
        this.handler(event);
      } catch (error) {
        if (!this.disposedValue && this.generationValue === generation) {
          this.failureValue = error;
          this.failureHandler(error, generation);
        }
        return;
      }
    }
  }

  dispose(): void {
    this.disposedValue = true;
  }
}
