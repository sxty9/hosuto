package ingame

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPlainStripsMarkdown(t *testing.T) {
	in := "# Heading\n\n" +
		"Some **bold** and _under_ and `code` here.\n" +
		"- one\n- two\n" +
		"See [the docs](https://example.com/x).\n" +
		"```\nfenced block\n```\n"
	got := plain(in)
	for _, bad := range []string{"**", "`", "```", "# ", "](", "[the docs]"} {
		if strings.Contains(got, bad) {
			t.Errorf("plain() left %q in output:\n%s", bad, got)
		}
	}
	for _, want := range []string{"Heading", "bold", "code", "• one", "• two", "the docs (https://example.com/x)", "fenced block"} {
		if !strings.Contains(got, want) {
			t.Errorf("plain() dropped %q; output:\n%s", want, got)
		}
	}
}

func TestChunkTextRespectsSize(t *testing.T) {
	long := strings.Repeat("word ", 500) // 2500 runes
	chunks := chunkText(long, 100)
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}
	for i, c := range chunks {
		if n := len([]rune(c)); n > 100 {
			t.Errorf("chunk %d has %d runes, over the 100 limit", i, n)
		}
		if strings.TrimSpace(c) == "" {
			t.Errorf("chunk %d is blank", i)
		}
	}
}

func TestChunkTextEmpty(t *testing.T) {
	if got := chunkText("   \n  ", 100); got != nil {
		t.Errorf("blank input should yield nil, got %v", got)
	}
}

func TestSplitToFitAlwaysFits(t *testing.T) {
	// A long run peppered with quotes/newlines maximises JSON-escaping inflation.
	s := strings.Repeat("aaa \"bb\" \n", 400)
	for _, part := range splitToFit("SomePlayerName", s) {
		if _, ok := tellrawCmd("SomePlayerName", part, colorAnswer); !ok && len([]rune(part)) > 1 {
			t.Errorf("splitToFit produced an oversized part (%d runes)", len([]rune(part)))
		}
	}
}

func TestTellrawIsValidEscapedJSON(t *testing.T) {
	cmd, ok := tellrawCmd("Steve", `he said "hi" & <stuff>`, colorAnswer)
	if !ok {
		t.Fatal("short line should fit")
	}
	// Everything after `tellraw Steve ` must be valid JSON (i.e. properly escaped).
	prefix := "tellraw Steve "
	if !strings.HasPrefix(cmd, prefix) {
		t.Fatalf("unexpected command shape: %q", cmd)
	}
	var comp map[string]any
	if err := json.Unmarshal([]byte(cmd[len(prefix):]), &comp); err != nil {
		t.Fatalf("tellraw payload is not valid JSON: %v (%s)", err, cmd)
	}
	if comp["text"] != `he said "hi" & <stuff>` {
		t.Errorf("text round-trip mismatch: %v", comp["text"])
	}
}
