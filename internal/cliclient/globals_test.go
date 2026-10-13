package cliclient

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/clienthost"
	"github.com/MMinasyan/lightcode/protocol"
)

func configHint(n string) protocol.Event {
	var scope protocol.Scope
	_ = scope.FromRuntimeScope(protocol.RuntimeScope{Kind: "runtime"})
	var event protocol.Event
	_ = event.FromConfigurationChangedEvent(protocol.ConfigurationChangedEvent{Kind: "configuration_changed", Scope: scope, ConfigurationRevision: configuration(n).ConfigurationRevision})
	return event
}
func warningHint(n string) protocol.Event {
	var scope protocol.Scope
	_ = scope.FromRuntimeScope(protocol.RuntimeScope{Kind: "runtime"})
	var event protocol.Event
	_ = event.FromWarningChangedEvent(protocol.WarningChangedEvent{Kind: "warning_changed", Scope: scope, WarningsRevision: warnings(n).WarningsRevision})
	return event
}
func TestGlobalsNoReaderCoalescesAndCoveredHintsDoNotRead(t *testing.T) {
	c, transport := fixture(t)
	transport.globals = true
	c.acceptEvent(eventDelivery{c.conn, configHint("5")})
	parked := receive(t, transport.requests)
	for i := 0; i < 1000; i++ {
		c.acceptEvent(eventDelivery{c.conn, configHint("6")})
		c.acceptEvent(eventDelivery{c.conn, warningHint("6")})
	}
	c.mu.Lock()
	if !c.globalsBusy || !c.globalsDirty || c.root != nil {
		t.Fatal("hints created an unintended Session reader")
	}
	c.mu.Unlock()
	config := configuration("6")
	config.Agents[0].Name = "payload-six"
	parked.respond(config)
	warn := warnings("6")
	warn.Warnings = []protocol.Warning{{Message: "payload-six"}}
	receive(t, transport.requests).respond(warn)
	receive(t, transport.requests).respond(config)
	receive(t, transport.requests).respond(warn)
	settleTasks(t, c)
	transport.mu.Lock()
	before := len(transport.calls)
	transport.mu.Unlock()
	for i := 0; i < 1000; i++ {
		c.acceptEvent(eventDelivery{c.conn, configHint("6")})
		c.acceptEvent(eventDelivery{c.conn, warningHint("5")})
	}
	c.mu.Lock()
	if c.globalsBusy || c.globalsDirty || c.configuration.Agents[0].Name != "payload-six" || c.warnings.Warnings[0].Message != "payload-six" {
		t.Fatal("covered hints refreshed or payload not installed atomically")
	}
	c.mu.Unlock()
	transport.mu.Lock()
	after := len(transport.calls)
	transport.mu.Unlock()
	if before != 4 || after != before {
		t.Fatalf("refresh count %d -> %d", before, after)
	}
}

func TestGlobalClockPayloadNonregressionAndHydrationFloors(t *testing.T) {
	c, transport := fixture(t)
	transport.globals = true
	c.mu.Lock()
	c.configuration = pointer(configuration("9"))
	c.warnings = pointer(warnings("9"))
	c.configuration.Agents[0].Name = "accepted-nine"
	c.globalsDirty = true
	c.kickGlobalsLocked()
	c.mu.Unlock()
	receive(t, transport.requests).respond(configuration("8"))
	receive(t, transport.requests).respond(warnings("8"))
	configReq := receive(t, transport.requests)
	c.mu.Lock()
	if c.configuration.Agents[0].Name != "accepted-nine" || c.warnings.WarningsRevision.Revision != "9" {
		t.Fatal("older payload replaced accepted clock")
	}
	c.mu.Unlock()
	configReq.respond(configuration("10"))
	receive(t, transport.requests).respond(warnings("10"))
	settleTasks(t, c)
	r, done := c.startReader()
	stale := hydration(rootA, "1", inputs(1, 1), nil)
	stale.ConfigurationRevision = configuration("9").ConfigurationRevision
	stale.WarningsRevision = warnings("9").WarningsRevision
	receive(t, transport.requests).respond(stale)
	fresh := receive(t, transport.requests)
	c.mu.Lock()
	if r.hydration != nil {
		t.Fatal("hydration installed below independent floors")
	}
	c.mu.Unlock()
	stale.ConfigurationRevision = configuration("10").ConfigurationRevision
	stale.WarningsRevision = warnings("10").WarningsRevision
	fresh.respond(stale)
	barrier(t, done)
	// Equal Session clocks do not suppress independent configuration/warnings.
	done = c.refreshReader(r)
	stale.ConfigurationRevision = configuration("11").ConfigurationRevision
	receive(t, transport.requests).respond(stale)
	barrier(t, done)
	c.mu.Lock()
	if r.hydration.ConfigurationRevision.Generation != "11" || r.hydration.WarningsRevision.Revision != "10" {
		t.Fatal("configuration-only delivery was suppressed")
	}
	c.mu.Unlock()
	done = c.refreshReader(r)
	stale.WarningsRevision = warnings("11").WarningsRevision
	receive(t, transport.requests).respond(stale)
	barrier(t, done)
	c.mu.Lock()
	defer c.mu.Unlock()
	if r.hydration.ConfigurationRevision.Generation != "11" || r.hydration.WarningsRevision.Revision != "11" {
		t.Fatal("equal Session discarded independent clocks")
	}
}

func TestGlobalsSupersededSelectionIgnoresSuccessAndErrors(t *testing.T) {
	c, transport := fixture(t)
	transport.globals = true
	c.mu.Lock()
	c.globalsDirty = true
	c.kickGlobalsLocked()
	c.mu.Unlock()
	oldConfig := receive(t, transport.requests)
	oldConfig.respond(configuration("900"))
	oldWarning := receive(t, transport.requests)
	c.selectRoot(rootB, false)
	rootReq := receive(t, transport.requests)
	rootReq.respond(hydration(rootB, "1", nil, nil))
	oldWarning.lost()
	receive(t, transport.requests).respond(configuration("1"))
	receive(t, transport.requests).respond(warnings("1"))
	settleTasks(t, c)
	c.mu.Lock()
	if c.configuration.ConfigurationRevision.Generation != "1" || c.warnings.WarningsRevision.Revision != "1" || c.root.hydration == nil {
		t.Fatal("superseded global success poisoned hydration")
	}
	c.mu.Unlock()
	if strings.Contains(outputs(c), "unexpected EOF") {
		t.Fatal("superseded global error printed")
	}
}

func TestConnectionRepairClearsOldFloorsBeforeNewOwnerHydration(t *testing.T) {
	for _, sameInstance := range []bool{true, false} {
		t.Run(map[bool]string{true: "same-instance", false: "new-instance"}[sameInstance], func(t *testing.T) {
			c, transport := fixture(t)
			settledReader(t, c, transport)
			transport.globals = true
			c.mu.Lock()
			c.configuration = pointer(configuration("900"))
			c.warnings = pointer(warnings("900"))
			c.provisional = "old activity"
			c.globalsDirty = true
			c.kickGlobalsLocked()
			oldConn := c.conn
			c.mu.Unlock()
			receive(t, transport.requests).respond(configuration("901"))
			oldWarning := receive(t, transport.requests)
			owner := instance
			if !sameInstance {
				owner = "ffffffffffffffffffffffffffffffff"
			}
			requests := make(chan request, 16)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/health" {
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(protocol.Health{InstanceId: owner, ProtocolVersion: protocol.N1})
					return
				}
				if r.URL.Path == "/v1/events" {
					w.Header().Set("Content-Type", "text/event-stream")
					w.WriteHeader(200)
					w.(http.Flusher).Flush()
					<-r.Context().Done()
					return
				}
				req := request{req: r, reply: make(chan reply, 1)}
				requests <- req
				select {
				case res := <-req.reply:
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(res.status)
					_ = json.NewEncoder(w).Encode(res.value)
				case <-r.Context().Done():
				}
			}))
			defer server.Close()
			dir := t.TempDir()
			raw, _ := json.Marshal(map[string]string{"endpoint": server.URL, "instance_id": owner, "protocol_version": string(protocol.N1), "credential": strings.Repeat("1", 64)})
			if err := os.WriteFile(filepath.Join(dir, "runtime.json"), raw, 0600); err != nil {
				t.Fatal(err)
			}
			c.mu.Lock()
			c.opts.Startup = clienthost.Options{DataDir: dir}
			c.mu.Unlock()
			c.task(func() { c.connect(true, oldConn) })
			var sessionReq, listReq request
			for i := 0; i < 2; i++ {
				req := receive(t, requests)
				if strings.Contains(req.req.URL.Path, "hydration") {
					sessionReq = req
				} else {
					listReq = req
				}
			}
			c.mu.Lock()
			if c.configuration != nil || c.warnings != nil || c.provisional != "" || c.root.floor != nil {
				t.Fatal("retirement kept old projections/floors/activity")
			}
			newReader := c.root
			newDone := newReader.chain.done
			c.mu.Unlock()
			fresh := hydration(rootA, "1", inputs(1, 1), nil)
			fresh.SessionRevision.InstanceId = owner
			fresh.Session.SessionRevision.InstanceId = owner
			fresh.ConfigurationRevision.InstanceId = owner
			fresh.WarningsRevision.InstanceId = owner
			sessionReq.respond(fresh)
			listReq.respond([]protocol.Session{{SessionId: rootA}})
			barrier(t, newDone)
			oldWarning.lost()
			cfg := configuration("1")
			cfg.ConfigurationRevision.InstanceId = owner
			warn := warnings("1")
			warn.WarningsRevision.InstanceId = owner
			receive(t, requests).respond(cfg)
			receive(t, requests).respond(warn)
			// The SSE task is deliberately live. A protocol barrier (a second
			// explicit refresh) proves the previous global installation completed.
			c.mu.Lock()
			c.globalsDirty = true
			c.kickGlobalsLocked()
			c.mu.Unlock()
			receive(t, requests).respond(cfg)
			receive(t, requests).respond(warn)
			c.mu.Lock()
			accepted := c.root.hydration != nil && c.conn != oldConn
			c.closed = true
			c.cancel()
			if c.connCancel != nil {
				c.connCancel()
			}
			c.mu.Unlock()
			settleTasks(t, c)
			if !accepted || strings.Contains(outputs(c), "unexpected EOF") {
				t.Fatal("old independent read failure poisoned new owner")
			}
		})
	}
}
