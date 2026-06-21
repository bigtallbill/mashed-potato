// Package tray provides the system-tray (taskbar) icon for mashed-potato.
//
// On Linux it uses fyne.io/systray, whose backend speaks the StatusNotifierItem
// D-Bus protocol directly — no cgo and no GTK/AppIndicator dependency, so the
// binary stays static. A session bus and an SNI-capable tray are required at
// runtime; Available reports whether that's the case so headless contexts can
// skip the tray cleanly.
package tray

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os/exec"

	"fyne.io/systray"
	"github.com/godbus/dbus/v5"
)

// Options configures the tray.
type Options struct {
	URL    string                  // dashboard URL to open
	Jobs   []string                // job names for the "Run" submenu
	RunJob func(name string) error // trigger a backup (in-process)
	Browse func() error            // mount the repo + open it in a file manager
	OnExit func()                  // called once the tray loop ends (e.g. shut down HTTP)
	// Register, if set, is called with a status updater the tray installs so the
	// server can push live status (text + state: idle|running|error).
	Register func(set func(text, state string))
}

// Available reports whether a usable session bus is reachable. If not, there's
// no point starting the tray (it would have nowhere to register).
func Available() bool {
	conn, err := dbus.SessionBus()
	if err != nil {
		return false
	}
	// SessionBus returns a shared connection; don't close it.
	return conn != nil
}

// Run starts the tray and blocks until Quit is called (via the menu or Stop).
// It must run on the main goroutine.
func Run(opts Options) {
	systray.Run(func() { onReady(opts) }, func() {
		if opts.OnExit != nil {
			opts.OnExit()
		}
	})
}

// Stop requests the tray loop to exit (safe to call from any goroutine, e.g. on
// SIGINT). It is a no-op if the tray never started.
func Stop() { systray.Quit() }

func onReady(opts Options) {
	systray.SetIcon(iconFor("idle"))
	systray.SetTitle("")
	systray.SetTooltip("mashed-potato — " + opts.URL)

	mStatus := systray.AddMenuItem("idle", "current status")
	mStatus.Disable()
	systray.AddSeparator()

	mOpen := systray.AddMenuItem("Open dashboard", "Open the mashed-potato web UI")
	mBrowse := systray.AddMenuItem("Browse snapshots", "Mount the repo and open it in a file manager")
	if opts.Browse == nil {
		mBrowse.Disable()
	}
	systray.AddSeparator()

	mRun := systray.AddMenuItem("Run job", "Run a backup now")
	if len(opts.Jobs) == 0 {
		mRun.Disable()
	}
	for _, name := range opts.Jobs {
		item := mRun.AddSubMenuItem(name, "Run "+name+" now")
		go func(job string, clicked <-chan struct{}) {
			for range clicked {
				if opts.RunJob != nil {
					_ = opts.RunJob(job)
				}
			}
		}(name, item.ClickedCh)
	}

	systray.AddSeparator()
	mQuit := systray.AddMenuItem("Quit", "Stop mashed-potato")

	if opts.Register != nil {
		opts.Register(func(text, state string) {
			mStatus.SetTitle(text)
			systray.SetTooltip("mashed-potato — " + text)
			systray.SetIcon(iconFor(state))
		})
	}

	go func() {
		for {
			select {
			case <-mOpen.ClickedCh:
				openBrowser(opts.URL)
			case <-mBrowse.ClickedCh:
				if opts.Browse != nil {
					_ = opts.Browse()
				}
			case <-mQuit.ClickedCh:
				systray.Quit()
				return
			}
		}
	}()
}

func openBrowser(url string) {
	// xdg-open is the desktop-agnostic launcher (xdg-utils on NixOS).
	_ = exec.Command("xdg-open", url).Start()
}

// iconFor returns a state-colored disc: gold (idle), green (running), red (error).
func iconFor(state string) []byte {
	switch state {
	case "running":
		return discPNG(color.RGBA{0x5f, 0xa5, 0x64, 0xff})
	case "error":
		return discPNG(color.RGBA{0xd4, 0x62, 0x3f, 0xff})
	default:
		return discPNG(color.RGBA{0xd8, 0xa6, 0x57, 0xff}) // accent / idle
	}
}

// discPNG draws a simple filled disc on a transparent background so we don't need
// to ship a binary image asset.
func discPNG(fg color.RGBA) []byte {
	const size = 64
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	edge := color.RGBA{0x2a, 0x24, 0x20, 0xff}
	c, r := float64(size-1)/2, float64(size)/2-2
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			dx, dy := float64(x)-c, float64(y)-c
			d := dx*dx + dy*dy
			switch {
			case d <= (r-2)*(r-2):
				img.Set(x, y, fg)
			case d <= r*r:
				img.Set(x, y, edge)
			}
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}
