package main

import (
	"strings"
	"testing"

	"github.com/v2camp/v2mem/internal/store"
)

func TestCmdSearchHydratesExternalSummary(t *testing.T) {
	db := testDB(t)
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendObservation(store.ObsInput{SessionID: "s", Event: "stop", Summary: "外置摘要可回填"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.MigrateSummaryExternal(false); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	out, err := captureStdout(t, func() error {
		return cmdSearch([]string{"--db", db, "--no-audit", "--layer", "observation", "外置摘要可回填"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "外置摘要可回填") {
		t.Fatalf("search 应回填外置摘要: %q", out)
	}
}
