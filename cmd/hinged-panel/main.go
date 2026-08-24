//go:build linux

// Command hinged-panel is the touch input panel for tablet posture.
//
// A folded convertible has no keyboard, so it needs an on-screen target to
// reach dictation and to summon a keyboard. This is that target: two large
// buttons that float above everything and never take focus.
//
// # Why raw X11 rather than a toolkit
//
// Two requirements, and they pull against each other. Tapping the panel must
// not move focus away from whatever you are dictating into, and the panel must
// stay on top of the on-screen keyboard it launches.
//
// Asking the window manager politely does not work for the first. WM_HINTS
// with input=False is the ICCCM way to say "never focus me" -- it is what
// set_accept_focus(false) sets -- and Muffin sets it and then focuses the
// window's frame on click anyway. Measured: focus moved from the browser to
// 0x2619f42, the frame the WM had wrapped around this window.
//
// So the window is override-redirect. That is not a hint, it is a statement
// that the window manager must not manage this window: no frame, no place in
// the focus model, and therefore no click-to-focus. It is the only way to make
// this guaranteed rather than requested.
//
// The cost is that keep-above is also a window manager service, forfeited
// along with everything else. An unmanaged window has to maintain its own
// stacking, which is what the periodic raise in watchScreen is for. Events
// cannot do it: under a compositing WM every window is redirected offscreen,
// so X reports them all unobscured and VisibilityNotify never fires.
//
// The WM_HINTS and _NET_WM_STATE properties below are set anyway. A window
// manager ignores them for an override-redirect window, but they cost nothing
// and describe the intent for anything else that inspects the window.
//
// Drawing is rectangles and arcs, so there is no font handling and no theme to
// load. On a touch target that is not a compromise: icons read better at a
// glance and at arm's length than a text label.
//
// # Limitation
//
// X11 only. The Wayland equivalent is layer-shell, which has no pure-Go
// binding today. On Wayland, bind `vox toggle` to a key or a hardware button
// instead; `hinged doctor` says which session you are in.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"encoding/binary"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
)

// Panel geometry. Sized for a fingertip rather than a mouse pointer: the
// smallest comfortable touch target is around 9mm, which is ~48px at typical
// laptop density, and these are deliberately larger.
const (
	panelW = 150
	panelH = 210
	btnH   = 96
	pad    = 8
	margin = 24 // gap from the screen edge
	topGap = 72 // clears a top panel on most desktops
)

// Colours as 0xRRGGBB.
const (
	colBackdrop     = 0x1c1f26
	colIdle         = 0x2864b4
	colListening    = 0xc62828
	colTranscribing = 0xd17a00
	colKeys         = 0x37474f
	colIcon         = 0xffffff
	colBorder       = 0x5a6270
)

type panel struct {
	mu     sync.Mutex // serialises drawing; see redraw
	conn   *xgb.Conn
	win    xproto.Window
	gc     xproto.Gcontext
	screen *xproto.ScreenInfo

	debug     bool
	state     string // vox state: ready | listening | transcribing
	kbdShown  bool
	lastRaise time.Time

	voxSocket string
	oskShow   []string
	oskHide   []string
}

func main() {
	debug := flag.Bool("debug", false, "log X events and redraws to stderr")
	socket := flag.String("vox-socket", defaultVoxSocket(), "vox API socket")
	oskShow := flag.String("osk-show", "onboard", "command to show the on-screen keyboard")
	oskHide := flag.String("osk-hide", "pkill -KILL -x onboard", "command to hide it")
	flag.Parse()

	p := &panel{
		debug:     *debug,
		state:     "ready",
		voxSocket: *socket,
		oskShow:   strings.Fields(*oskShow),
		oskHide:   strings.Fields(*oskHide),
	}
	if err := p.run(); err != nil {
		fmt.Fprintf(os.Stderr, "hinged-panel: %v\n", err)
		os.Exit(1)
	}
}

// nativeOrder is the byte order X expects for 32-bit property values: the
// server and client are on the same machine here, so it is the machine's own.
var nativeOrder = binary.LittleEndian

func defaultVoxSocket() string {
	if s := os.Getenv("VOX_SOCKET"); s != "" {
		return s
	}
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = fmt.Sprintf("/run/user/%d", os.Getuid())
	}
	return dir + "/vox.sock"
}

func (p *panel) run() error {
	conn, err := xgb.NewConn()
	if err != nil {
		return fmt.Errorf("cannot reach the X server: %w\n\n"+
			"the panel is X11 only; on Wayland bind 'vox toggle' to a key instead", err)
	}
	defer conn.Close()
	p.conn = conn
	p.screen = xproto.Setup(conn).DefaultScreen(conn)

	if err := p.createWindow(); err != nil {
		return err
	}
	go p.watchVox()
	go p.watchScreen()
	return p.eventLoop()
}

func (p *panel) createWindow() error {
	wid, err := xproto.NewWindowId(p.conn)
	if err != nil {
		return err
	}
	p.win = wid

	x := int16(int(p.screen.WidthInPixels) - panelW - margin)
	y := int16(topGap)

	// The value list must be ordered by mask bit, ascending.
	mask := uint32(xproto.CwBackPixel | xproto.CwOverrideRedirect | xproto.CwEventMask)
	values := []uint32{
		colBackdrop,
		1, // override-redirect: the window manager must not manage this window,
		//    which is what makes "never takes focus" a guarantee rather than a
		//    request the WM is free to ignore.
		uint32(xproto.EventMaskExposure | xproto.EventMaskButtonPress |
			xproto.EventMaskVisibilityChange | xproto.EventMaskStructureNotify),
	}

	if err := xproto.CreateWindowChecked(p.conn, p.screen.RootDepth, p.win, p.screen.Root,
		x, y, panelW, panelH, 0,
		xproto.WindowClassInputOutput, p.screen.RootVisual, mask, values).Check(); err != nil {
		return fmt.Errorf("creating the panel window: %w", err)
	}

	gc, err := xproto.NewGcontextId(p.conn)
	if err != nil {
		return err
	}
	p.gc = gc
	if err := xproto.CreateGCChecked(p.conn, p.gc, xproto.Drawable(p.win),
		xproto.GcForeground, []uint32{colIcon}).Check(); err != nil {
		return err
	}

	if err := p.setHints(); err != nil {
		return err
	}
	if err := xproto.MapWindowChecked(p.conn, p.win).Check(); err != nil {
		return err
	}
	p.raise()
	return nil
}

// atom interns a property name, or returns 0 if the server does not know it.
func (p *panel) atom(name string) xproto.Atom {
	r, err := xproto.InternAtom(p.conn, false, uint16(len(name)), name).Reply()
	if err != nil {
		return 0
	}
	return r.Atom
}

// setHints asks the window manager to keep the panel visible but unfocusable.
//
// These must be set before the window is mapped: a window manager reads them
// when it takes the window under management, and several ignore later changes.
func (p *panel) setHints() error {
	// WM_HINTS with the input field false. This is the "never give me the
	// keyboard focus" request, and it is what set_accept_focus(false) sets.
	// The structure is nine 32-bit values; only the flags and input fields
	// matter here, and InputHint is bit 0.
	hints := []uint32{1 /* InputHint */, 0 /* input = False */, 0, 0, 0, 0, 0, 0, 0}
	buf := make([]byte, 4*len(hints))
	for i, v := range hints {
		nativeOrder.PutUint32(buf[i*4:], v)
	}
	if err := xproto.ChangePropertyChecked(p.conn, xproto.PropModeReplace, p.win,
		xproto.AtomWmHints, xproto.AtomWmHints, 32, uint32(len(hints)), buf).Check(); err != nil {
		return fmt.Errorf("setting WM_HINTS: %w", err)
	}

	// A managed window should identify itself: window managers, accessibility
	// tools and the user's own diagnostics all key off these.
	name := "hinged input panel"
	xproto.ChangeProperty(p.conn, xproto.PropModeReplace, p.win,
		xproto.AtomWmName, xproto.AtomString, 8, uint32(len(name)), []byte(name))
	if na := p.atom("_NET_WM_NAME"); na != 0 {
		if utf8 := p.atom("UTF8_STRING"); utf8 != 0 {
			xproto.ChangeProperty(p.conn, xproto.PropModeReplace, p.win, na, utf8, 8,
				uint32(len(name)), []byte(name))
		}
	}
	// WM_CLASS is instance\0class\0.
	class := "hinged-panel\x00Hinged-panel\x00"
	xproto.ChangeProperty(p.conn, xproto.PropModeReplace, p.win,
		xproto.AtomWmClass, xproto.AtomString, 8, uint32(len(class)), []byte(class))

	// Utility type: a helper window rather than a document window, so window
	// managers do not give it a titlebar or a place in the task switcher.
	if wt, ut := p.atom("_NET_WM_WINDOW_TYPE"), p.atom("_NET_WM_WINDOW_TYPE_UTILITY"); wt != 0 && ut != 0 {
		b := make([]byte, 4)
		nativeOrder.PutUint32(b, uint32(ut))
		xproto.ChangeProperty(p.conn, xproto.PropModeReplace, p.win, wt, xproto.AtomAtom, 32, 1, b)
	}

	// Above everything, and out of the taskbar and pager.
	st := p.atom("_NET_WM_STATE")
	var states []xproto.Atom
	for _, n := range []string{"_NET_WM_STATE_ABOVE", "_NET_WM_STATE_SKIP_TASKBAR", "_NET_WM_STATE_SKIP_PAGER"} {
		if a := p.atom(n); a != 0 {
			states = append(states, a)
		}
	}
	if st != 0 && len(states) > 0 {
		b := make([]byte, 4*len(states))
		for i, a := range states {
			nativeOrder.PutUint32(b[i*4:], uint32(a))
		}
		xproto.ChangeProperty(p.conn, xproto.PropModeReplace, p.win, st, xproto.AtomAtom, 32,
			uint32(len(states)), b)
	}
	return nil
}

// raise puts the panel back on top.
//
// _NET_WM_STATE_ABOVE should make this unnecessary, and mostly does. It stays
// as a cheap backstop for two cases: window managers that honour the state
// lazily, and other always-on-top windows mapped after us.
func (p *panel) raise() {
	// Rate-limited so that a fight with another always-on-top window cannot
	// turn into a busy loop of mutual raising.
	if time.Since(p.lastRaise) < 150*time.Millisecond {
		return
	}
	p.lastRaise = time.Now()
	p.logf("raise")
	xproto.ConfigureWindow(p.conn, p.win,
		xproto.ConfigWindowStackMode, []uint32{xproto.StackModeAbove})
	p.conn.Sync()
}

// watchScreen keeps the panel on screen and on top.
//
// Both problems need the same periodic check, and neither can be done from
// events alone here.
//
// Rotation is the important one. The panel anchors to the right edge, and a
// convertible rotates when it folds: a 1920x1080 landscape screen becomes
// 1080x1920 portrait, so a position computed once at startup ends up hundreds
// of pixels off the side of the display. The window is still mapped and still
// being drawn -- it is simply nowhere the user can see it, which looks exactly
// like it disappeared.
//
// Restacking is the cheaper one. _NET_WM_STATE_ABOVE handles it on a
// well-behaved window manager, but under a compositing WM there is no reliable
// event to fall back on: every window is redirected to an offscreen pixmap, so
// X reports them all as unobscured and VisibilityNotify never fires.
func (p *panel) watchScreen() {
	var lastW, lastH uint16
	for range time.Tick(time.Second) {
		geo, err := xproto.GetGeometry(p.conn, xproto.Drawable(p.screen.Root)).Reply()
		if err != nil {
			continue
		}
		if geo.Width != lastW || geo.Height != lastH {
			lastW, lastH = geo.Width, geo.Height
			p.logf("screen is now %dx%d, repositioning", geo.Width, geo.Height)
			p.reposition(geo.Width, geo.Height)
		}
		p.raise()
	}
}

// reposition anchors the panel to the top-right of the current screen.
func (p *panel) reposition(sw, sh uint16) {
	x := int(sw) - panelW - margin
	y := topGap
	// On a tall portrait screen the panel would sit oddly high; keep it in the
	// upper third, where a thumb can reach it while holding the machine.
	if sh > sw {
		y = int(sh) / 6
	}
	if x < 0 {
		x = 0
	}
	xproto.ConfigureWindow(p.conn, p.win,
		xproto.ConfigWindowX|xproto.ConfigWindowY,
		[]uint32{uint32(int32(x)), uint32(int32(y))})
	p.conn.Sync()
}

// watchVox subscribes to state changes so the microphone button reflects what
// the service is doing, rather than polling or guessing.
func (p *panel) watchVox() {
	for {
		conn, err := net.Dial("unix", p.voxSocket)
		if err != nil {
			// vox may not be running yet, or at all. The keyboard button still
			// works, so the panel stays useful either way.
			p.setState("ready")
			time.Sleep(3 * time.Second)
			continue
		}
		fmt.Fprintln(conn, "subscribe")
		sc := bufio.NewScanner(conn)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			p.setState(strings.TrimSpace(strings.TrimPrefix(line, "ok")))
		}
		conn.Close()
		time.Sleep(time.Second)
	}
}

func (p *panel) setState(s string) {
	if s == "" || s == p.state {
		return
	}
	p.state = s
	p.redraw()
}

func (p *panel) logf(format string, args ...any) {
	if p.debug {
		fmt.Fprintf(os.Stderr, "[%s] "+format+"\n",
			append([]any{time.Now().Format("15:04:05.000")}, args...)...)
	}
}

func (p *panel) eventLoop() error {
	for {
		ev, err := p.conn.WaitForEvent()
		if err != nil {
			p.logf("WaitForEvent error: %v", err)
			return err
		}
		if ev == nil {
			p.logf("nil event, exiting")
			return nil
		}
		switch e := ev.(type) {
		case xproto.ExposeEvent:
			p.logf("Expose count=%d", e.Count)
			p.redraw()
		case xproto.ButtonPressEvent:
			p.click(e.EventY)
		case xproto.VisibilityNotifyEvent:
			p.logf("VisibilityNotify state=%d (0=unobscured 1=partial 2=fully)", e.State)
			// Anything other than fully visible means something stacked above
			// us. Onboard mapping its own window is the common case.
			if e.State != xproto.VisibilityUnobscured {
				p.raise()
				p.redraw()
			}
		}
	}
}

// click dispatches on which half of the panel was tapped.
func (p *panel) click(y int16) {
	if int(y) < pad+btnH {
		p.toggleVoice()
		return
	}
	p.toggleKeyboard()
}

func (p *panel) toggleVoice() {
	// Showing a keyboard and dictating at once makes no sense: both want to
	// put text in the same field. Starting dictation dismisses the keyboard.
	if p.kbdShown {
		p.hideKeyboard()
	}
	go func() {
		conn, err := net.Dial("unix", p.voxSocket)
		if err != nil {
			return
		}
		defer conn.Close()
		fmt.Fprintln(conn, "toggle")
		bufio.NewReader(conn).ReadString('\n')
	}()
}

func (p *panel) toggleKeyboard() {
	if p.kbdShown {
		p.hideKeyboard()
		return
	}
	// Cancel any recording first, for the same reason as above.
	go exec.Command("vox", "cancel").Run()
	if len(p.oskShow) > 0 {
		cmd := exec.Command(p.oskShow[0], p.oskShow[1:]...)
		if err := cmd.Start(); err == nil {
			go cmd.Wait() // reap it rather than leaving a zombie
		}
	}
	p.kbdShown = true
	// The keyboard maps its window above ours, so reclaim the top immediately
	// rather than waiting for the visibility event to arrive.
	go func() {
		for i := 0; i < 10; i++ {
			time.Sleep(200 * time.Millisecond)
			p.raise()
		}
	}()
	p.redraw()
}

func (p *panel) hideKeyboard() {
	if len(p.oskHide) > 0 {
		c := exec.Command(p.oskHide[0], p.oskHide[1:]...)
		_ = c.Run()
	}
	p.kbdShown = false
	p.redraw()
}

// --- drawing -----------------------------------------------------------------

func (p *panel) setColor(c uint32) {
	xproto.ChangeGC(p.conn, p.gc, xproto.GcForeground, []uint32{c})
}

func (p *panel) fill(x, y int16, w, h uint16, c uint32) {
	p.setColor(c)
	xproto.PolyFillRectangle(p.conn, xproto.Drawable(p.win), p.gc,
		[]xproto.Rectangle{{X: x, Y: y, Width: w, Height: h}})
}

func (p *panel) redraw() {
	// Drawing is a sequence of "set colour, then draw" pairs, so two
	// goroutines interleaving would paint with each other's colours. The vox
	// state watcher and the X event loop both redraw.
	p.mu.Lock()
	defer p.mu.Unlock()
	p.logf("redraw state=%s kbd=%v", p.state, p.kbdShown)

	p.fill(0, 0, panelW, panelH, colBackdrop)

	micY := int16(pad)
	p.fill(pad, micY, panelW-2*pad, btnH, p.micColor())
	p.drawMic(panelW/2, micY+btnH/2)

	kbdY := int16(pad + btnH + pad)
	p.fill(pad, kbdY, panelW-2*pad, btnH, colKeys)
	p.drawKeyboard(panelW/2, kbdY+btnH/2)

	// A thin strip under the mic button repeats its state, so the panel reads
	// correctly even for someone who cannot distinguish the button colours.
	p.fill(pad, micY+btnH-6, p.stateBarWidth(), 6, colIcon)

	p.conn.Sync()
}

func (p *panel) micColor() uint32 {
	switch p.state {
	case "listening":
		return colListening
	case "transcribing":
		return colTranscribing
	default:
		return colIdle
	}
}

// stateBarWidth encodes state as a length as well as a colour: empty when
// idle, full while listening, half while transcribing.
func (p *panel) stateBarWidth() uint16 {
	switch p.state {
	case "listening":
		return panelW - 2*pad
	case "transcribing":
		return (panelW - 2*pad) / 2
	default:
		return 0
	}
}

// drawMic draws a microphone: a capsule, a cradle, a stem and a base.
func (p *panel) drawMic(cx, cy int16) {
	p.setColor(colIcon)
	d := xproto.Drawable(p.win)

	// Capsule body.
	const bw, bh = 16, 30
	xproto.PolyFillRectangle(p.conn, d, p.gc, []xproto.Rectangle{
		{X: cx - bw/2, Y: cy - bh/2 - 6, Width: bw, Height: bh},
	})
	// Rounded ends.
	xproto.PolyFillArc(p.conn, d, p.gc, []xproto.Arc{
		{X: cx - bw/2, Y: cy - bh/2 - 6 - bw/2, Width: bw, Height: bw, Angle1: 0, Angle2: 360 * 64},
		{X: cx - bw/2, Y: cy + bh/2 - 6 - bw/2, Width: bw, Height: bw, Angle1: 0, Angle2: 360 * 64},
	})
	// Cradle: an arc open at the top.
	xproto.PolyArc(p.conn, d, p.gc, []xproto.Arc{
		{X: cx - 14, Y: cy - 8, Width: 28, Height: 28, Angle1: 180 * 64, Angle2: 180 * 64},
	})
	// Stem and base.
	xproto.PolyFillRectangle(p.conn, d, p.gc, []xproto.Rectangle{
		{X: cx - 2, Y: cy + 14, Width: 4, Height: 10},
		{X: cx - 12, Y: cy + 24, Width: 24, Height: 4},
	})
}

// drawKeyboard draws a keyboard: an outline with three rows of keys.
func (p *panel) drawKeyboard(cx, cy int16) {
	p.setColor(colIcon)
	d := xproto.Drawable(p.win)

	const kw, kh = 56, 38
	x0, y0 := cx-kw/2, cy-kh/2

	// Outline, drawn as four thin bars.
	xproto.PolyFillRectangle(p.conn, d, p.gc, []xproto.Rectangle{
		{X: x0, Y: y0, Width: kw, Height: 2},
		{X: x0, Y: y0 + kh - 2, Width: kw, Height: 2},
		{X: x0, Y: y0, Width: 2, Height: kh},
		{X: x0 + kw - 2, Y: y0, Width: 2, Height: kh},
	})

	// Keys: two rows of small squares and a spacebar.
	var keys []xproto.Rectangle
	for row := 0; row < 2; row++ {
		for col := 0; col < 5; col++ {
			keys = append(keys, xproto.Rectangle{
				X: x0 + 7 + int16(col*9), Y: y0 + 8 + int16(row*9), Width: 5, Height: 5,
			})
		}
	}
	keys = append(keys, xproto.Rectangle{X: x0 + 14, Y: y0 + 26, Width: 28, Height: 5})
	xproto.PolyFillRectangle(p.conn, d, p.gc, keys)
}
