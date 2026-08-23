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
- **Wrap guard.** The sensor wraps 360→0 at full fold, so a reading of 5° means
  *folded all the way back*, not *lying open flat*. Without this the machine
  flips to laptop mode at the exact moment it becomes a tablet.
- **Asymmetric debounce.** One sample to enter tablet mode, three to leave.
  Entering late means the keyboard is already face-down registering keypresses;
  leaving early is merely inconvenient.
- **Slew gate.** Angular change faster than 720°/s is rejected as a glitch. The
  comparison is circular, so the genuine 359°→0° wrap is a 1° move and is never
  rejected. This is not theoretical — see [Hardware notes](#hardware-notes).
- **Lid override.** A shut lid and a fully folded hinge both read near zero. The
  lid switch is the only thing that distinguishes them, and a shut lid must
  never assert tablet mode.
- **Tent posture.** A hinge angle gives you states the binary switch cannot
  express. Tent asserts `SW_TABLET_MODE` (the keyboard faces away) but is
  reported distinctly over the API.

## Safety

The worst failure in this domain is a machine left with no working input. Design
rules, in order of importance:

1. **Synthesis fails safe.** If the daemon dies, the virtual switch disappears
   and libinput re-enables everything by itself. This is a structural advantage
   over `xinput disable` and over the `inhibited` sysfs knob, neither of which
   restores itself.
2. **Release before cleanup.** Returning to laptop mode restores input first and
   does slower work after.
3. **A dead-man timer** releases the switch if the sensor stops reporting.

If you are ever stuck with a dead keyboard, switch to a TTY and:

```sh
sudo systemctl stop hinged
```

## Install

Requires Go 1.24+ to build. No runtime dependencies.

```sh
git clone https://github.com/denelson1-dot/hinged-convertible
cd hinged-convertible
CGO_ENABLED=0 go build -o hinged ./cmd/hinged
./hinged doctor
```

Reading the hinge angle needs no privileges. Reading the kernel's switch devices
does; `packaging/udev/60-hinged-switch.rules` grants access to switch devices
only — not keyboards, touchpads or touchscreens — and only to the locally
active user:

```sh
sudo install -m644 packaging/udev/60-hinged-switch.rules /etc/udev/rules.d/
sudo udevadm control --reload && sudo udevadm trigger
```

This is deliberately narrower than `usermod -aG input $USER`, which would grant
your whole session read access to every input device including the keyboard.

## Commands

| Command | |
|---|---|
| `hinged doctor` | What this machine exposes, what's reachable, what would be used |
| `hinged watch` | Live posture decisions, read-only, changes nothing |

## Hardware notes

Findings from developing against an HP ENVY x360 15-bp1xx that may save you time
on similar hardware:

**Reading IIO sensor attributes from Go is unreliable.** A HID-sensor attribute
read triggers a round trip to the sensor hub, and the driver intermittently
answers `0` rather than waiting. Go's `os` package makes this much worse,
because it opens pollable descriptors non-blocking and registers them with the
runtime poller. Measured at 20 samples/second with the hinge stationary at 110°:

| Method | Bad reads |
|---|---|
| `os.ReadFile` | 3.7% (11 of 293) |
| `syscall.Open` + `Read` | 0.24% (1 of 418) |

Raw syscalls cut it roughly fifteen-fold but do not eliminate it, which is why
the slew gate exists. A spurious `0` is indistinguishable from a fully folded
hinge — unfiltered, it switches your keyboard off at random.

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
- [ ] `uinput` switch synthesis ← the core deliverable
- [ ] Switch-health auditing and repair for lying firmware
- [ ] Command hooks and D-Bus API
- [ ] Accelerometer-pair angle derivation
- [ ] Optional offline voice dictation module

## Prior art

`hinged` is meant to interoperate with these, not replace them.

- **[libinput][switches]** — reacts to `SW_TABLET_MODE`. Does the actual
  disabling. `hinged` gives it a switch to react to.
- **[iio-sensor-proxy]** — the standard rotation provider. Explicitly
  [declined][isp199] tablet-mode detection. `hinged` leaves rotation to it.
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
