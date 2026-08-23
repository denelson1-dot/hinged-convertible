# hinged

**Tablet mode for Linux convertibles — for the machines the kernel gets wrong.**

Written in Go. The core is a single static binary with no runtime dependencies.

> **Status: early.** The policy engine and hardware probe work and are tested.
> Switch synthesis is in progress. See [Roadmap](#roadmap).

---

## Does this apply to my laptop?

```sh
hinged doctor
```

It needs no privileges and changes nothing. Its output is also exactly what to
paste into a bug report.

**Most people don't need this tool, and it will tell you so.** If your kernel
already reports `SW_TABLET_MODE` correctly, libinput handles the rest and
`hinged` stays out of the way. This is for the machines where that doesn't work.

## Why this exists

When the kernel emits `SW_TABLET_MODE`, **libinput already disables the internal
keyboard, touchpad and trackpoint** ([docs][switches]). That happens inside
libinput, so it works on every Wayland compositor and on X11 alike. Detecting a
fold and running `xinput disable` reimplements — badly, and X11-only — something
you already have.

The real problem is that **a great many convertibles never emit that switch, or
emit it wrongly**:

- The kernel's `hp-wmi` driver has had tablet-mode reporting **disabled by
  default since ~5.10**, because HP models claimed support and then always
  reported 0 — breaking auto-rotation and the on-screen keyboard for everyone.
- `intel-vbtn` and `intel-hid` gate the switch behind **DMI allow-lists**. If
  your chassis isn't listed, you get nothing.
- Some machines assert it *falsely at boot*, permanently disabling the keyboard.
  The usual folk remedy is blacklisting `intel_ish_ipc`, which also costs you
  rotation and lid-wake.

Meanwhile plenty of those same machines expose a perfectly good **hinge angle**
that nothing in the stack consumes.

`hinged` bridges the two. It reads whatever your machine actually has, applies a
real thresholding policy, and **synthesizes a `uinput` device that emits
`SW_TABLET_MODE`**. Everything downstream — libinput, GNOME, KDE, sway,
Hyprland — then works unmodified.

```
hinge angle ─┐
dual accel  ─┼─→  policy engine  ─→  uinput SW_TABLET_MODE
vendor keys ─┘    hysteresis              │
                  wrap guard              ▼
                  debounce        libinput disables the keyboard
                  slew gate       your desktop reacts, unmodified
                  lid override
```

## The policy engine

The thresholding is the part that doesn't exist elsewhere, and it is a pure
function with no I/O — so all of it is tested without hardware.

- **Hysteresis.** Enter tablet at ≥210°, return to laptop at ≤180°. The dead
  band between them is what stops the posture flapping while the hinge rests
  near a threshold. Never set them equal.
- **Wrap guard.** The sensor wraps 360→0 at full fold, so a small angle can mean
  *folded all the way back* — but it can equally mean a shut lid or a sensor
  glitch. The angle alone cannot tell them apart, so the decision uses the
  physical path instead: you cannot reach a wrapped near-zero from laptop
  posture without passing through tent and tablet first, so from laptop such a
  reading is always rejected. At startup, where there is no path to reason from,
  the lid switch settles it.
- **Asymmetric debounce.** One sample to enter tablet mode on the ordinary path,
  three when the evidence is weak (startup, after a gap, near-zero angles), and
  accumulated evidence to leave. Entering late means the keyboard is already
  face-down registering keypresses; asserting wrongly means the keyboard stops
  working. The second error is worse, so ambiguity always costs corroboration.
- **Slew gate.** Angular change faster than 720°/s is rejected as a glitch. The
  comparison is circular, so the genuine 359°→0° wrap is a 1° move and is never
  rejected. This is not theoretical — see [Hardware notes](#hardware-notes).
- **Lid override.** A shut lid and a fully folded hinge both read near zero. The
  lid switch is the only thing that distinguishes them, and a shut lid must
  never assert tablet mode — at any angle, not just near zero.
- **Dead-man release.** If the sensor stops answering while tablet mode is
  asserted, the switch is released anyway. Releasing must never depend on the
  sensor's cooperation, because a wedged sensor is exactly when it matters.
- **Tent posture.** A hinge angle gives you states the binary switch cannot
  express. Tent asserts `SW_TABLET_MODE` (the keyboard faces away) but is
  reported distinctly over the API.

## Safety

The worst failure in this domain is a machine left with no working input.

**Synthesis does not fail safe, and you should not assume it does.** An earlier
version of this README claimed that if the daemon dies the virtual switch
disappears and libinput re-enables everything. That is false, and the asymmetry
is visible in libinput's own source. When a tablet-mode switch device is
removed while asserting, the touchpad path calls `tp_resume()`
([`evdev-mt-touchpad.c`][tp-resume]) but the keyboard path only detaches its
listener and nulls the pointer, never calling `fallback_resume()`
([`evdev-fallback.c`][kbd-noresume]). The keyboard was suspended by closing its
fd, and only a switch event of `0` reopens it — an event a destroyed device can
never send.

So a `SIGKILL`, OOM kill or panic while asserting can return your touchpad and
leave the internal keyboard dead until the compositor rebuilds its libinput
context. The mitigations that actually work are to emit an explicit
`SW_TABLET_MODE = 0` before destroying the device on every ordinary exit path,
to emit `0` at startup so a restart clears a latched state, and to run under a
supervisor that can do the same. Those are **not yet implemented** — see the
roadmap.

If you are ever stuck with a dead keyboard, switch to a TTY and:

```sh
sudo systemctl stop hinged
```

That works because the kernel VT console reads evdev directly rather than
through libinput.

[tp-resume]: https://gitlab.freedesktop.org/libinput/libinput/-/blob/main/src/evdev-mt-touchpad.c
[kbd-noresume]: https://gitlab.freedesktop.org/libinput/libinput/-/blob/main/src/evdev-fallback.c

### Does libinput even suspend the keyboard on this hardware?

Possibly not. `fallback_pair_tablet_mode` refuses to pair a keyboard with a
tablet-mode switch unless that keyboard is tagged `EVDEV_TAG_INTERNAL_KEYBOARD`,
which comes from an `ID_INTEGRATION` udev property. On the reference machine:

```
$ udevadm info -q property -n /dev/input/event3 | grep -c ID_INTEGRATION
0                                    # the AT internal keyboard: absent
$ udevadm info -q property -n /dev/input/event8 | grep INTEGRATION
ID_INPUT_TOUCHPAD_INTEGRATION=internal   # the touchpad: present
```

So a synthesized switch may suspend the touchpad and not the keyboard here.
Confirming this with `libinput debug-events` is the first task on the roadmap,
because it decides whether switch synthesis alone is sufficient or whether a
direct-inhibition fallback is mandatory.

## Install

Requires the Go toolchain named in `go.mod`. No runtime dependencies.

```sh
git clone https://github.com/denelson1-dot/hinged-convertible
cd hinged-convertible
CGO_ENABLED=0 go build -o hinged ./cmd/hinged
./hinged doctor
```

Reading the hinge angle needs no privileges. Reading the kernel's switch devices
does; `packaging/udev/70-hinged-switch.rules` grants access to switch devices
only, and only to the locally active user. The rule is numbered 70 on purpose:
`ID_INPUT_SWITCH` is set by systemd's `60-input-id.rules`, so a rule numbered 60
sorts *before* the property exists and silently never matches on a cold boot.

```sh
sudo install -m644 packaging/udev/70-hinged-switch.rules /etc/udev/rules.d/
sudo udevadm control --reload && sudo udevadm trigger
```

This is deliberately narrower than `usermod -aG input $USER`, which would grant
your whole session read access to every input device including the keyboard.
Note that `uaccess` is implemented by systemd-logind; on OpenRC, runit or s6 the
tag does nothing and the group-based alternative in the rule file applies.

## Commands

| Command | |
|---|---|
| `hinged daemon` | Run it: synthesize the switch and fire hooks |
| `hinged doctor` | What this machine exposes, what's reachable, what would be used |
| `hinged watch` | Live posture decisions, read-only, changes nothing |
| `hinged config` | Show the loaded configuration and where it came from |
| `hinged release` | Publish `SW_TABLET_MODE=0` and exit, for recovery |

`hinged daemon --dry-run` decides and logs everything while creating no device
and running no hooks. Use it on hardware whose behaviour you do not yet trust.

## Configuration

Everything is optional; delete the file and hinged autodetects. See
[`examples/config.toml`](examples/config.toml). Unknown keys are rejected rather
than ignored — a silent typo in a file that decides whether your keyboard
switches off is not an acceptable failure mode.

All desktop behaviour lives in hooks rather than being compiled in, which is
what lets one binary serve GNOME, KDE, sway and everything else. Commands are
argv arrays, never shell strings, and every hook's exit code and stderr are
logged so a broken one is visible.

## On-screen keyboard and dictation

hinged does not ship a keyboard. A good OSK is a large project on its own and
several maintained ones exist; what is missing on a convertible is something
that knows *when* to show one. hinged drives whichever you have — Onboard,
wvkbd, squeekboard — and on GNOME and KDE deliberately does nothing, because
their shells already react to the `SW_TABLET_MODE` hinged now supplies.

For dictation it calls **[vox](https://github.com/denelson1-dot/vox)**, a
separate system-wide service. That split is deliberate: the speech model loads
once and is shared by everything on the machine, instead of every project
carrying its own copy of the same few hundred megabytes.

### Thresholds are not compiled in

The defaults are calibrated for one chassis. Every threshold can be overridden,
and an inconsistent set is rejected at startup rather than producing undefined
posture bands:

```sh
hinged watch -enter-angle 200 -leave-angle 160 -wrap-guard 15
```

| Flag | Meaning |
|---|---|
| `-enter-angle` | Assert tablet mode at or above this angle (default 210) |
| `-leave-angle` | Laptop posture at or below this angle (default 180) |
| `-wrap-guard` | Below this, a reading may be a hinge wrapped past 360 (default 30) |
| `-tablet-angle` | Fully folded at or above this angle (default 300) |
| `-max-slew` | Reject change faster than this in °/s; 0 disables (default 720) |

For reference, the ChromeOS EC ships 180° with 20° of hysteresis, and treats
under 15° as closed — so other vendors do land in a different place.

## Hardware notes

Findings from developing against an HP ENVY x360 15-bp1xx that may save you time
on similar hardware:

**Reading IIO sensor attributes from Go is unreliable.** A HID-sensor attribute
read triggers a round trip to the sensor hub, and the driver intermittently
answers `0` rather than waiting. Reading through Go's `os` package is
measurably worse than issuing the syscalls directly. Interleaved A/B at 20
samples/second with the hinge stationary:

| Method | Bad reads |
|---|---|
| `os.ReadFile` | ~3% |
| `syscall.Open` + `Read` | ~0.3% |

**The mechanism is not known.** An earlier version of this file blamed
`O_NONBLOCK` and the runtime poller. That explanation was tested and is wrong:
an `os.NewFile` wrapper around a plain *blocking* descriptor, never registered
with the poller, is affected just as badly. Something in the `os.File` read path
is responsible, but this project has not identified what. The effect replicates;
the cause is open.

Raw syscalls reduce the glitch rate by roughly an order of magnitude but do not
eliminate it, which is why the policy layer also refuses to read a near-zero
angle as a fold unless the physical path supports it. A spurious `0` is
otherwise indistinguishable from a fully folded hinge, and unfiltered it
switches your keyboard off at random.

**Angle units are not degrees by default.** The IIO ABI specifies radians after
`scale` and `offset` are applied. This machine's `in_angl_scale` is
`0.017453293` — exactly π/180 — so raw values *happen* to be degrees. That is a
coincidence, not a rule; assuming it elsewhere scales every threshold by 57×.

**Scale and offset may not be per-channel.** Here they are `in_angl_scale` and
`in_angl_offset`, device-wide, while the raw values are `in_angl0_raw` etc.

**One device, three angles.** `in_angl0_label` through `2` are `hinge`, `screen`
and `keyboard`. Only the first is the lid-to-base angle. Select by label; IIO
device numbering is not stable across boots.

**The sensor advertises its own rate.** `in_angl_sampling_frequency` is 10 Hz
here, so polling faster than 20 Hz only re-reads unchanged values.

## Roadmap

- [x] Pure policy engine with hysteresis, wrap guard, debounce, slew gate
- [x] `doctor` — permission-aware hardware probe
- [x] `watch` — live read-only posture decisions
- [x] Configurable thresholds
- [x] Dead-man release when the sensor stops reporting
- [ ] **Verify libinput honours a synthetic switch on real hardware** ← blocks everything
- [ ] `uinput` switch synthesis, with explicit release before device destroy
- [ ] Switch-health auditing and repair for lying firmware
- [ ] Command hooks and D-Bus API
- [ ] Accelerometer-pair angle derivation
- [ ] Optional offline voice dictation module

## Layout

```
policy/           the pure decision engine — importable, no I/O, no hardware
internal/source/  IIO and evdev readers
internal/probe/   unprivileged hardware discovery, behind `doctor`
internal/watch/   the live loop
cmd/hinged/       CLI
```

`policy` is deliberately **not** under `internal/`: it is the reusable part, and
it has no dependency on anything else here.

## Testing

Everything decision-shaped is tested without hardware. `policy` is pure and
covered at ~91% including a fuzz target; `source` and `probe` are tested against
sysfs fixtures and captured `/proc/bus/input/devices` records, including a
malicious device name that tries to forge a record.

| Package | Coverage |
|---|---|
| `policy` | 91% |
| `internal/probe` | 49% |
| `internal/source` | 25% |
| `internal/watch`, `cmd/hinged` | 0% |
| **total** | **33%** |

The live loop and the CLI are the untested parts. They are mostly I/O
orchestration, but that is an explanation rather than an excuse.

```sh
go test ./...
go test -coverpkg=./... ./...      # coverage across all packages
```

CI additionally runs `staticcheck`, `govulncheck`, `go mod tidy -diff`, and
builds for `amd64`, `arm64`, `arm` and `386`. That matrix is not decoration:
`sizeof(struct input_event)` differs on 32-bit, and the wrong value compiles
cleanly everywhere while silently desynchronising the event stream.

## Prior art

`hinged` is meant to interoperate with these, not replace them.

- **[libinput][switches]** — reacts to `SW_TABLET_MODE`. Does the actual
  disabling. `hinged` gives it a switch to react to.
- **[iio-sensor-proxy]** — the standard rotation provider. Explicitly
  [declined][isp199] tablet-mode detection. `hinged` leaves rotation to it.
- **libinput device quirks** — if your machine's problem is *lying* firmware
  rather than *missing* firmware, `ModelTabletModeSwitchUnreliable` in libinput's
  quirks database may fix it with no daemon, no `/dev/uinput` and no privileges.
  Try that first.
- **[minibook-dual-accelerometer]** — the best existing thresholding policy, and
  the source of the jerk/tilt-gating ideas here. Chuwi MiniBook only, needs a
  DKMS module.
- **[asus-accel-tablet-mode-driver]** — accelerometer-derived tablet mode,
  Asus only.

## License

MIT

[switches]: https://wayland.freedesktop.org/libinput/doc/latest/switches.html
[iio-sensor-proxy]: https://gitlab.freedesktop.org/hadess/iio-sensor-proxy
[isp199]: https://github.com/hadess/iio-sensor-proxy/issues/199
[minibook-dual-accelerometer]: https://github.com/rhalkyard/minibook-dual-accelerometer
[asus-accel-tablet-mode-driver]: https://github.com/asus-linux-drivers/asus-accel-tablet-mode-driver
