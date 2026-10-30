// Package browser launches the user's installed Chromium-family browser with a
// dedicated agent profile. Relo never downloads or bundles a browser and never
// touches the personal profile.
package browser

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// ErrNotFound means no supported Chromium-family browser is installed.
var ErrNotFound = errors.New("browser: no Chromium-family browser found; install Chrome, Chromium, Brave, Edge or Vivaldi, or set a browser path")

// Installed is a browser binary found on this machine.
type Installed struct {
	Path   string
	Family string
}

type candidate struct {
	family string
	names  []string // executable names looked up on PATH (linux)
	mac    []string // app bundle executables (darwin)
	win    []string // paths relative to %ProgramFiles%, %ProgramFiles(x86)% or %LOCALAPPDATA% (windows)
}

// candidates are ordered by preference.
var candidates = []candidate{
	{
		family: "chrome",
		names:  []string{"google-chrome-stable", "google-chrome"},
		mac:    []string{"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"},
		win:    []string{`Google\Chrome\Application\chrome.exe`},
	},
	{
		family: "chromium",
		names:  []string{"chromium", "chromium-browser"},
		mac:    []string{"/Applications/Chromium.app/Contents/MacOS/Chromium"},
		win:    []string{`Chromium\Application\chrome.exe`},
	},
	{
		family: "brave",
		names:  []string{"brave-browser", "brave"},
		mac:    []string{"/Applications/Brave Browser.app/Contents/MacOS/Brave Browser"},
		win:    []string{`BraveSoftware\Brave-Browser\Application\brave.exe`},
	},
	{
		family: "edge",
		names:  []string{"microsoft-edge-stable", "microsoft-edge"},
		mac:    []string{"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge"},
		win:    []string{`Microsoft\Edge\Application\msedge.exe`},
	},
	{
		family: "vivaldi",
		names:  []string{"vivaldi-stable", "vivaldi"},
		mac:    []string{"/Applications/Vivaldi.app/Contents/MacOS/Vivaldi"},
		win:    []string{`Vivaldi\Application\vivaldi.exe`},
	},
}

// Find returns the browser to use. A non-empty override must point at an
// executable file and is used as-is; otherwise the first installed candidate wins.
func Find(override string) (Installed, error) {
	if override != "" {
		if err := checkExecutable(override); err != nil {
			return Installed{}, fmt.Errorf("browser: configured path %q: %w", override, err)
		}
		return Installed{Path: override, Family: familyOf(filepath.Base(override))}, nil
	}
	for _, c := range candidates {
		for _, p := range c.paths() {
			if checkExecutable(p) == nil {
				return Installed{Path: p, Family: c.family}, nil
			}
		}
	}
	return Installed{}, ErrNotFound
}

func (c candidate) paths() []string {
	var out []string
	switch runtime.GOOS {
	case "linux":
		for _, n := range c.names {
			if p, err := exec.LookPath(n); err == nil {
				out = append(out, p)
			}
		}
	case "darwin":
		out = append(out, c.mac...)
	case "windows":
		for _, root := range []string{os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)"), os.Getenv("LOCALAPPDATA")} {
			if root == "" {
				continue
			}
			for _, rel := range c.win {
				out = append(out, filepath.Join(root, rel))
			}
		}
	}
	return out
}

func checkExecutable(p string) error {
	fi, err := os.Stat(p)
	if err != nil {
		return err
	}
	if fi.IsDir() {
		return errors.New("is a directory")
	}
	if runtime.GOOS != "windows" && fi.Mode()&0o111 == 0 {
		return errors.New("not executable")
	}
	return nil
}

func familyOf(base string) string {
	for _, c := range candidates {
		for _, n := range c.names {
			if base == n {
				return c.family
			}
		}
	}
	return "custom"
}
