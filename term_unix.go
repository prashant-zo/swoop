//go:build darwin || linux

package main

import (
	"context"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"time"
	"unsafe"
)

type winsize struct{ Row, Col, X, Y uint16 }

func termWidth() int {
	var ws winsize
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(syscall.Stdout),
		uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(&ws)))
	if e != 0 || ws.Col == 0 {
		return 80
	}
	return int(ws.Col)
}

func enableVT() bool { return true }

var savedTermios *syscall.Termios

func ioctlTermios(req uintptr, t *syscall.Termios) bool {
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(syscall.Stdin), req, uintptr(unsafe.Pointer(t)))
	return e == 0
}

// setRaw switches the keyboard to key-by-key mode: no echo, no waiting for
// Enter, Ctrl+C arrives as a key instead of killing the program.
func setRaw() bool {
	var t syscall.Termios
	if !ioctlTermios(ioctlGetTermios, &t) {
		return false
	}
	if savedTermios == nil {
		s := t
		savedTermios = &s
	}
	r := *savedTermios
	r.Lflag &^= syscall.ICANON | syscall.ECHO | syscall.ISIG | syscall.IEXTEN
	r.Iflag &^= syscall.IXON | syscall.ICRNL
	r.Cc[syscall.VMIN] = 1
	r.Cc[syscall.VTIME] = 0
	return ioctlTermios(ioctlSetTermios, &r)
}

// setCooked puts the keyboard back the way we found it.
func setCooked() {
	if savedTermios != nil {
		ioctlTermios(ioctlSetTermios, savedTermios)
	}
}

func readClipboard() string {
	cmds := [][]string{{"pbpaste"}}
	if runtime.GOOS == "linux" {
		cmds = [][]string{{"wl-paste", "-n"}, {"xclip", "-o", "-selection", "clipboard"}, {"xsel", "-ob"}}
	}
	for _, cmd := range cmds {
		ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
		out, err := exec.CommandContext(ctx, cmd[0], cmd[1:]...).Output()
		cancel()
		if err == nil {
			return string(out)
		}
	}
	return ""
}

func openDir(dir string) error {
	cmd := "xdg-open"
	if runtime.GOOS == "darwin" {
		cmd = "open"
	}
	return exec.Command(cmd, dir).Start()
}

// revealFile opens the folder with the file selected (Finder on macOS).
func revealFile(path string) error {
	if runtime.GOOS == "darwin" {
		return exec.Command("open", "-R", path).Start()
	}
	return exec.Command("xdg-open", filepath.Dir(path)).Start()
}
