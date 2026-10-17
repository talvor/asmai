// SPDX-License-Identifier: Apache-2.0

package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/talvor/asmai/internal/daemon"
)

func TestAttachReturnDiscardsEarlierSnapshotAndRetry(t *testing.T) {
	agent := daemon.DashboardAgent{Agent: "leader@planning", Status: daemon.StatusIdle}
	m := newModel("", Options{Columns: 100, Rows: 30, Refresh: time.Second, StoppedRetry: time.Second})
	m.factory = factory{running: true, agents: []daemon.DashboardAgent{agent}}
	m.read = true

	updated, cmd := m.Update(attachedMsg{})
	m = updated.(model)
	if cmd == nil {
		t.Fatal("attach return did not request a new snapshot")
	}

	updated, cmd = m.Update(readMsg{generation: m.generation})
	m = updated.(model)
	if cmd == nil || !strings.Contains(m.View().Content, "The factory is not running.") {
		t.Fatal("return snapshot did not show the stopped factory and continue polling")
	}

	updated, cmd = m.Update(readMsg{factory: factory{running: true, agents: []daemon.DashboardAgent{agent}}, generation: m.generation - 1})
	m = updated.(model)
	if cmd != nil || !strings.Contains(m.View().Content, "The factory is not running.") || strings.Contains(m.View().Content, agent.Agent) {
		t.Fatal("snapshot started before attach replaced the stopped factory")
	}

	_, cmd = m.Update(retryMsg{generation: m.generation - 1})
	if cmd != nil {
		t.Fatal("retry queued before attach started another read")
	}
	_, cmd = m.Update(retryMsg{generation: m.generation})
	if cmd == nil {
		t.Fatal("current retry did not continue polling")
	}
}
