package integration

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestStepsOriginMustMatchBrowserCanonicalForm(t *testing.T) {
	for _, tc := range []struct {
		origin  string
		invalid bool
	}{
		{"https://example.com", false}, {"http://127.0.0.1:8080", false},
		{"https://Example.com", true}, {"https://example.com/", true},
		{"https://example.com:443", true}, {"http://127.0.0.1:80", true},
		{"https://example.com:0443", true}, {"http://127.1", true},
		{"https://[2001:0db8::1]", true}, {"https://é.example", true},
		{"https://[2001:db8::1]", false},
		{"https://example.com#fragment", true},
	} {
		t.Run(tc.origin, func(t *testing.T) {
			c := &Compiled{Integration: &Integration{Source: Source{Origin: tc.origin}}}
			var problems []string
			c.validateSteps(&Operation{}, "op", func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) })
			found := strings.Contains(strings.Join(problems, "\n"), "canonical browser origin")
			if found != tc.invalid {
				t.Fatalf("canonical origin error = %v, want %v: %v", found, tc.invalid, problems)
			}
		})
	}
}

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
