//go:build linux

// Command hinged-panel is the touch input panel for tablet posture.
//
// A folded convertible has no keyboard, so it needs an on-screen target to
// reach dictation and to summon a keyboard. This is that target: two large
// buttons that float above everything and never take focus.
//
// # Why raw X11 rather than a toolkit
//
// The one hard requirement is that tapping the panel must not move focus away
// from whatever you are dictating into. X11 expresses this directly with an
// override-redirect window: the window manager does not manage it, so it is
// never given focus, never appears in a task list, and stays above managed
// windows without asking. A toolkit would have to reach through to the same
// property anyway, and the toolkits available either do not expose it or cost
// a cgo dependency to get at it.
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
	"time"

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
	conn   *xgb.Conn
	win    xproto.Window
	gc     xproto.Gcontext
	screen *xproto.ScreenInfo

	state    string // vox state: ready | listening | transcribing
	kbdShown bool

	voxSocket string
	oskShow   []string
	oskHide   []string
}

func main() {
	socket := flag.String("vox-socket", defaultVoxSocket(), "vox API socket")
	oskShow := flag.String("osk-show", "onboard", "command to show the on-screen keyboard")
	oskHide := flag.String("osk-hide", "pkill -KILL -x onboard", "command to hide it")
	flag.Parse()

	p := &panel{
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
		1, // override-redirect: the window manager leaves this window alone,
		//    which is what guarantees it never takes focus.
		uint32(xproto.EventMaskExposure | xproto.EventMaskButtonPress),
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

	return xproto.MapWindowChecked(p.conn, p.win).Check()
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

func (p *panel) eventLoop() error {
	for {
		ev, err := p.conn.WaitForEvent()
		if err != nil {
			return err
		}
		if ev == nil {
			return nil
		}
		switch e := ev.(type) {
		case xproto.ExposeEvent:
			p.redraw()
		case xproto.ButtonPressEvent:
			p.click(e.EventY)
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
