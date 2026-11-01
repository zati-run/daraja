// Package plan turns exploration observations into a candidate integration.
//
// Planner is the extension point. The Deterministic planner shipped here
// makes no model call: it proposes one operation per observed JSON route,
// infers schemas from observed shapes, classifies effects by HTTP method,
// and uses the task text only to rank candidates. It does not understand
// natural language. A candidate is never served until a person reviews it:
// writes come out with reconcile "none" and a review note.
package plan

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/zati-run/daraja/builder/explore"
	"github.com/zati-run/daraja/integration"
)

// Planner proposes a candidate integration.
type Planner interface {
	Plan(ctx context.Context, task *explore.Task, rep *explore.Report) (*integration.Integration, *Report, error)
}

// Report explains a plan.
type Report struct {
	Planner    string      `json:"planner"`
	Candidates []Candidate `json:"candidates"`
	Skipped    []string    `json:"skipped"`
	Review     []string    `json:"review"`
}

// Candidate summarizes one proposed operation.
type Candidate struct {
	Operation string  `json:"operation"`
	Route     string  `json:"route"`
	Effect    string  `json:"effect"`
	Score     float64 `json:"score"`
}

// Deterministic is the rule-based planner.
type Deterministic struct {
	// MaxOperations bounds the candidate size. Zero means 12.
	MaxOperations int
}

const plannerName = "daraja/deterministic/v1"

var wordRE = regexp.MustCompile(`[a-z]+`)

func words(s string) map[string]bool {
	m := map[string]bool{}
	for _, w := range wordRE.FindAllString(strings.ToLower(s), -1) {
		if len(w) > 2 {
			m[explore.Singular(w)] = true
		}
	}
	return m
}

// Plan implements Planner.
func (d Deterministic) Plan(_ context.Context, task *explore.Task, rep *explore.Report) (*integration.Integration, *Report, error) {
	max := d.MaxOperations
	if max == 0 {
		max = 12
	}
	goal := words(task.Goal)
	pr := &Report{Planner: plannerName}
	type scored struct {
		op    integration.Operation
		score float64
		route string
	}
	var all []scored
	names := map[string]int{}
	for _, o := range rep.Observations {
		route := o.Method + " " + o.PathTemplate
		ok2xx := false
		for _, s := range o.Statuses {
			if s >= 200 && s < 300 {
				ok2xx = true
			}
		}
		if !o.Blocked && (!ok2xx || o.ResponseShape == nil) {
			pr.Skipped = append(pr.Skipped, route+": no successful JSON response observed")
			continue
		}
		op, err := operationFor(o, names)
		if err != nil {
			pr.Skipped = append(pr.Skipped, route+": "+err.Error())
			continue
		}
		score := 0.0
		for w := range words(o.PathTemplate) {
			if goal[w] {
				score++
			}
		}
		if op.Effect == integration.EffectWrite && (goal["create"] || goal["add"] || goal["update"]) {
			score += 0.5
		}
		all = append(all, scored{op, score, route})
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].score > all[j].score })
	if len(all) > max {
		for _, s := range all[max:] {
			pr.Skipped = append(pr.Skipped, s.route+": below the candidate limit")
		}
		all = all[:max]
	}
	in := &integration.Integration{
		Format:      integration.Format,
		ID:          task.IntegrationID,
		Version:     "0-candidate",
		Description: "Candidate for: " + task.Goal,
		Source:      integration.Source{Kind: "web", Origin: task.Origin},
		Session:     task.Session,
		Provenance: integration.Provenance{
			BuiltBy: "daraja plan", Planner: plannerName, Observations: rep.Hash(),
			Notes: "Candidate only. Review names, schemas, effects and reconciliation before publishing.",
		},
	}
	for _, s := range all {
		in.Operations = append(in.Operations, s.op)
		pr.Candidates = append(pr.Candidates, Candidate{Operation: s.op.Name, Route: s.route, Effect: s.op.Effect, Score: s.score})
		if s.op.Effect == integration.EffectWrite {
			pr.Review = append(pr.Review,
				s.op.Name+": write with reconcile \"none\"; add a marker and a reconcile read if the source supports one",
				s.op.Name+": effect and destructive were proposed from the method; confirm both (a POST search or query is a read with reviewed_safe_method)")
		} else {
			pr.Review = append(pr.Review, s.op.Name+": effect proposed as read from the method; confirm the request has no effect (some apps act on GET)")
		}
	}
	pr.Review = append(pr.Review, "output schemas come from sampled responses; tighten required fields and types")
	return in, pr, nil
}

func operationFor(o explore.Observation, names map[string]int) (integration.Operation, error) {
	effect := integration.EffectRead
	verb := "get"
	segs := nouns(o.PathTemplate)
	lastIsParam := strings.HasSuffix(strings.TrimSuffix(o.PathTemplate, ".json"), "}")
	switch o.Method {
	case "GET":
		if !lastIsParam {
			verb = "list"
		}
	case "POST":
		effect, verb = integration.EffectWrite, "create"
	case "PUT", "PATCH":
		effect, verb = integration.EffectWrite, "update"
	case "DELETE":
		effect, verb = integration.EffectWrite, "delete"
	default:
		return integration.Operation{}, fmt.Errorf("method %s is not proposed", o.Method)
	}
	noun := "resource"
	if len(segs) > 0 {
		noun = segs[len(segs)-1]
		if verb != "list" {
			noun = explore.Singular(noun)
		}
	}
	name := verb + "_" + noun
	names[name]++
	if names[name] > 1 {
		name = fmt.Sprintf("%s_%d", name, names[name])
	}
	props := map[string]any{}
	required := []any{}
	path := o.PathTemplate
	for _, p := range o.PathParams {
		props[p.Name] = map[string]any{"type": p.Kind}
		required = append(required, p.Name)
		path = strings.Replace(path, "{"+p.Name+"}", "{{input."+p.Name+"}}", 1)
	}
	req := integration.RequestSpec{Method: o.Method, Path: path}
	if len(o.QueryNames) > 0 {
		req.Query = map[string]string{}
		for _, q := range o.QueryNames {
			n := sanitize(q)
			if _, clash := props[n]; clash {
				n = "q_" + n
			}
			props[n] = map[string]any{"type": "string"}
			req.Query[q] = "{{input." + n + "}}"
		}
	}
	if effect == integration.EffectWrite && o.RequestShape != nil {
		body, fields := bodyTemplate(o.RequestShape, "", props)
		for k, v := range fields {
			props[k] = v
		}
		b, _ := json.Marshal(body)
		req.Body = b
	}
	in := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		in["required"] = required
	}
	output, outSchema := outputMapping(o.ResponseShape)
	inB, _ := json.Marshal(in)
	outB, _ := json.Marshal(outSchema)
	op := integration.Operation{
		Name:         name,
		Description:  fmt.Sprintf("%s %s (observed %d time(s)).", o.Method, o.PathTemplate, o.Count),
		Effect:       effect,
		InputSchema:  inB,
		OutputSchema: outB,
		Request:      req,
		Response:     integration.ResponseSpec{SuccessStatus: successStatuses(o), Output: output},
	}
	if effect == integration.EffectWrite {
		// A proposal only: the method never decides the effect class, and
		// review must confirm both effect and destructive.
		destructive := o.Method == "DELETE"
		op.Destructive = &destructive
		op.Reconcile = &integration.Reconcile{Strategy: integration.ReconcileNone}
		op.Description += " REVIEW: confirm the effect and add reconciliation."
		if o.Blocked {
			op.Description += " The explorer blocked this write, so no response shape was observed."
		}
	}
	return op, nil
}

func successStatuses(o explore.Observation) []int {
	var out []int
	for _, s := range o.Statuses {
		if s >= 200 && s < 300 {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		switch o.Method {
		case "POST":
			out = []int{200, 201}
		default:
			out = []int{200}
		}
	}
	return out
}

var identRE = regexp.MustCompile(`[^a-z0-9_]+`)

func sanitize(s string) string {
	s = strings.Trim(identRE.ReplaceAllString(strings.ToLower(s), "_"), "_")
	if s == "" || s[0] < 'a' || s[0] > 'z' {
		s = "f_" + s
	}
	return s
}

func nouns(tmpl string) []string {
	var out []string
	for _, s := range strings.Split(tmpl, "/") {
		s = strings.TrimSuffix(s, ".json")
		if s == "" || strings.HasPrefix(s, "{") || s == "api" || regexp.MustCompile(`^v\d+$`).MatchString(s) {
			continue
		}
		out = append(out, sanitize(s))
	}
	return out
}

// bodyTemplate turns an observed request shape into a JSON body template
// whose scalar leaves are inputs.
func bodyTemplate(shape map[string]any, prefix string, taken map[string]any) (any, map[string]any) {
	fields := map[string]any{}
	props, _ := shape["properties"].(map[string]any)
	if props == nil {
		return map[string]any{}, fields
	}
	out := map[string]any{}
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		s, _ := props[k].(map[string]any)
		if s["type"] == "object" {
			sub, f := bodyTemplate(s, prefix+k+"_", taken)
			out[k] = sub
			for n, v := range f {
				fields[n] = v
			}
			continue
		}
		name := sanitize(k)
		if _, clash := taken[name]; clash {
			name = sanitize(prefix + k)
		}
		if _, clash := fields[name]; clash {
			name = sanitize(prefix + k)
		}
		fields[name] = s
		out[k] = "{{input." + name + "}}"
	}
	return out, fields
}

// outputMapping selects scalar fields from a response shape: top-level
// scalars, and the scalars of a single wrapped object such as {"item":{...}}.
func outputMapping(shape map[string]any) (map[string]string, map[string]any) {
	mapping := map[string]string{}
	props := map[string]any{}
	add := func(name, ptr string, s map[string]any) {
		if len(mapping) >= 30 {
			return
		}
		mapping[name] = ptr
		props[name] = nullable(s)
	}
	top, _ := shape["properties"].(map[string]any)
	keys := sortedKeys(top)
	for _, k := range keys {
		s, _ := top[k].(map[string]any)
		if isScalar(s) {
			add(sanitize(k), "/"+esc(k), s)
		}
	}
	for _, k := range keys {
		s, _ := top[k].(map[string]any)
		inner, _ := s["properties"].(map[string]any)
		if s["type"] == "object" && inner != nil {
			for _, ik := range sortedKeys(inner) {
				is, _ := inner[ik].(map[string]any)
				if isScalar(is) {
					name := sanitize(ik)
					if _, clash := mapping[name]; clash {
						name = sanitize(k + "_" + ik)
					}
					add(name, "/"+esc(k)+"/"+esc(ik), is)
				}
			}
		}
		if s["type"] == "array" {
			// A list: return the array as is, typed loosely.
			mapping[sanitize(k)] = "/" + esc(k)
			props[sanitize(k)] = map[string]any{"type": []any{"array", "null"}}
		}
	}
	return mapping, map[string]any{"type": "object", "properties": props}
}

func nullable(s map[string]any) map[string]any {
	switch t := s["type"].(type) {
	case string:
		if t == "null" {
			return map[string]any{}
		}
		return map[string]any{"type": []any{t, "null"}}
	case []any:
		has := false
		for _, x := range t {
			if x == "null" {
				has = true
			}
		}
		if !has {
			t = append(t, "null")
		}
		return map[string]any{"type": t}
	}
	return map[string]any{}
}

func isScalar(s map[string]any) bool {
	switch t := s["type"].(type) {
	case string:
		return t != "object" && t != "array"
	case []any:
		for _, x := range t {
			if x == "object" || x == "array" {
				return false
			}
		}
		return true
	}
	return false
}

func sortedKeys(m map[string]any) []string {
	k := make([]string, 0, len(m))
	for x := range m {
		k = append(k, x)
	}
	sort.Strings(k)
	return k
}

func esc(s string) string { return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1") }
