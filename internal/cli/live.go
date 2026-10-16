package cli

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// liveDelay is how long a live table stays blank: listings served from
// the cache arrive at once, and drawing them only to erase them flickers.
const liveDelay = 250 * time.Millisecond

// liveTable draws a table on the terminal while it fills: render returns
// the table so far with a line under it that names what is still
// awaited.
// Each update erases the previous drawing and draws anew; finish erases
// the last one, so that the final output is printed the usual way and
// equals what a pipe gets. Lines are cut at the terminal's width, and a
// drawing taller than the terminal is not drawn, because it could not be
// erased.
type liveTable struct {
	w    io.Writer
	size func() (int, int, bool)

	mu     sync.Mutex
	render func() string
	lines  int
	shown  bool
	done   bool
	timer  *time.Timer
}

// newLiveTable starts a live table on w, the terminal whose size size
// reports; it draws nothing before liveDelay has passed.
func newLiveTable(w io.Writer, size func() (int, int, bool), render func() string) *liveTable {
	t := &liveTable{w: w, size: size, render: render}
	t.timer = time.AfterFunc(liveDelay, func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		t.shown = true
		t.draw()
	})
	return t
}

// update runs change, which changes what render reads, and redraws;
// render never runs at the same time as change. When change says nothing
// more is awaited, the drawing is erased instead, for the final output.
func (t *liveTable) update(change func() (more bool)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !change() {
		t.done = true
		t.erase()
		return
	}
	if t.shown {
		t.draw()
	}
}

// finish erases the drawing, if update has not already; nothing is drawn
// afterwards.
func (t *liveTable) finish() {
	t.timer.Stop()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.done = true
	t.erase()
}

func (t *liveTable) draw() {
	if t.done {
		return
	}
	width, height, ok := t.size()
	if !ok {
		return
	}
	lines := strings.Split(strings.TrimSuffix(t.render(), "\n"), "\n")
	if len(lines) >= height {
		return
	}
	// One write erases and draws, so the terminal does not flicker.
	var buf bytes.Buffer
	buf.WriteString(t.eraseSeq())
	for _, l := range lines {
		if r := []rune(l); len(r) >= width {
			l = string(r[:width-1])
		}
		buf.WriteString(l + "\n")
	}
	_, _ = t.w.Write(buf.Bytes())
	t.lines = len(lines)
}

func (t *liveTable) erase() {
	if seq := t.eraseSeq(); seq != "" {
		_, _ = io.WriteString(t.w, seq)
	}
	t.lines = 0
}

// eraseSeq moves the cursor up to the drawing's first line and clears
// to the end of the screen.
func (t *liveTable) eraseSeq() string {
	if t.lines == 0 {
		return ""
	}
	return fmt.Sprintf("\x1b[%dA\r\x1b[J", t.lines)
}
