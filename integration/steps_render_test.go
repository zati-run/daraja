package integration

import (
	"errors"
	"fmt"
	"testing"
)

func TestStepsProofRequiresInputUsedBeforeCommit(t *testing.T) {
	op := Operation{
		Effect:   EffectWrite,
		Response: ResponseSpec{Identifier: "id", Extract: map[string]Extract{"id": {CSS: "#id", Text: "{{input.title}}"}}},
		Steps:    []Step{{Click: "#submit", Commit: true}},
	}
	validate := func() []string {
		var problems []string
		new(Compiled).validateStepsProof(&op, "write", func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) })
		return problems
	}
	if len(validate()) == 0 {
		t.Fatal("accepted an anchor that was never entered")
	}
	op.Steps = append([]Step{{Fill: "#title", Value: "{{input.title}}"}}, op.Steps...)
	if problems := validate(); len(problems) != 0 {
		t.Fatalf("rejected pre-commit proof input: %v", problems)
	}
	op.Steps[0], op.Steps[1] = op.Steps[1], op.Steps[0]
	if len(validate()) == 0 {
		t.Fatal("accepted an anchor entered only after commit")
	}
}

func TestRenderStepsRequiresProofInputBeforeCommit(t *testing.T) {
	op := Operation{
		Effect:   EffectWrite,
		Response: ResponseSpec{Identifier: "id", Extract: map[string]Extract{"id": {Text: "prefix {{input.title}}"}}},
		Steps:    []Step{{Fill: "#title", Value: "{{input.title}}"}, {Click: "#submit", Commit: true}},
	}
	if steps, err := op.RenderSteps(Vars{}); !errors.Is(err, ErrAbsent) || steps != nil {
		t.Fatalf("missing proof: steps=%v err=%v", steps, err)
	}
	for _, value := range []string{"", "  "} {
		if steps, err := op.RenderSteps(Vars{Input: map[string]any{"title": value}}); err == nil || steps != nil {
			t.Fatalf("empty proof %q: steps=%v err=%v", value, steps, err)
		}
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
