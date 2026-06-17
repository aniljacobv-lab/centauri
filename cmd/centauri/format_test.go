package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/proxima360/centauri/internal/model"
)

func sampleEvents() []*model.Event {
	return []*model.Event{
		{Subject: "item:1", Facet: "source", Type: model.Observed,
			Value: map[string]any{"price_cents": float64(500), "kind": "PEN"},
			Confidence: 1, Provenance: model.SystemFeed,
			EffectiveTime: 1000, RecordedTime: 1000, EventID: "e1"},
		{Subject: "item:2", Facet: "shelf", Type: model.Correction,
			Value: map[string]any{"price_cents": float64(750)},
			Confidence: 0.8, Provenance: model.HumanEntry,
			EffectiveTime: 2000, RecordedTime: 2000, EventID: "e2"},
	}
}

func render(t *testing.T, format string) string {
	t.Helper()
	var b bytes.Buffer
	if err := formatEvents(sampleEvents(), format, &b); err != nil {
		t.Fatalf("format %s: %v", format, err)
	}
	return b.String()
}

func TestFormatsAllRender(t *testing.T) {
	for _, f := range []string{"csv", "jsonl", "json", "table", "yaml", "plain"} {
		out := render(t, f)
		if !strings.Contains(out, "item:1") || !strings.Contains(out, "item:2") {
			t.Fatalf("format %s missing subjects:\n%s", f, out)
		}
	}
}

func TestFormatCSVHeader(t *testing.T) {
	if out := render(t, "csv"); !strings.HasPrefix(out, "subject,facet,type") {
		t.Fatalf("csv header wrong:\n%s", out)
	}
}

func TestFormatTableHasColumns(t *testing.T) {
	out := render(t, "table")
	if !strings.Contains(out, "subject") || !strings.Contains(out, "price_cents") {
		t.Fatalf("table missing columns:\n%s", out)
	}
}

func TestFormatUnknown(t *testing.T) {
	var b bytes.Buffer
	if err := formatEvents(sampleEvents(), "xml", &b); err == nil {
		t.Fatal("expected an error for an unknown format")
	}
}
