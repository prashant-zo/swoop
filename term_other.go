//go:build !darwin && !linux && !windows

package main

import "errors"

func termWidth() int               { return 80 }
func enableVT() bool               { return true }
func setRaw() bool                 { return false }
func setCooked()                   {}
func readClipboard() string        { return "" }
func openDir(dir string) error     { return errors.New("not supported on this OS") }
func revealFile(path string) error { return errors.New("not supported on this OS") }
