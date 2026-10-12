// The target desktop bootstrap: one native connection generation, the
// generated SDK client riding the credential-free byte bridge, and the single
// event pull loop over the native generated Go subscription. The next step
// mounts the conversation UI on this bootstrap; this module owns only the
// native connection, transport, event pull, and disposal/repair lifecycle.
import { NextEvent, Open, Request } from '../../client-bindings/wailsjs/go/desktopclient/Host';
import type { desktopclient } from '../../client-bindings/wailsjs/go/models';
import type { Client } from '../generated/protocol/client';
import type { Event } from '../generated/protocol/types.gen';
import { createRuntimeClient } from './transport';

export type RuntimeEventHandler = (event: Event) => void;

// RuntimeConnection is one opened native connection: its generation stamp,
// the generated SDK client frozen to that connection, and the single pull
// loop. Opening again supersedes the previous generation; disposal and
// terminal stream failure stop the loop, and the caller repairs by reopening
// through the same owner and rehydrating before rendering further
// authoritative state.
export class RuntimeConnection {
  private infoValue: desktopclient.ConnectionInfo | null = null;
  private generationValue = '';
  private clientValue: Client | null = null;
  private handler: RuntimeEventHandler = () => {};
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
  // disposal, a terminal stream failure, or supersession.
  private async pull(generation: string): Promise<void> {
    while (!this.disposedValue && this.generationValue === generation) {
      let event: Event;
      try {
        const raw = await NextEvent(generation);
        if (this.disposedValue || this.generationValue !== generation) {
          return;
        }
        event = JSON.parse(raw) as Event;
      } catch (error) {
        if (!this.disposedValue && this.generationValue === generation) {
          this.failureValue = error;
        }
        return;
      }
      this.handler(event);
    }
  }

  dispose(): void {
    this.disposedValue = true;
  }
}

// The preview webview boots the connection over the real native bridge; the
// later step installs its event handler, rehydration and explicit repair on
// this same owner. Node test environments have no window and import only the
// class.
if (typeof window !== 'undefined') {
  const previewConnection = new RuntimeConnection();
  previewConnection.open().catch((error) => {
    console.error('lightcode: desktop bootstrap:', error);
  });
}
