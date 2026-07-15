package ingame

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"hosuto/internal/chatstore"
	"hosuto/internal/store"
)

// RCON frames cap a command near 1446 bytes; stay well under it. rconSafe leaves headroom for a
// per-part "[i/n] " prefix that is added after the fit check.
const (
	rconLimit = 1400
	rconSafe  = 1200
	chunkSize = 900 // runes per answer chunk before considering escaping/limits
)

// Minecraft text-component colors used for the CLI's own lines. Kept few and consistent.
const (
	colorAnswer = "white"
	colorInfo   = "gray"
	colorError  = "red"
	colorList   = "aqua"
	colorActive = "green"
)

// msgKind selects the color for a one-line CLI message.
type msgKind int

const (
	msgInfo msgKind = iota
	msgError
)

func (k msgKind) color() string {
	if k == msgError {
		return colorError
	}
	return colorInfo
}

// textComponent is the minimal Minecraft raw-JSON text object. Always produced via json.Marshal so
// the payload is correctly escaped — never hand-built.
type textComponent struct {
	Text  string      `json:"text"`
	Color string      `json:"color,omitempty"`
	Click *clickEvent `json:"clickEvent,omitempty"`
}

type clickEvent struct {
	Action string `json:"action"`
	Value  string `json:"value"`
}

// answerTarget is the tellraw selector for AI answers: the asking player when replies are private
// (default), else everyone. CLI feedback (help/list/errors) always targets the player.
func (e *Engine) answerTarget(player string) string {
	if e.replyPrivate() {
		return player
	}
	return "@a"
}

// reply sends one colored line privately to the player.
func (e *Engine) reply(ctx context.Context, srv store.Server, player string, kind msgKind, text string) {
	if cmd, ok := tellrawCmd(player, text, kind.color()); ok {
		e.send(ctx, srv, cmd)
	}
}

// replyHelp prints the grammar as a single multi-line component.
func (e *Engine) replyHelp(ctx context.Context, srv store.Server, player string) {
	t := e.trigger()
	help := strings.Join([]string{
		t + " <question> — ask the AI about this server",
		t + " new [question] — start a new chat",
		t + " list — show chats (click one to resume)",
		t + " resume <n> — resume a chat",
		t + " end — leave the current chat",
	}, "\n")
	if cmd, ok := tellrawCmd(player, help, colorInfo); ok {
		e.send(ctx, srv, cmd)
	}
}

// replyList prints the conversations as clickable, numbered lines. Clicking runs the chat command
// "!ai resume <n>" (no leading slash => posted as chat => read back by the follower), so a click is
// identical to typing it.
func (e *Engine) replyList(ctx context.Context, srv store.Server, player string, list []chatstore.Summary, activeID string) {
	cmds := make([]string, 0, len(list)+1)
	if cmd, ok := tellrawCmd(player, "Chats (newest first) — click to resume:", colorInfo); ok {
		cmds = append(cmds, cmd)
	}
	for i, c := range list {
		title := strings.TrimSpace(c.Title)
		if title == "" {
			title = "(untitled)"
		}
		label := fmt.Sprintf("[%d] %s", i+1, title)
		color := colorList
		if c.ID == activeID {
			label += " (current)"
			color = colorActive
		}
		comp := textComponent{
			Text:  label,
			Color: color,
			Click: &clickEvent{Action: "run_command", Value: e.trigger() + " resume " + fmt.Sprint(i+1)},
		}
		if b, err := json.Marshal(comp); err == nil {
			cmd := "tellraw " + player + " " + string(b)
			if len(cmd) <= rconLimit {
				cmds = append(cmds, cmd)
			}
		}
	}
	e.send(ctx, srv, cmds...)
}

// replyAnswer renders the AI's answer to plain in-game text and delivers it in fitted, numbered parts.
func (e *Engine) replyAnswer(ctx context.Context, srv store.Server, player, answer string) {
	target := e.answerTarget(player)
	var fitted []string
	for _, p := range chunkText(plain(answer), chunkSize) {
		fitted = append(fitted, splitToFit(target, p)...)
	}
	n := len(fitted)
	cmds := make([]string, 0, n)
	for i, p := range fitted {
		text := p
		if n > 1 {
			text = fmt.Sprintf("[%d/%d] %s", i+1, n, p)
		}
		if cmd, ok := tellrawCmd(target, text, colorAnswer); ok {
			cmds = append(cmds, cmd)
		}
	}
	e.send(ctx, srv, cmds...)
}

// send runs the tellraw commands over one RCON session. Fire-and-forget: a server that is down or
// whose RCON is not up yet simply drops the reply — the conversation is already persisted and the
// dashboard has it (precedent: Manager.Say).
func (e *Engine) send(ctx context.Context, srv store.Server, cmds ...string) {
	if len(cmds) == 0 {
		return
	}
	c, cancel := ctxTimeout(ctx, 15*time.Second)
	defer cancel()
	_, _, _ = e.Mgr.Command(c, srv, cmds...)
}

// tellrawCmd builds a `tellraw <target> <json>` command for a colored line and reports whether it fits
// under the safe RCON size.
func tellrawCmd(target, text, color string) (string, bool) {
	b, err := json.Marshal(textComponent{Text: text, Color: color})
	if err != nil {
		return "", false
	}
	cmd := "tellraw " + target + " " + string(b)
	return cmd, len(cmd) <= rconSafe
}

// splitToFit splits a text part until each piece yields a tellraw command within the safe size —
// necessary because JSON escaping can inflate a part unpredictably. It prefers to break on whitespace
// near the midpoint and stops at a single rune (which it emits even if still oversized).
func splitToFit(target, p string) []string {
	if _, ok := tellrawCmd(target, p, colorAnswer); ok {
		return []string{p}
	}
	runes := []rune(p)
	if len(runes) <= 1 {
		return []string{p}
	}
	mid := len(runes) / 2
	for i := mid; i < len(runes)-1 && i < mid+60; i++ {
		if runes[i] == ' ' || runes[i] == '\n' {
			mid = i
			break
		}
	}
	left := strings.TrimSpace(string(runes[:mid]))
	right := strings.TrimSpace(string(runes[mid:]))
	if left == "" || right == "" { // no usable boundary — force an even cut
		mid = len(runes) / 2
		left = string(runes[:mid])
		right = string(runes[mid:])
	}
	return append(splitToFit(target, left), splitToFit(target, right)...)
}

// chunkText splits s into pieces of at most size runes, preferring a break at the last newline or
// space in the second half of the window so words and lines stay intact.
func chunkText(s string, size int) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	var out []string
	for {
		r := []rune(s)
		if len(r) <= size {
			out = append(out, s)
			return out
		}
		cut := size
		for i := size; i > size/2; i-- {
			if r[i] == '\n' || r[i] == ' ' {
				cut = i
				break
			}
		}
		out = append(out, strings.TrimRight(string(r[:cut]), " \n"))
		s = strings.TrimLeft(string(r[cut:]), " \n")
	}
}

// --- markdown -> plain (in-game chat has no markdown) ---

var (
	reFence  = regexp.MustCompile("(?m)^```.*$")
	reLink   = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)
	reBold   = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	reBoldU  = regexp.MustCompile(`__([^_]+)__`)
	reCode   = regexp.MustCompile("`([^`]+)`")
	reHead   = regexp.MustCompile(`(?m)^\s*#{1,6}\s*`)
	reQuote  = regexp.MustCompile(`(?m)^\s*>\s?`)
	reBullet = regexp.MustCompile(`(?m)^(\s*)[-*]\s+`)
	reBlanks = regexp.MustCompile(`\n{3,}`)
)

// plain strips the markdown Claude tends to emit down to text Minecraft chat can show: fences and
// inline code unwrapped, emphasis removed, links flattened to "label (url)", headings/quotes stripped,
// list markers turned into bullets.
func plain(s string) string {
	s = reFence.ReplaceAllString(s, "")
	s = reLink.ReplaceAllString(s, "$1 ($2)")
	s = reBold.ReplaceAllString(s, "$1")
	s = reBoldU.ReplaceAllString(s, "$1")
	s = reCode.ReplaceAllString(s, "$1")
	s = reHead.ReplaceAllString(s, "")
	s = reQuote.ReplaceAllString(s, "")
	s = reBullet.ReplaceAllString(s, "$1• ")
	s = reBlanks.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}
