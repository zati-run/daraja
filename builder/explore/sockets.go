package explore

import (
	"context"
	"sync"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/serviceworker"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

// socketGuard extends noSocketsJS from pages to the workers and frames they
// start, in a read-only walk. Request interception does not see WebSocket
// frames, so a socket constructor left in any JavaScript realm is a write
// channel. (noSocketsJS itself refuses shared workers, which tab-level
// auto-attach does not reach.)
//
// Every related target of an attached tab (a dedicated or service worker,
// or a cross-site frame) is auto-attached paused, before its first script
// runs. A dedicated worker gets the socket constructors removed and its own
// related targets armed the same way, and is then resumed. A cross-site
// frame (no execution context yet) and a service worker (attaching hangs)
// cannot be attached while paused, so they are held paused until the walk
// ends: they never run, which keeps them from writing and leaves their
// traffic unrecorded. Tabs bypass service workers, so a page does not wait
// on a held one. A target that cannot be armed is held too.
type socketGuard struct {
	root context.Context // the browser's context; worker sessions hang off it
	life context.Context // ends with the walk
	wg   *sync.WaitGroup

	mu       sync.Mutex
	armed    int                       // workers started with sockets removed
	held     map[target.SessionID]bool // targets kept paused, until they are gone
	heldEver int                       // how many targets were ever kept paused
	running  int                       // targets already running when attached
	attached []context.CancelFunc
}

var autoAttachPaused = target.SetAutoAttachParams{AutoAttach: true, WaitForDebuggerOnStart: true, Flatten: ptrBool(true)}

func ptrBool(b bool) *bool { return &b }

func newSocketGuard(root, life context.Context, wg *sync.WaitGroup) *socketGuard {
	return &socketGuard{root: root, life: life, wg: wg, held: map[target.SessionID]bool{}}
}

// armTab arms a tab (or a worker) session: its related targets start
// paused. Popups are left to tab following, which records and guards them
// like any tab. (Auto-attach on the browser session instead stalls popups.)
func (g *socketGuard) armTab(ctx context.Context) error {
	ectx, ecancel := context.WithCancel(ctx)
	context.AfterFunc(g.life, ecancel)
	evs := chromedp.Events(ectx, target.AttachedToTarget)
	gone := chromedp.Events(ectx, target.DetachedFromTarget)
	p := autoAttachPaused
	p.Filter = target.Filter{{Type: "page", Exclude: true}, {Type: "browser", Exclude: true}, {Type: "tab", Exclude: true}, {}}
	if _, err := chromedp.Call(ctx, target.SetAutoAttach, p); err != nil {
		ecancel()
		return err
	}
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		for ev, err := range evs {
			if err != nil {
				return
			}
			g.handle(ev)
		}
	}()
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		for ev, err := range gone {
			if err != nil {
				return
			}
			g.mu.Lock()
			delete(g.held, ev.SessionID)
			g.mu.Unlock()
		}
	}()
	return nil
}

// stopServiceWorkers stops service workers already running, so that any
// that start again during the walk do so under an armed tab, paused.
func (g *socketGuard) stopServiceWorkers(tab context.Context) error {
	if _, err := chromedp.Call(tab, serviceworker.Enable, cdp.Empty{}); err != nil {
		return err
	}
	_, err := chromedp.Call(tab, serviceworker.StopAllWorkers, cdp.Empty{})
	return err
}

func (g *socketGuard) handle(ev target.EventAttachedToTarget) {
	if ev.TargetInfo == nil {
		return
	}
	if !ev.WaitingForDebugger {
		g.mu.Lock()
		g.running++
		g.mu.Unlock()
		return
	}
	switch ev.TargetInfo.Type {
	case "worker":
		g.wg.Add(1)
		go func() {
			defer g.wg.Done()
			if !g.armWorker(ev.TargetInfo.TargetID) {
				g.hold(ev.SessionID)
			}
		}()
	default: // a cross-site frame or a service worker cannot be attached while paused
		g.hold(ev.SessionID)
	}
}

// armWorker removes the socket constructors from a paused worker, arms its
// own related targets, and resumes it. It reports false when the worker is
// left paused.
func (g *socketGuard) armWorker(id target.ID) bool {
	wctx, wcancel := chromedp.NewContext(g.root, chromedp.WithTargetID(id), chromedp.WithDetachOnCancel())
	g.mu.Lock()
	g.attached = append(g.attached, wcancel)
	g.mu.Unlock()
	if err := chromedp.Do(wctx); err != nil {
		return false
	}
	res, err := chromedp.Call(wctx, runtime.Evaluate, runtime.EvaluateParams{Expression: noSocketsJS + `; typeof WebSocket === "function" && (() => { try { new WebSocket("ws://0.0.0.0:0/"); return false } catch (e) { return e.name === "SecurityError" } })()`, ReturnByValue: ptrBool(true)})
	if err != nil || res.ExceptionDetails != nil || res.Result == nil || string(res.Result.Value) != "true" {
		return false
	}
	if err := g.armTab(wctx); err != nil {
		return false
	}
	if _, err := chromedp.Call(wctx, runtime.RunIfWaitingForDebugger, cdp.Empty{}); err != nil {
		return false
	}
	g.mu.Lock()
	g.armed++
	g.mu.Unlock()
	return true
}

func (g *socketGuard) hold(s target.SessionID) {
	g.mu.Lock()
	g.held[s] = true
	g.heldEver++
	g.mu.Unlock()
}

// waitHeldGone waits for the targets still held to go away, which the pages
// that started them going away causes. It returns how many remain. Navigating
// a page returns before its frames are gone, and a frame released while its
// page is still there runs, with its sockets.
func (g *socketGuard) waitHeldGone(ctx context.Context, d time.Duration) int {
	deadline := time.Now().Add(d)
	for {
		g.mu.Lock()
		n := len(g.held)
		g.mu.Unlock()
		if n == 0 || time.Now().After(deadline) {
			return n
		}
		select {
		case <-ctx.Done():
			return n
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// disarm, after the walk has stopped every page and every held target has
// gone, stops pausing new targets and detaches from workers.
func (g *socketGuard) disarm(tab context.Context) {
	// Turning auto-attach off would release any target still held under the
	// main tab, which is why the caller makes sure there are none. Then
	// restore chromedp's own setting.
	_, _ = chromedp.Call(tab, target.SetAutoAttach, target.SetAutoAttachParams{AutoAttach: false, Flatten: ptrBool(true)})
	_, _ = chromedp.Call(tab, target.SetAutoAttach, target.SetAutoAttachParams{AutoAttach: true, Flatten: ptrBool(true)})
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, c := range g.attached {
		c()
	}
}
