package runtime

import (
	"encoding/json"
	"net"
	"net/http"
	"time"

	"github.com/MMinasyan/lightcode/protocol"
)

// The one multiplexed events stream: every authenticated GET /v1/events owns
// one capacity-256 subscription and one frame writer that serializes
// notification frames and 15-second keepalive comments. A slow reader blocks
// only its own writer; when its bounded queue overflows, the observation bus
// removes that subscription alone, and the stream's private watcher closes
// exactly its native connection — even while a socket write blocks — so the
// client reconnects and rehydrates from authoritative reads. A disconnect
// releases the stream's delivery resources and never cancels committed
// Agent or Job work; owner shutdown exits the writer so its admitted call
// converges.
const (
	sseSubscriptionCapacity = 256
	sseHeartbeatInterval    = 15 * time.Second
)

// GetEvents serves the one authenticated event stream. The whole stream
// lifetime is one admitted call, so shutdown joins it after the server's
// connections close; the writer selects on the owner's cancellation, the
// caller's disconnect, the subscription's closure, and the heartbeat clock.
func (rt *protocolHandlers) GetEvents(w http.ResponseWriter, r *http.Request) {
	release, err := rt.enter(r.Context())
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	defer release()
	sub, err := rt.obs.subscribe(sseSubscriptionCapacity)
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	// The subscription releases on every exit: client disconnect, queue
	// closure, owner cancellation, or write failure.
	defer sub.Close()

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}

	// The per-stream watcher unblocks a writer stuck in a socket write when
	// its subscription closes — saturation or Runtime cleanup — by closing
	// only this stream's native connection. It writes no frame and touches
	// no work.
	watcherDone := make(chan struct{})
	stop := make(chan struct{})
	if conn, ok := r.Context().Value(sseConnectionKey{}).(net.Conn); ok && conn != nil {
		go func() {
			defer close(watcherDone)
			select {
			case <-sub.closed:
				_ = conn.Close()
			case <-stop:
			}
		}()
		defer func() {
			close(stop)
			<-watcherDone
		}()
	}

	heartbeat := time.NewTicker(sseHeartbeatInterval)
	defer heartbeat.Stop()
	instance := rt.protocolInstance()
	events := sub.Events()
	for {
		select {
		case event, ok := <-events:
			if !ok {
				// The subscription was removed — saturation or cleanup: this
				// stream ends alone and the client reconnects.
				return
			}
			if err := writeEventFrame(w, qualifyEvent(event, instance)); err != nil {
				return
			}
		case <-heartbeat.C:
			if _, err := w.Write([]byte(": keepalive\n\n")); err != nil {
				return
			}
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		case <-r.Context().Done():
			return
		case <-rt.work.Done():
			return
		}
	}
}

// writeEventFrame serializes one notification frame — the closed event
// union's JSON on one data line — and flushes it.
func writeEventFrame(w http.ResponseWriter, event protocol.Event) error {
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	if _, err := w.Write([]byte("event: notification\ndata: " + string(data) + "\n\n")); err != nil {
		return err
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	return nil
}
