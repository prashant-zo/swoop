//go:build darwin || linux

package main

import (
	"context"
	"os/exec"
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
