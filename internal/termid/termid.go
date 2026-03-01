// Package termid sets the terminal tab title, icon, and color for Mantis.
// Uses OSC escape sequences supported by iTerm2, VS Code, Warp, Kitty, and most modern terminals.
package termid

import (
	"fmt"
	"os"
	"strings"
)

const (
	// OSC escape sequences
	oscStart = "\033]"
	oscEnd   = "\007"
	// ST terminator (alternative to BEL, needed by some terminals)
	oscEndST = "\033\\"
)

// Set applies Mantis terminal identity:
// - Tab/window title:  🦗 Mantis
// - Tab color:         copper (#D4843A) — matches brand
// - VS Code terminal title displayed in tab
// Called once on REPL startup.
func Set(username string) {
	if !isTerminal() {
		return
	}

	title := "🦗 Mantis"
	if username != "" {
		title = "🦗 Mantis — " + username
	}

	// OSC 0: set icon name AND window title (universally supported)
	fmt.Printf("%s0;%s%s", oscStart, title, oscEnd)

	// OSC 1: set icon name only (tab title in some terminals)
	fmt.Printf("%s1;%s%s", oscStart, title, oscEnd)

	// iTerm2: set tab color to copper (#D4 = 212, #84 = 132, #3A = 58)
	if isITerm2() {
		setITerm2TabColor(212, 132, 58)
		setITerm2Badge("🦗")
	}

	// VS Code: set the terminal name shown in the tab list
	// Uses the unofficial sequence VS Code reads for tab label
	if isVSCode() {
		fmt.Printf("%s0;%s%s", oscStart, title, oscEnd)
	}
}

// Clear restores the terminal title to the shell on exit.
func Clear() {
	if !isTerminal() {
		return
	}
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "shell"
	}
	// Extract just the shell name (e.g. "zsh" from "/bin/zsh")
	parts := strings.Split(shell, "/")
	fmt.Printf("%s0;%s%s", oscStart, parts[len(parts)-1], oscEnd)
	fmt.Printf("%s1;%s%s", oscStart, parts[len(parts)-1], oscEnd)
}

func setITerm2TabColor(r, g, b int) {
	// iTerm2 proprietary: set tab color
	fmt.Printf("%s6;1;bg;red;brightness;%d%s", oscStart, r, oscEnd)
	fmt.Printf("%s6;1;bg;green;brightness;%d%s", oscStart, g, oscEnd)
	fmt.Printf("%s6;1;bg;blue;brightness;%d%s", oscStart, b, oscEnd)
}

func setITerm2Badge(text string) {
	// Show badge in top-right corner of the terminal pane
	encoded := encodeBase64(text)
	fmt.Printf("%s1337;SetBadgeFormat=%s%s", oscStart, encoded, oscEnd)
}

func isTerminal() bool {
	// Check if stdout is a real TTY
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

func isITerm2() bool {
	return os.Getenv("TERM_PROGRAM") == "iTerm.app"
}

func isVSCode() bool {
	return os.Getenv("TERM_PROGRAM") == "vscode" ||
		os.Getenv("VSCODE_INJECTION") != "" ||
		os.Getenv("VSCODE_SHELL_INTEGRATION") != ""
}

// encodeBase64 encodes s in base64 (used by iTerm2 badge protocol).
func encodeBase64(s string) string {
	const chars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	b := []byte(s)
	var out strings.Builder
	for i := 0; i < len(b); i += 3 {
		var buf [3]byte
		n := copy(buf[:], b[i:])
		out.WriteByte(chars[buf[0]>>2])
		out.WriteByte(chars[(buf[0]&0x03)<<4|buf[1]>>4])
		if n > 1 {
			out.WriteByte(chars[(buf[1]&0x0f)<<2|buf[2]>>6])
		} else {
			out.WriteByte('=')
		}
		if n > 2 {
			out.WriteByte(chars[buf[2]&0x3f])
		} else {
			out.WriteByte('=')
		}
	}
	return out.String()
}
