package runtime

import (
	"encoding/json"
	"net"
	"net/http"
	"time"

	"github.com/Syntheresis/melta/protocol"
)

// The one multiplexed events stream: every authenticated GET /v1/events owns
// one bounded subscription and one frame writer that serializes notification
// frames and 15-second keepalive comments. A slow reader blocks only its own
// writer; when its bounded queue overflows, the observation bus removes that
// subscription alone. A disconnect releases the stream's delivery resources
// and never cancels committed Agent or Job work; host release closes the
// connection so the writer exits. The stream holds no owner command-admission
// guard across its wait.
const (
	sseSubscriptionCapacity = 256
	sseHeartbeatInterval    = 15 * time.Second
)

// GetEvents serves the one authenticated event stream.
func (rt *protocolHandlers) GetEvents(w http.ResponseWriter, r *http.Request) {
	sub, err := rt.owner.Subscribe(sseSubscriptionCapacity)
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	defer sub.Close()

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}

	// The per-stream watcher unblocks a writer stuck in a socket write when
	// its subscription closes — saturation or owner cleanup — by closing
	// only this stream's native connection. It writes no frame and touches
	// no work.
	watcherDone := make(chan struct{})
	stop := make(chan struct{})
	if conn, ok := r.Context().Value(sseConnectionKey{}).(net.Conn); ok && conn != nil {
		go func() {
			defer close(watcherDone)
			select {
			case <-sub.Done():
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
	events := sub.Events()
	for {
		select {
		case event, ok := <-events:
			if !ok {
				// The subscription was removed — saturation or cleanup: this
				// stream ends alone and the client reconnects. Closing the
				// native connection here covers the race where the events
				// channel closes while the writer is between frames; the
				// watcher covers a writer blocked inside a socket write.
				if conn, ok := r.Context().Value(sseConnectionKey{}).(net.Conn); ok && conn != nil {
					_ = conn.Close()
				}
				return
			}
			if err := writeEventFrame(w, event); err != nil {
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
