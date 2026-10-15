// One opened native connection: generation stamp, the frozen generation
// handle and the single pull loop. Opening again supersedes the previous
// generation; the caller repairs by reopening this same owner. Side-effect free.
import { NextEvent, Open } from '../../client-bindings/wailsjs/go/desktopclient/Host';
import type { desktopclient } from '../../client-bindings/wailsjs/go/models';
import type { Event } from '../generated/protocol/types.gen';
import { validateRevisionHint } from './projection';
import type { InvocationClient } from './results';

export type RuntimeEventHandler = (event: Event) => void;
// The failure carries the generation that died, so a superseded loss is subsumed.
export type RuntimeFailureHandler = (error: unknown, generation: string) => void;

export class RuntimeConnection {
  private infoValue: desktopclient.ConnectionInfo | null = null;
  private clientValue: InvocationClient | null = null;
  private handler: RuntimeEventHandler = () => {};
  private failureHandler: RuntimeFailureHandler = () => {};
  private openAttempt = 0;
  private disposedValue = false;

  getInfo(): desktopclient.ConnectionInfo | null {
    return this.infoValue;
  }

  getClient(): InvocationClient {
    if (this.disposedValue || this.clientValue === null) {
      throw new Error('runtime connection is not open');
    }
    return this.clientValue;
  }

  setEventHandler(handler: RuntimeEventHandler): void {
    this.handler = handler;
  }

  // setFailureHandler receives every terminal delivery failure; disposal and
  // supersession never signal it.
  setFailureHandler(handler: RuntimeFailureHandler): void {
    this.failureHandler = handler;
  }

  // Whether a callback's generation is the one currently presented.
  isCurrentGeneration(generation: string): boolean {
    return !this.disposedValue && this.infoValue !== null && this.infoValue.generation === generation;
  }

  async open(): Promise<desktopclient.ConnectionInfo> {
    if (this.disposedValue) {
      throw new Error('runtime connection is disposed');
    }
    const attempt = ++this.openAttempt;
    // The installed connection retires at open start; a superseded attempt installs nothing.
    this.infoValue = null;
    this.clientValue = null;
    const info = await Open();
    if (this.disposedValue) {
      throw new Error('runtime connection open disposed');
    }
    if (attempt !== this.openAttempt) {
      throw new Error('runtime connection open superseded');
    }
    this.infoValue = info;
    // The handle is frozen to THIS generation; a retained handle never sends a newer stamp.
    this.clientValue = Object.freeze({ generation: info.generation });
    this.pull(info);
    return info;
  }

  // The single pull loop: one outstanding NextEvent, stopped by disposal,
  // supersession or terminal failure. Ownership is the installed info object.
  private async pull(info: desktopclient.ConnectionInfo): Promise<void> {
    while (!this.disposedValue && this.infoValue === info) {
      try {
        const raw = await NextEvent(info.generation);
        if (this.disposedValue || this.infoValue !== info) {
          return;
        }
        const event = JSON.parse(raw) as Event;
        // A delivered revision hint is validated before dispatch; a malformed,
        // out-of-range or foreign hint is an invalid delivery.
        if (event.kind === 'session_changed') {
          validateRevisionHint('session', event.session_revision, info.instance_id);
        } else if (event.kind === 'configuration_changed') {
          validateRevisionHint('configuration', event.configuration_revision, info.instance_id);
        } else if (event.kind === 'warning_changed') {
          validateRevisionHint('warning', event.warnings_revision, info.instance_id);
        }
        this.handler(event);
      } catch (error) {
        if (!this.disposedValue && this.infoValue === info) {
          this.failureHandler(error, info.generation);
        }
        return;
      }
    }
  }

  dispose(): void {
    this.disposedValue = true;
  }
}
