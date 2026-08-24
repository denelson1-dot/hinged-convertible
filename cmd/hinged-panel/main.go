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
	panelW  = 116
	btnH    = 54
	pad     = 6
	nButton = 3
	panelH  = pad + nButton*(btnH+pad) // 186
	margin  = 20                       // gap from the screen edge
	topGap  = 72                       // clears a top panel on most desktops
)

// Colours as 0xRRGGBB.
const (
	colBackdrop     = 0x1c1f26
	colIdle         = 0x2864b4
	colListening    = 0xc62828
	colTranscribing = 0xd17a00
	colKeys         = 0x37474f
	colEnter        = 0x2e5d4b
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
	def := detectOSK()
	debug := flag.Bool("debug", false, "log X events and redraws to stderr")
	socket := flag.String("vox-socket", defaultVoxSocket(), "vox API socket")
	oskShow := flag.String("osk-show", def.show, "command to show the on-screen keyboard")
	oskHide := flag.String("osk-hide", def.hide, "command to hide it")
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

// oskDefault is the show/hide pair for one desktop's keyboard.
type oskDefault struct{ name, show, hide string }

// detectOSK picks the keyboard that suits the running desktop.
//
// This is not just a convenience. A desktop shell's menus and dialogs take a
// modal grab, and a click on any other X client dismisses them -- so typing
// into the start menu with a separate keyboard application closes the very
// thing you were typing into. A shell's own keyboard lives in the same process
// as its menus, so it does not break their grabs.
//
// Onboard is the better keyboard in general and stays the fallback. But on a
// desktop that ships its own, the built-in one is the only one that can type
// into that desktop's UI.
func detectOSK() oskDefault {
	const cinnamonToggle = "gdbus call --session --dest org.Cinnamon " +
		"--object-path /org/Cinnamon --method org.Cinnamon.ToggleKeyboard"

	desk := strings.ToLower(os.Getenv("XDG_CURRENT_DESKTOP") + " " + os.Getenv("XDG_SESSION_DESKTOP"))
	switch {
	case strings.Contains(desk, "cinnamon"):
		// ToggleKeyboard is a toggle, so the same command serves both ways;
		// the panel tracks which state it believes the keyboard is in.
		return oskDefault{"cinnamon", cinnamonToggle, cinnamonToggle}
	case strings.Contains(desk, "gnome"), strings.Contains(desk, "kde"), strings.Contains(desk, "plasma"):
		// These shells show their own keyboard in response to
		// SW_TABLET_MODE, which hinged now supplies, so driving one here
		// would fight the desktop.
		return oskDefault{"builtin", "", ""}
	case hasBinary("wvkbd-mobintl"):
		return oskDefault{"wvkbd", "wvkbd-mobintl -L 320", "pkill -x wvkbd-mobintl"}
	default:
		return oskDefault{"onboard", "onboard", "pkill -KILL -x onboard"}
	}
}

func hasBinary(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

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

// buttonAt returns which button a tap landed on, or -1 for the gaps between.
// Taps in the padding do nothing rather than being rounded to a neighbour: a
// mis-hit that submits a form is worse than one that does nothing.
func buttonAt(y int16) int {
	for i := 0; i < nButton; i++ {
		top := pad + i*(btnH+pad)
		if int(y) >= top && int(y) < top+btnH {
			return i
		}
	}
	return -1
}

func (p *panel) click(y int16) {
	switch buttonAt(y) {
	case 0:
		p.toggleVoice()
	case 1:
		p.toggleKeyboard()
	case 2:
		p.pressEnter()
	}
}

// pressEnter submits whatever was just dictated.
//
// It goes through vox rather than the panel growing its own virtual keyboard:
// vox already owns one and the device permissions for it, and a second
// synthetic keyboard would be a second thing to get right.
func (p *panel) pressEnter() {
	go func() {
		conn, err := net.Dial("unix", p.voxSocket)
		if err != nil {
			p.logf("enter: vox unreachable: %v", err)
			return
		}
		defer conn.Close()
		fmt.Fprintln(conn, "key Return")
		line, _ := bufio.NewReader(conn).ReadString('\n')
		p.logf("enter: %s", strings.TrimSpace(line))
	}()
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
	if len(p.oskShow) == 0 {
		p.logf("no on-screen keyboard configured; this desktop shows its own")
		return
	}
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

// buttonTop is the y of button i.
func buttonTop(i int) int16 { return int16(pad + i*(btnH+pad)) }

func (p *panel) redraw() {
	// Drawing is a sequence of "set colour, then draw" pairs, so two
	// goroutines interleaving would paint with each other's colours. The vox
	// state watcher and the X event loop both redraw.
	p.mu.Lock()
	defer p.mu.Unlock()
	p.logf("redraw state=%s kbd=%v", p.state, p.kbdShown)

	p.fill(0, 0, panelW, panelH, colBackdrop)
	w := uint16(panelW - 2*pad)

	micY := buttonTop(0)
	p.fill(pad, micY, w, btnH, p.micColor())
	p.drawMic(panelW/2, micY+btnH/2)

	kbdY := buttonTop(1)
	p.fill(pad, kbdY, w, btnH, colKeys)
	p.drawKeyboard(panelW/2, kbdY+btnH/2)

	entY := buttonTop(2)
	p.fill(pad, entY, w, btnH, colEnter)
	p.drawEnter(panelW/2, entY+btnH/2)

	// A strip along the bottom of the mic button repeats its state as a length
	// as well as a colour, so the panel still reads correctly to someone who
	// cannot distinguish the button colours.
	p.fill(pad, micY+btnH-4, p.stateBarWidth(), 4, colIcon)

	p.conn.Sync()
}

// drawEnter draws a return arrow: a shaft to the left, a head, and the riser.
func (p *panel) drawEnter(cx, cy int16) {
	p.setColor(colIcon)
	d := xproto.Drawable(p.win)

	xproto.PolyFillRectangle(p.conn, d, p.gc, []xproto.Rectangle{
		{X: cx - 13, Y: cy + 2, Width: 24, Height: 3}, // shaft
		{X: cx + 8, Y: cy - 10, Width: 3, Height: 15}, // riser
	})
	// Arrowhead, as a stack of rows narrowing to a point.
	var head []xproto.Rectangle
	for i := int16(0); i < 6; i++ {
		head = append(head, xproto.Rectangle{
			X: cx - 13 + i, Y: cy + 3 - i, Width: 1, Height: uint16(1 + 2*i),
		})
	}
	xproto.PolyFillRectangle(p.conn, d, p.gc, head)
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
		return uint16(panelW - 2*pad)
	case "transcribing":
		return uint16((panelW - 2*pad) / 2)
	default:
		return 0
	}
}

// drawMic draws a microphone: a capsule, a cradle, a stem and a base.
func (p *panel) drawMic(cx, cy int16) {
	p.setColor(colIcon)
	d := xproto.Drawable(p.win)

	// Capsule body.
	const bw, bh = 12, 20
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
		{X: cx - 10, Y: cy - 6, Width: 20, Height: 20, Angle1: 180 * 64, Angle2: 180 * 64},
	})
	// Stem and base.
	xproto.PolyFillRectangle(p.conn, d, p.gc, []xproto.Rectangle{
		{X: cx - 1, Y: cy + 8, Width: 3, Height: 7},
		{X: cx - 8, Y: cy + 15, Width: 17, Height: 3},
	})
}

// drawKeyboard draws a keyboard: an outline with three rows of keys.
func (p *panel) drawKeyboard(cx, cy int16) {
	p.setColor(colIcon)
	d := xproto.Drawable(p.win)

	const kw, kh = 42, 28
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
				X: x0 + 5 + int16(col*7), Y: y0 + 6 + int16(row*6), Width: 4, Height: 4,
			})
		}
	}
	keys = append(keys, xproto.Rectangle{X: x0 + 11, Y: y0 + 19, Width: 20, Height: 4})
	xproto.PolyFillRectangle(p.conn, d, p.gc, keys)
}
