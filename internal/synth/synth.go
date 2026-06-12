// Package synth generates FD-shaped synthetic price events: intents
// fanning out to four facets, with deliberately seeded wedges (PDT
// activations that lag or never happen), shelf-print failures, and a
// penny markdown — so every signature query has something to find.
// All data is synthetic; no client data is ever used.
package synth

import (
	"fmt"
	"math/rand"
	"time"

	"github.com/proxima360/centauri/internal/model"
	"github.com/proxima360/centauri/internal/store"
)

var facets = []string{"register", "pdt", "shelf", "storecentral"}

// Seed populates the store with nSKUs x nStores subjects over months of
// price-change history. Returns a count summary.
func Seed(st *store.Store, nSKUs, nStores, nChanges int, rng *rand.Rand) (map[string]int, error) {
	start := time.Now().AddDate(0, -6, 0)
	wedges, fails := 0, 0

	for sku := 1; sku <= nSKUs; sku++ {
		for stN := 1; stN <= nStores; stN++ {
			subject := fmt.Sprintf("item:%06d/store:%04d", 100000+sku, 4000+stN)
			price := 200 + rng.Intn(800) // cents

			// ~20%% of subjects are slow movers: their last price change
			// happened months ago, so their wedges age realistically.
			myChanges := nChanges
			if rng.Intn(5) == 0 {
				myChanges = 1 + rng.Intn(2)
			}
			for c := 0; c < myChanges; c++ {
				effective := start.AddDate(0, 0, c*45+rng.Intn(10))
				if effective.After(time.Now().AddDate(0, 0, 14)) {
					break
				}
				price += rng.Intn(101) - 40 // drift
				if price < 100 {
					price = 100
				}
				kind := "REGULAR"
				// One in 40 changes is a penny markdown.
				if rng.Intn(40) == 0 {
					price = 1
					kind = "PENNY"
				}
				batch := fmt.Sprintf("sendnow:BATCH-%05d", rng.Intn(99999))
				jobRun := fmt.Sprintf("automic:RUN-%06d", rng.Intn(999999))

				intent := &model.Event{
					Subject: subject, Facet: "source", Type: model.Intent,
					Value:         map[string]any{"price_cents": price, "kind": kind},
					EffectiveTime: effective.UnixMicro(),
					Provenance:    model.SystemFeed, Confidence: 1.0,
					SourceSystem: "RMS", SourceRef: jobRun,
				}
				intent.EventID = model.NewID()

				var events []*model.Event
				var links []model.CausalLink
				events = append(events, intent)

				for _, fc := range facets {
					// ~3% of shelf distributions fail entirely (the RRD path).
					if fc == "shelf" && rng.Intn(33) == 0 {
						fails++
						continue
					}
					d := &model.Event{
						Subject: subject, Facet: fc, Type: model.Distributed,
						Value:         map[string]any{"price_cents": price, "kind": kind},
						EffectiveTime: effective.UnixMicro(),
						Provenance:    model.SystemFeed, Confidence: 1.0,
						SourceSystem:  pipelineFor(fc), SourceRef: batch,
					}
					d.EventID = model.NewID()
					events = append(events, d)
					links = append(links, model.CausalLink{From: intent.EventID, To: d.EventID, Type: model.DistributedAs})
				}

				// RecordedTime: pretend ingest happened 1 day before effective.
				recorded := effective.AddDate(0, 0, -1).UnixMicro()
				if err := st.Append(recorded, events, links); err != nil {
					return nil, err
				}

				// Activations: register activates fast (early-activation
				// window); PDT lags 0-28 days — and ~12% never activate
				// (the wedge); shelf+storecentral activate within a week.
				for _, e := range events {
					if e.Type != model.Distributed {
						continue
					}
					switch e.Facet {
					case "register":
						_ = st.Activate(e.EventID, effective.UnixMicro())
					case "pdt":
						if rng.Intn(100) < 12 {
							wedges++ // never activated -> stays pending
							continue
						}
						lag := time.Duration(rng.Intn(28*24)) * time.Hour
						at := effective.Add(lag)
						if at.Before(time.Now()) {
							_ = st.Activate(e.EventID, at.UnixMicro())
						} else {
							wedges++
						}
					default:
						_ = st.Activate(e.EventID, effective.Add(time.Duration(rng.Intn(7*24))*time.Hour).UnixMicro())
					}
				}
			}
		}
	}
	stats := st.Stats()
	stats["seeded_wedges"] = wedges
	stats["seeded_shelf_failures"] = fails
	return stats, nil
}

func pipelineFor(facet string) string {
	switch facet {
	case "register":
		return "SENDNOW"
	case "pdt":
		return "PDT_FEED"
	case "shelf":
		return "RRD_PRINT"
	default:
		return "STORE_CENTRAL"
	}
}
