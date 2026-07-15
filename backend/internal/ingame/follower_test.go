package ingame

import "testing"

func TestChatReMatchesLoaders(t *testing.T) {
	cases := []struct {
		line, player, text string
		match              bool
	}{
		{`[12:34:56] [Server thread/INFO]: <IchBinsHenry> !ai hello`, "IchBinsHenry", "!ai hello", true},
		{`[00:00:01] [Async Chat Thread - #0/INFO]: <Steve> !ai list`, "Steve", "!ai list", true},
		{`[12:34:56] [Server thread/INFO]: Steve joined the game`, "", "", false},
		{`[12:34:56] [Server thread/INFO]: [Rcon] tellraw stuff`, "", "", false},
		{`not a log line at all`, "", "", false},
	}
	for _, c := range cases {
		m := chatRe.FindStringSubmatch(c.line)
		if (m != nil) != c.match {
			t.Errorf("match(%q) = %v, want %v", c.line, m != nil, c.match)
			continue
		}
		if m != nil {
			if m[1] != c.player || m[2] != c.text {
				t.Errorf("parse(%q) = (%q,%q), want (%q,%q)", c.line, m[1], m[2], c.player, c.text)
			}
		}
	}
}

func TestUUIDReAnchor(t *testing.T) {
	line := `[12:34:55] [User Authenticator #1/INFO]: UUID of player IchBinsHenry is 069a79f4-44e9-4726-a5be-fca90e38aaf5`
	m := uuidRe.FindStringSubmatch(line)
	if m == nil {
		t.Fatal("expected a UUID anchor match")
	}
	if m[1] != "IchBinsHenry" {
		t.Errorf("name = %q", m[1])
	}
	if m[2] != "069a79f4-44e9-4726-a5be-fca90e38aaf5" {
		t.Errorf("uuid = %q", m[2])
	}
}

func TestSplitVerb(t *testing.T) {
	cases := []struct{ in, verb, rest string }{
		{"", "", ""},
		{"help", "help", ""},
		{"LIST", "list", ""},
		{"resume 3", "resume", "3"},
		{"new how many players are online?", "new", "how many players are online?"},
		{"how many players are online?", "how", "many players are online?"},
		{"  end  ", "end", ""},
	}
	for _, c := range cases {
		v, r := splitVerb(c.in)
		if v != c.verb || r != c.rest {
			t.Errorf("splitVerb(%q) = (%q,%q), want (%q,%q)", c.in, v, r, c.verb, c.rest)
		}
	}
}
