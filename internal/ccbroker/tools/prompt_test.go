package tools

import (
	"strings"
	"testing"
)

func TestBuildSystemPrompt_PreferredExecutorMarked(t *testing.T) {
	p := BuildSystemPrompt(PromptInput{
		ChannelType:         "tui",
		PreferredExecutorID: "exe_a",
		Executors: []ExecutorInfo{
			{ExecutorID: "exe_a", DisplayName: "Laptop", Type: "local_agent"},
			{ExecutorID: "exe_b", DisplayName: "Sandbox", Type: "sandbox"},
		},
	})
	if !strings.Contains(p, "exe_a") || !strings.Contains(p, "PREFERRED FOR THIS SESSION") {
		t.Errorf("expected preferred marker on exe_a, got:\n%s", p)
	}
	if !strings.Contains(p, "exe_b") {
		t.Errorf("expected non-preferred executor listed, got:\n%s", p)
	}
	if strings.Count(p, "PREFERRED FOR THIS SESSION") != 1 {
		t.Errorf("only one executor should be marked preferred, got %d",
			strings.Count(p, "PREFERRED FOR THIS SESSION"))
	}
	if !strings.Contains(p, "interactive terminal client") {
		t.Errorf("TUI channel hint missing")
	}
}

func TestBuildSystemPrompt_IMChannelHasIMHint(t *testing.T) {
	p := BuildSystemPrompt(PromptInput{
		ChannelType: "im",
		Executors:   []ExecutorInfo{{ExecutorID: "exe_x", DisplayName: "x", Type: "sandbox"}},
	})
	if !strings.Contains(p, "instant messaging") {
		t.Errorf("IM channel hint missing")
	}
	if strings.Contains(p, "PREFERRED FOR THIS SESSION") {
		t.Errorf("IM channel should not have preferred marker")
	}
}

func TestBuildSystemPrompt_NoExecutors(t *testing.T) {
	p := BuildSystemPrompt(PromptInput{ChannelType: "tui"})
	if !strings.Contains(p, "No execution environments") {
		t.Errorf("expected empty-list note, got:\n%s", p)
	}
}
