package main

import "testing"

func TestProposalHookFlagSpawnsWithoutTranscript(t *testing.T) {
	t.Setenv("MEM_DISTILL_SPAWN", "1")
	t.Setenv("MEM_PROPOSAL_HOOK", "1")
	var called bool
	prev := startDistillProc
	startDistillProc = func(db, session string) error {
		called = true
		return nil
	}
	t.Cleanup(func() { startDistillProc = prev })

	maybeSpawnDistill("test.db", hookInput{SessionID: "s-proposal"})
	if !called {
		t.Fatal("MEM_PROPOSAL_HOOK=1 时 Stop 应拉起 Proposal，而不是被 transcript_path 挡住")
	}
}

func TestProposalHookPendingSpawnsOnSessionStart(t *testing.T) {
	t.Setenv("MEM_PROPOSAL_HOOK", "1")
	var called bool
	prev := startDistillProc
	startDistillProc = func(db, session string) error {
		called = true
		return nil
	}
	t.Cleanup(func() { startDistillProc = prev })

	distillPendingFailOpen("test.db", "-")
	if !called {
		t.Fatal("MEM_PROPOSAL_HOOK=1 时 SessionStart 应拉起 Proposal pending")
	}
}
