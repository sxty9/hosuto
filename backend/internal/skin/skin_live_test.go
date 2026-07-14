package skin

import (
	"bytes"
	"context"
	"image/png"
	"os"
	"testing"
	"time"
)

// TestFaceLive renders a real player's face against Mojang's live session server. It is skipped
// unless HOSUTO_LIVE=1, so the normal `go test` stays offline — but it is the only check that proves
// the crop coordinates and the hat compositing are actually right, rather than merely self-consistent
// with my own expectations.
func TestFaceLive(t *testing.T) {
	if os.Getenv("HOSUTO_LIVE") != "1" {
		t.Skip("set HOSUTO_LIVE=1 to hit Mojang")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// jeb_ — a real, stable account with a real skin.
	const jeb = "853c80ef-3c37-49fd-aa49-938b674adae6"

	raw, err := New("", nil).Face(ctx, jeb, 64)
	if err != nil {
		t.Fatalf("Face: %v", err)
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("the renderer emitted something that is not a PNG: %v", err)
	}
	if b := img.Bounds(); b.Dx() != 64 || b.Dy() != 64 {
		t.Fatalf("size = %dx%d, want 64x64", b.Dx(), b.Dy())
	}

	// A face is not one flat colour. If the crop landed on an empty or uniform region of the skin
	// sheet (the classic symptom of wrong coordinates), every pixel would be identical.
	first := img.At(0, 0)
	distinct := 0
	for y := 0; y < 64; y += 4 {
		for x := 0; x < 64; x += 4 {
			if img.At(x, y) != first {
				distinct++
			}
		}
	}
	if distinct < 8 {
		t.Errorf("the rendered face is (nearly) a single colour — the crop is probably wrong")
	}

	// Every pixel must be fully opaque: the face layer has no transparency, and the hat is composited
	// OVER it. A transparent result would mean we rendered the hat alone.
	for y := 0; y < 64; y += 8 {
		for x := 0; x < 64; x += 8 {
			if _, _, _, a := img.At(x, y).RGBA(); a != 0xffff {
				t.Errorf("pixel (%d,%d) is not opaque (alpha=%d) — the face layer is missing", x, y, a)
				return
			}
		}
	}

	if out := os.Getenv("HOSUTO_LIVE_OUT"); out != "" {
		_ = os.WriteFile(out, raw, 0o644)
		t.Logf("wrote %s (%d bytes)", out, len(raw))
	}
}
