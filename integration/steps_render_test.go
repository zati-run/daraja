package integration

import (
	"errors"
	"testing"
)

func TestRenderStepsRequiresProofInputBeforeCommit(t *testing.T) {
	op := Operation{
		Effect:   EffectWrite,
		Response: ResponseSpec{Identifier: "id", Extract: map[string]Extract{"id": {Text: "{{input.title}}"}}},
		Steps:    []Step{{Fill: "#title", Value: "{{input.title}}"}, {Click: "#submit", Commit: true}},
	}
	if steps, err := op.RenderSteps(Vars{}); !errors.Is(err, ErrAbsent) || steps != nil {
		t.Fatalf("missing proof: steps=%v err=%v", steps, err)
	}
	steps, err := op.RenderSteps(Vars{Input: map[string]any{"title": "hello"}})
	if err != nil || len(steps) != 2 || steps[0].Value != "hello" {
		t.Fatalf("present proof: steps=%v err=%v", steps, err)
	}
	op.Steps = []Step{{Fill: "#optional", Value: "{{input.optional}}"}, {Click: "#submit", Commit: true}, {Fill: "#title", Value: "{{input.title}}"}}
	steps, err = op.RenderSteps(Vars{})
	if err != nil || len(steps) != 1 || !steps[0].Commit {
		t.Fatalf("unrelated and post-commit optional values: steps=%v err=%v", steps, err)
	}
}
