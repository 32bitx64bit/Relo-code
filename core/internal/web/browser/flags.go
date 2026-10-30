package browser

import (
	"os"
	"runtime"
)

// Lane selects how the browser is run.
type Lane string

const (
	// LaneHidden runs --headless=new with a dedicated profile. Default lane.
	LaneHidden Lane = "hidden"
	// LaneWindow runs a headed window with its own profile. Used only to clear
	// challenges that the hidden lane cannot pass.
	LaneWindow Lane = "window"
)

// Flags never passed to the browser. They mark the session as automated or open
// a network debugging endpoint, both of which sites can detect.
var forbiddenFlags = []string{
	"--enable-automation",
	"--remote-debugging-port",
}

// hiddenScreen is the reported screen for the hidden lane. Chromium requires the
// braced form; "1920x1080" alone aborts the browser.
const hiddenScreen = "{1920x1080}"

// windowSize is the initial size of both lanes. The window lane needs it too:
// without it the escalation window reuses whatever size the dedicated profile
// was last left at, so the coordinates the agent clicks are not the coordinates
// the window is showing.
const windowSize = "1920,1080"

// buildArgs returns the command-line flags for lane. profileDir must be the
// lane's dedicated profile; proxy is optional and passed verbatim. softwareGL
// drops the measured ANGLE flags, for hosts where they do not start.
func buildArgs(lane Lane, profileDir, proxy string, softwareGL bool) []string {
	args := []string{
		"--remote-debugging-pipe",
		"--user-data-dir=" + profileDir,
		"--no-first-run",
		"--no-default-browser-check",
		"--password-store=basic",
		"--use-mock-keychain",
		// WebRTC must not leak the local/private address outside the proxy.
		"--force-webrtc-ip-handling-policy=disable_non_proxied_udp",
		// Measured: without this, navigator.webdriver is true under the pipe.
		// Both lanes need it, because the window lane is judged by the same
		// pages as the hidden one. Blink feature toggle only; no JS property is
		// patched.
		"--disable-blink-features=AutomationControlled",
		"--window-size=" + windowSize,
	}
	if !softwareGL {
		// Measured: without these the WebGL renderer reports the software
		// rasteriser string that gives headless Chromium away.
		args = append(args,
			"--use-gl=angle",
			"--use-angle=gl",
		)
	}
	if runtime.GOOS == "linux" && os.Geteuid() == 0 {
		// Chromium refuses to start as root without this. Root sessions are the
		// caller's choice; the sandbox is then off for this browser only.
		args = append(args, "--no-sandbox")
	}
	if lane == LaneHidden {
		args = append(args,
			"--headless=new",
			"--screen-info="+hiddenScreen,
		)
	}
	if proxy != "" {
		// Measured on Chromium 154: Chromium bypasses its proxy for loopback,
		// private and link-local addresses by default, so a 127.0.0.1 or
		// localhost request went straight to the origin and never reached the
		// netguard proxy. The leading "<-loopback>" drops loopback from the
		// implicit bypass list, so those requests are routed through the proxy
		// with everything else. Both lanes need it: the escalation window is
		// pointed at the same guard proxy as the hidden lane.
		args = append(args,
			"--proxy-server="+proxy,
			"--proxy-bypass-list=<-loopback>",
		)
	}
	return args
}
