package gateway

import (
	"time"

	"github.com/aniljacobv-lab/centauri/internal/model"
)

// audit enqueues one audit fact for a proxied request. Contract (fixed):
// subject gw:<route>, facet "gateway", OBSERVED / SYSTEM_FEED / confidence 1 /
// source_system GATEWAY. Never blocks: a full buffer drops (and counts) —
// proxying traffic always outranks recording it.
func (g *Gateway) audit(route *Route, method, path string, status int, ms int64,
	keyName, upstreamHost string, bytesOut int64, extra map[string]any) {
	if route.Audit != "meta" {
		return
	}
	v := map[string]any{
		"method":    method,
		"path":      path,
		"status":    status,
		"ms":        ms,
		"key":       keyName,
		"upstream":  upstreamHost,
		"bytes_out": bytesOut,
	}
	for k, x := range extra {
		v[k] = x
	}
	ev := &model.Event{
		Subject: "gw:" + route.Name, Facet: "gateway", Type: model.Observed,
		Value: v, Provenance: model.SystemFeed, Confidence: 1, SourceSystem: "GATEWAY",
	}
	select {
	case g.auditCh <- ev:
	default:
		g.auditDropped.Add(1)
	}
}

// auditLoop is the single writer: it batches queued audit facts into one
// Append per ~1s (or per 100 facts), keeping fsync pressure off the hot path.
// On Stop it drains whatever is queued before exiting.
func (g *Gateway) auditLoop() {
	defer close(g.doneCh)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	var batch []*model.Event
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := g.st.Append(time.Now().UnixMicro(), batch, nil); err != nil {
			g.auditDropped.Add(uint64(len(batch)))
		}
		batch = nil
	}
	drain := func() {
		for {
			select {
			case ev := <-g.auditCh:
				batch = append(batch, ev)
			default:
				return
			}
		}
	}
	for {
		select {
		case ev := <-g.auditCh:
			batch = append(batch, ev)
			if len(batch) >= 100 {
				flush()
			}
		case <-tick.C:
			flush()
		case ack := <-g.flushCh:
			drain()
			flush()
			close(ack)
		case <-g.stopCh:
			drain()
			flush()
			return
		}
	}
}

// Flush forces the audit writer to drain and append everything queued so far,
// returning once the facts are durably in the log. A test hook, also handy
// before reading fresh audit data.
func (g *Gateway) Flush() {
	ack := make(chan struct{})
	select {
	case g.flushCh <- ack:
		<-ack
	case <-g.doneCh: // writer already stopped (Stop drained everything)
	}
}
