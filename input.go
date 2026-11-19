package main

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"unicode/utf8"
)

// One goroutine reads the keyboard for the whole program and hands chunks
// to whoever is listening (the prompt, a download, the downloads list).
// A single reader means no screen ever steals keys meant for another.

var (
	inputCh = make(chan []byte, 64)
	rawKeys bool   // keyboard is in key-by-key mode
	keyBuf  []key  // parsed keys not consumed yet
	lineBuf []byte // fallback line mode: bytes not consumed yet
)

type key struct {
	name string // "rune", "enter", "esc", "up", "down", "left", "right", "backspace", "ctrl-c", "ctrl-d", "ctrl-u", "tab"
	r    rune
}

func startInput() {
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				b := make([]byte, n)
				copy(b, buf[:n])
				inputCh <- b
			}
			if err != nil {
				close(inputCh)
				return
			}
		}
	}()
}

func parseKeys(b []byte) []key {
	var ks []key
	for i := 0; i < len(b); {
		c := b[i]
		switch {
		case c == 0x1b:
			if i+1 < len(b) && (b[i+1] == '[' || b[i+1] == 'O') {
				j := i + 2
				for j < len(b) && !(b[j] >= 0x40 && b[j] <= 0x7e) {
					j++
				}
				if j < len(b) {
					switch b[j] {
					case 'A':
						ks = append(ks, key{name: "up"})
					case 'B':
						ks = append(ks, key{name: "down"})
					case 'C':
						ks = append(ks, key{name: "right"})
					case 'D':
						ks = append(ks, key{name: "left"})
					}
					i = j + 1 // also swallows bracketed-paste markers and other sequences
					continue
				}
			}
			ks = append(ks, key{name: "esc"})
			i++
		case c == '\r' || c == '\n':
			ks = append(ks, key{name: "enter"})
			if c == '\r' && i+1 < len(b) && b[i+1] == '\n' {
				i++
			}
			i++
		case c == 3:
			ks = append(ks, key{name: "ctrl-c"})
			i++
		case c == 4:
			ks = append(ks, key{name: "ctrl-d"})
			i++
		case c == 21:
			ks = append(ks, key{name: "ctrl-u"})
			i++
		case c == 127 || c == 8:
			ks = append(ks, key{name: "backspace"})
			i++
		case c == '\t':
			ks = append(ks, key{name: "tab"})
			i++
		case c < 32:
			i++
		default:
			r, size := utf8.DecodeRune(b[i:])
			ks = append(ks, key{name: "rune", r: r})
			i += size
		}
	}
	return ks
}

// nextKey blocks until a key is pressed. ok is false when input is gone.
func nextKey() (key, bool) {
	for len(keyBuf) == 0 {
		b, ok := <-inputCh
		if !ok {
			return key{}, false
		}
		keyBuf = append(keyBuf, parseKeys(b)...)
	}
	k := keyBuf[0]
	keyBuf = keyBuf[1:]
	return k, true
}

// takeKeys returns keys already waiting, without blocking.
func takeKeys() []key {
	for {
		select {
		case b, ok := <-inputCh:
			if !ok {
				ks := keyBuf
				keyBuf = nil
				return ks
			}
			keyBuf = append(keyBuf, parseKeys(b)...)
			continue
		default:
		}
		ks := keyBuf
		keyBuf = nil
		return ks
	}
}

// drainKeys drops keys pressed during a download so they don't leak into
// the prompt afterwards.
func drainKeys() { takeKeys() }

// promptLine shows a prompt and reads one line with simple editing:
// typing, paste, Backspace, Esc or Ctrl+U to clear. ok is false on Ctrl+C,
// or Ctrl+D on an empty line.
func promptLine(prompt string) (string, bool) {
	if !rawKeys {
		fmt.Print(prompt)
		return readCookedLine()
	}
	var buf []rune
	rows := 1
	redraw := func() {
		var b strings.Builder
		b.WriteString("\r")
		if rows > 1 {
			fmt.Fprintf(&b, "\x1b[%dA", rows-1)
		}
		b.WriteString("\x1b[J")
		b.WriteString(prompt)
		b.WriteString(string(buf))
		os.Stdout.WriteString(b.String())
	}
	count := func() int {
		w := termWidth()
		if w < 1 {
			w = 80
		}
		return (vlen(prompt)+len(buf))/w + 1
	}
	os.Stdout.WriteString(prompt)
	for {
		k, ok := nextKey()
		if !ok {
			fmt.Print("\r\n")
			return "", false
		}
		// handle everything already waiting in one go, so a big paste is fast
		ks := append([]key{k}, takeKeys()...)
		var echo strings.Builder
		for i, k := range ks {
			switch k.name {
			case "enter":
				os.Stdout.WriteString(echo.String())
				os.Stdout.WriteString("\r\n")
				keyBuf = append(ks[i+1:], keyBuf...) // keep pasted lines for later
				return string(buf), true
			case "ctrl-c":
				os.Stdout.WriteString(echo.String() + "\r\n")
				return "", false
			case "ctrl-d":
				if len(buf) == 0 {
					os.Stdout.WriteString(echo.String() + "\r\n")
					return "", false
				}
			case "backspace":
				if len(buf) > 0 {
					os.Stdout.WriteString(echo.String())
					echo.Reset()
					buf = buf[:len(buf)-1]
					redraw()
					rows = count()
				}
			case "esc", "ctrl-u":
				os.Stdout.WriteString(echo.String())
				echo.Reset()
				buf = nil
				redraw()
				rows = count()
			case "rune":
				buf = append(buf, k.r)
				echo.WriteRune(k.r)
			}
		}
		os.Stdout.WriteString(echo.String())
		rows = count()
	}
}

// readCookedLine is the fallback when the keyboard can't do key-by-key mode
// (for example when input is piped in): the terminal hands over whole lines.
func readCookedLine() (string, bool) {
	for {
		if i := bytes.IndexAny(lineBuf, "\r\n"); i >= 0 {
			line := string(lineBuf[:i])
			rest := lineBuf[i+1:]
			if lineBuf[i] == '\r' && len(rest) > 0 && rest[0] == '\n' {
				rest = rest[1:]
			}
			lineBuf = rest
			return line, true
		}
		b, ok := <-inputCh
		if !ok {
			if len(lineBuf) > 0 {
				line := string(lineBuf)
				lineBuf = nil
				return line, true
			}
			return "", false
		}
		lineBuf = append(lineBuf, b...)
	}
}
