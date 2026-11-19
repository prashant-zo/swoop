package main

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// listView shows the downloads list. It returns an entry to continue, or nil.
func listView(st *Store) *Entry {
	if len(st.Entries) == 0 {
		fmt.Println("  " + c(dimClr, "No downloads yet. Paste a link to start one."))
		return nil
	}
	if !rawKeys {
		for i := len(st.Entries) - 1; i >= 0; i-- {
			fmt.Println("  " + listRow(st.Entries[i], false, termWidth()-4))
		}
		return nil
	}
	sel := 0
	lines := 0
	flash := ""
	confirm := false
	fmt.Print(hideCursor)
	defer fmt.Print(showCursor)

	// newest first
	items := func() []*Entry {
		out := make([]*Entry, 0, len(st.Entries))
		for i := len(st.Entries) - 1; i >= 0; i-- {
			out = append(out, st.Entries[i])
		}
		return out
	}
	draw := func() {
		list := items()
		if sel >= len(list) {
			sel = len(list) - 1
		}
		w := termWidth() - 4
		if w > 100 {
			w = 100
		}
		var b strings.Builder
		if lines > 0 {
			fmt.Fprintf(&b, "\x1b[%dF\x1b[J", lines)
		}
		n := 0
		put := func(s string) { b.WriteString(s + "\n"); n++ }
		put("  " + c(boldClr, "Your downloads") + c(dimClr, fmt.Sprintf("  (%d)", len(list))))
		const window = 12
		start := 0
		if sel >= window {
			start = sel - window + 1
		}
		for i := start; i < len(list) && i < start+window; i++ {
			if i == sel {
				put(c(boldClr+accent, "  ❯ ") + listRow(list[i], true, w-4))
			} else {
				put("    " + listRow(list[i], false, w-4))
			}
		}
		if len(list) > window {
			put(c(dimClr, fmt.Sprintf("    … %d more, use ↑ ↓", len(list)-window)))
		}
		put("")
		switch {
		case confirm:
			put("  " + c(warnClr, "Delete "+truncate(list[sel].Name, 40)+" from your computer too?") + "  " + c(boldClr, "y") + c(dimClr, " yes · ") + c(boldClr, "n") + c(dimClr, " no"))
		case flash != "":
			put("  " + c(okClr, flash))
		default:
			k := func(s string) string { return c(boldClr, s) }
			e := list[sel]
			enter := " open"
			if e.Status != "done" {
				enter = " continue"
			}
			put("  " + k("↑↓") + c(dimClr, " choose  ") + k("Enter") + c(dimClr, enter+"  ") + k("o") + c(dimClr, " show in folder  ") +
				k("d") + c(dimClr, " remove from list  ") + k("x") + c(dimClr, " delete file too  ") + k("Esc") + c(dimClr, " back"))
		}
		lines = n
		os.Stdout.WriteString(b.String())
	}
	clear := func() {
		if lines > 0 {
			fmt.Fprintf(os.Stdout, "\x1b[%dF\x1b[J", lines)
			lines = 0
		}
	}

	draw()
	for {
		k, ok := nextKey()
		if !ok {
			clear()
			return nil
		}
		list := items()
		e := list[sel]
		flash = ""
		if confirm {
			confirm = false
			if k.name == "rune" && (k.r == 'y' || k.r == 'Y') {
				e.deleteFiles()
				st.remove(e)
				flash = "deleted " + truncate(e.Name, 50)
				if len(st.Entries) == 0 {
					clear()
					fmt.Println("  " + c(okClr, flash))
					return nil
				}
			}
			draw()
			continue
		}
		switch {
		case k.name == "up":
			if sel > 0 {
				sel--
			}
		case k.name == "down":
			if sel < len(list)-1 {
				sel++
			}
		case k.name == "esc" || k.name == "ctrl-c" || (k.name == "rune" && (k.r == 'q' || k.r == 'Q')):
			clear()
			return nil
		case k.name == "enter":
			if e.Status != "done" {
				clear()
				return e
			}
			if err := revealFile(e.Path); err != nil || !exists(e.Path) {
				flash = "that file isn't there anymore"
			} else {
				flash = "opened the folder"
			}
		case k.name == "rune" && (k.r == 'o' || k.r == 'O'):
			target := e.Path
			if !exists(target) {
				target = e.Dir
				openDir(target)
			} else {
				revealFile(target)
			}
			flash = "opened the folder"
		case k.name == "rune" && (k.r == 'd' || k.r == 'D'):
			st.remove(e)
			flash = "removed from list, the file is still on your computer"
			if len(st.Entries) == 0 {
				clear()
				fmt.Println("  " + c(okClr, flash))
				return nil
			}
		case k.name == "rune" && (k.r == 'x' || k.r == 'X'):
			confirm = true
		}
		draw()
	}
}

func listRow(e *Entry, selected bool, width int) string {
	var icon, status string
	switch e.Status {
	case "done":
		icon, status = c(okClr, "✓"), c(dimClr, "done "+ago(e.Updated))
	case "failed":
		icon, status = c(errClr, "✗"), c(errClr, "failed")
	default: // paused, or "downloading" left over from a closed window
		icon = c(warnClr, "■")
		if e.Size > 0 {
			status = c(warnClr, fmt.Sprintf("paused %.0f%%", float64(e.Done)*100/float64(e.Size)))
		} else {
			status = c(warnClr, "paused")
		}
	}
	size := ""
	if e.Size > 0 {
		size = fmtBytes(e.Size)
	}
	kind := ""
	if e.Kind == "torrent" {
		kind = c(dimClr, " torrent")
	}
	nameW := width - 34
	if nameW < 12 {
		nameW = 12
	}
	name := truncate(e.Name, nameW)
	if selected {
		name = c(boldClr, name)
	}
	return fmt.Sprintf("%s  %s%s%s  %10s  %s", icon, name, strings.Repeat(" ", max(0, nameW-vlen(truncate(e.Name, nameW)))), kind, size, status)
}

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
