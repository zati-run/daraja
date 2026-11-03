package integration

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"github.com/andybalholm/cascadia"
)

// ExecBrowserSteps drives the application's own interface in a headed browser.
// It is the top rung of the ladder, for what no request can do.
const ExecBrowserSteps = "browser.steps"

// Step is one action of a browser.steps plan. It sets exactly one of Navigate,
// Fill, Select, Click, WaitFor and WaitPathNot.
type Step struct {
	Navigate string `json:"navigate,omitempty"` // path on the origin; may use {{input.x}}
	Fill     string `json:"fill,omitempty"`     // CSS selector of a text field
	Select   string `json:"select,omitempty"`   // CSS selector of a <select>
	Value    string `json:"value,omitempty"`    // for Fill and Select; may use {{input.x}} and {{receipt.marker}}
	Click    string `json:"click,omitempty"`    // CSS selector
	// Text narrows Click to the first shown element whose text contains it. A commit
	// step may use it only with Expect, so a redesign can't turn it into a
	// click on something else.
	Text        string `json:"text,omitempty"`
	WaitFor     string `json:"wait_for,omitempty"`      // CSS selector to become visible
	WaitPathNot string `json:"wait_path_not,omitempty"` // page path no longer starts with this
	// Expect is checked before the step acts, on every step. A mismatch up to
	// and including the commit step's check means the commit action didn't run.
	Expect []Expect `json:"expect,omitempty"`
	// Commit marks the one step of a write after which the effect may exist.
	Commit bool `json:"commit,omitempty"`
}

// Expect asserts that an element exists, and optionally that its text equals
// a value.
type Expect struct {
	CSS  string `json:"css"`
	Text string `json:"text,omitempty"`
}

func (c *Compiled) validateSteps(op *Operation, where string, add func(string, ...any)) {
	if u, err := url.Parse(c.Source.Origin); err == nil && u.Host != "" {
		host, port := u.Hostname(), u.Port()
		noncanonical := u.User != nil || u.Fragment != "" || u.ForceQuery || u.Path != "" || strings.HasSuffix(u.Host, ":") || u.Host != strings.ToLower(u.Host) || (len(port) > 1 && port[0] == '0') || (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80")
		for _, r := range host {
			if r > 127 {
				noncanonical = true
			}
		}
		if ip, err := netip.ParseAddr(host); err == nil {
			noncanonical = noncanonical || ip.String() != host
		} else {
			last := strings.TrimSuffix(host, ".")
			if i := strings.LastIndexByte(last, '.'); i >= 0 {
				last = last[i+1:]
			}
			_, numeric := strconv.ParseUint(last, 0, 64)
			if strings.Contains(host, ":") || numeric == nil || (last != "" && strings.Trim(last, "0123456789") == "") {
				noncanonical = true
			}
		}
		if noncanonical {
			add("%s: browser.steps source.origin must be a canonical browser origin (lowercase host, no trailing slash or default port)", where)
		}
	}
	if c.Session.Provider != ProviderBrowser {
		add("%s: browser.steps needs session provider browser", where)
	}
	if len(op.Steps) == 0 {
		add("%s: browser.steps needs steps", where)
	}
	r := op.Request
	if r.Method != "" || r.Path != "" || len(r.Query) > 0 || len(r.Headers) > 0 || len(r.Body) > 0 || len(r.Form) > 0 || r.FormPage != nil {
		add("%s: browser.steps takes steps, not a request", where)
	}
	resp := &op.Response
	if len(resp.Output) > 0 || len(resp.SuccessStatus) > 0 || resp.RedirectSuccess != "" {
		add("%s: browser.steps takes response.extract, response.identifier and response.refused_when only", where)
	}
	if len(resp.Extract) == 0 && op.Effect == EffectRead {
		add("%s: a read needs response.extract", where)
	}
	if op.Effect == EffectWrite {
		if _, ok := resp.Extract[resp.Identifier]; resp.Identifier == "" || !ok {
			add("%s: a browser.steps write needs response.identifier naming a response.extract", where)
		} else {
			c.validateStepsProof(op, where, add)
		}
	}
	extractRoots := map[string]bool{"input": true}
	if op.Effect == EffectWrite {
		extractRoots["receipt"] = true
	}
	for name, e := range resp.Extract {
		if !valueNameRE.MatchString(name) {
			add("%s: response.extract: bad name %q", where, name)
		}
		validateExtract(where+".response.extract."+name, e, extractRoots, add)
	}
	// The answer is the final page, so it always counts as a success status;
	// what it means is decided by refused_when and the identifier.
	resp.SuccessStatus = []int{200}

	roots := map[string]bool{"input": true}
	if op.Effect == EffectWrite {
		roots["receipt"] = true
	}
	commits := 0
	for i, st := range op.Steps {
		w := fmt.Sprintf("%s.steps[%d]", where, i)
		n := 0
		for _, set := range []bool{st.Navigate != "", st.Fill != "", st.Select != "", st.Click != "", st.WaitFor != "", st.WaitPathNot != ""} {
			if set {
				n++
			}
		}
		if n != 1 {
			add("%s: exactly one of navigate, fill, select, click, wait_for, wait_path_not is required", w)
		}
		if st.Navigate != "" {
			checkPath(w, st.Navigate, add)
			checkTemplate(w+".navigate", st.Navigate, map[string]bool{"input": true}, add)
		}
		for _, sel := range []string{st.Fill, st.Select, st.Click, st.WaitFor} {
			if sel != "" {
				if _, err := cascadia.ParseGroup(sel); err != nil {
					add("%s: selector %q: %v", w, sel, err)
				}
			}
		}
		if (st.Fill != "" || st.Select != "") != (st.Value != "") {
			add("%s: value goes with fill or select", w)
		}
		checkTemplate(w+".value", st.Value, roots, add)
		if st.Text != "" && st.Click == "" {
			add("%s: text applies only to click", w)
		}
		checkTemplate(w+".text", st.Text, roots, add)
		for j, e := range st.Expect {
			if _, err := cascadia.ParseGroup(e.CSS); e.CSS == "" || err != nil {
				add("%s.expect[%d]: css %q is not a CSS selector", w, j, e.CSS)
			}
		}
		if op.Effect == EffectRead && !op.ReviewedSafeMethod && (st.Fill != "" || st.Select != "" || st.Click != "") {
			add("%s: a read that fills, selects or clicks needs reviewed_safe_method; reads and writes stay separate", w)
		}
		if st.Commit {
			commits++
			if st.Click == "" && st.Navigate == "" {
				add("%s: a commit step must be click or navigate", w)
			}
			if st.Text != "" && len(st.Expect) == 0 {
				add("%s: a commit step with a text anchor needs expect", w)
			}
		}
	}
	switch {
	case op.Effect == EffectWrite && commits != 1:
		add("%s: a browser.steps write needs exactly one commit step, found %d", where, commits)
	case op.Effect == EffectRead && commits != 0:
		add("%s: a read has no commit step", where)
	}
}

// validateStepsProof makes the identifier evidence of this write. The final
// page is whatever the application ended on, and an existing record's page has
// an identifier too. So the identifier must come from an element that shows
// what this write typed: the marker, when the write has one.
func (c *Compiled) validateStepsProof(op *Operation, where string, add func(string, ...any)) {
	marked := op.Reconcile != nil && op.Reconcile.Strategy == ReconcileMarker
	e := op.Response.Extract[op.Response.Identifier]
	refs := templateRefs(e.Text)
	anchored := false
	for _, r := range refs {
		if !marked || (r.root == "receipt" && r.name == "marker") {
			anchored = true
		}
	}
	switch {
	case e.CSS == "" || !anchored && marked:
		add("%s: a browser.steps write's identifier needs css with text {{receipt.marker}}, so an unrelated page can't count as this write", where)
	case !anchored:
		add("%s: a browser.steps write's identifier needs css with text from {{input.x}}, so an unrelated page can't count as this write", where)
	}
	if !marked {
		for _, st := range op.Steps {
			if st.Commit {
				break
			}
			if st.Fill == "" && st.Select == "" {
				continue
			}
			for _, used := range templateRefs(st.Value) {
				for _, anchor := range refs {
					if used.root == "input" && used == anchor {
						return
					}
				}
			}
		}
		add("%s: a browser.steps write's identifier needs an input used by a fill or select before the commit step", where)
		return
	}
	for _, st := range op.Steps {
		if st.Commit {
			break
		}
		if st.Fill == "" {
			continue
		}
		for _, r := range templateRefs(st.Value) {
			if r.root == "receipt" && r.name == "marker" {
				return
			}
		}
	}
	add("%s: reconcile marker needs {{receipt.marker}} typed by a fill before the commit step", where)
}

// RenderSteps fills the plan's templates. A fill or select whose value names
// an absent input is dropped, like an absent field of a request.
func (o *Operation) RenderSteps(v Vars) ([]Step, error) {
	out := make([]Step, 0, len(o.Steps))
	proof := map[ref]bool{}
	if o.Effect == EffectWrite {
		for _, r := range templateRefs(o.Response.Extract[o.Response.Identifier].Text) {
			proof[r] = true
		}
	}
	committed := false
	for _, st := range o.Steps {
		var err error
		if st.Navigate != "" {
			st.Navigate, err = v.RenderString(st.Navigate, url.PathEscape)
			if err != nil {
				return nil, err
			}
		}
		if st.Text != "" {
			st.Text, err = v.RenderString(st.Text, nil)
			if err != nil {
				return nil, err
			}
		}
		if st.Value != "" {
			valueRefs := templateRefs(st.Value)
			if !committed && (st.Fill != "" || st.Select != "") {
				for _, r := range valueRefs {
					if !proof[r] {
						continue
					}
					value, err := v.lookup(r)
					if err != nil {
						return nil, err
					}
					s, err := scalarString(value)
					if err != nil {
						return nil, err
					}
					if strings.TrimSpace(s) == "" {
						return nil, fmt.Errorf("write proof value {{%s.%s}} is empty", r.root, r.name)
					}
				}
			}
			st.Value, err = v.RenderString(st.Value, nil)
			if errors.Is(err, ErrAbsent) {
				if !committed && (st.Fill != "" || st.Select != "") {
					for _, r := range valueRefs {
						if proof[r] {
							return nil, err
						}
					}
				}
				continue
			}
			if err != nil {
				return nil, err
			}
		}
		out = append(out, st)
		committed = committed || st.Commit
	}
	return out, nil
}
