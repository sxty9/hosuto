// Package skin renders a player's Minecraft face from their skin.
//
// hosuto owns the Linux user → Minecraft account mapping, so hosuto is the only service that CAN do
// this — and therefore the one that should. It renders the face itself rather than linking a
// third-party head service (crafatar, mc-heads): those are an availability dependency the landscape
// does not otherwise have (crafatar's public instance has been returning HTTP 521 for months), they
// leak every member's UUID to a stranger, and the whole job is about sixty lines of stdlib.
//
// The chain, all cached because a skin changes rarely and a member list re-renders often:
//
//	uuid → sessionserver profile → base64 "textures" property → skin PNG URL → the 64×64 skin
//	     → crop the face at (8,8) → composite the hat overlay at (40,8) → scale up → PNG
//
// The hat layer is the part people forget. It is a second, transparent 8×8 tile that most modern
// skins use for hair, glasses or a hood; a face rendered without it looks wrong to the person whose
// face it is.
package skin

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ErrNoSkin means the profile has no usable skin (or Mojang does not know the UUID). Callers should
// answer 404 and let the UI fall back to initials — a wrong face is worse than no face.
var ErrNoSkin = errors.New("no skin for that profile")

const (
	sessionBase = "https://sessionserver.mojang.com/session/minecraft/profile/"
	ttl         = 6 * time.Hour // a skin changes rarely; a member list re-renders constantly
	maxSkin     = 2 << 20       // a Minecraft skin is a few KB; anything larger is not one
)

type entry struct {
	png []byte
	at  time.Time
}

// Renderer turns a UUID into a face PNG.
type Renderer struct {
	base string
	http *http.Client

	mu    sync.Mutex
	cache map[string]entry // key: uuid|size
	now   func() time.Time
}

// New builds a renderer. An empty baseURL uses Mojang's session server.
func New(baseURL string, hc *http.Client) *Renderer {
	if baseURL == "" {
		baseURL = sessionBase
	}
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	return &Renderer{
		base:  strings.TrimSuffix(baseURL, "/") + "/",
		http:  hc,
		cache: map[string]entry{},
		now:   time.Now,
	}
}

// Face returns a size×size PNG of the player's face, hat layer composited on top.
func (r *Renderer) Face(ctx context.Context, uuid string, size int) ([]byte, error) {
	if size <= 0 || size > 512 {
		size = 64
	}
	// The session server wants the UUID undashed; the rest of hosuto stores it dashed because
	// whitelist.json demands that form.
	id := strings.ReplaceAll(strings.TrimSpace(uuid), "-", "")
	if len(id) != 32 {
		return nil, ErrNoSkin
	}
	key := fmt.Sprintf("%s|%d", id, size)

	r.mu.Lock()
	if e, ok := r.cache[key]; ok && r.now().Sub(e.at) < ttl {
		r.mu.Unlock()
		return e.png, nil
	}
	r.mu.Unlock()

	skinURL, err := r.skinURL(ctx, id)
	if err != nil {
		return nil, err
	}
	src, err := r.fetchSkin(ctx, skinURL)
	if err != nil {
		return nil, err
	}
	out, err := encodeFace(src, size)
	if err != nil {
		return nil, err
	}

	r.mu.Lock()
	r.cache[key] = entry{png: out, at: r.now()}
	r.mu.Unlock()
	return out, nil
}

// skinURL resolves the profile's skin texture URL.
//
// The session server answers 204 with an EMPTY BODY for a UUID it does not know — not 404 — so a
// naive "status < 300 means fine" check would go on to parse nothing and produce a confusing error.
func (r *Renderer) skinURL(ctx context.Context, id string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.base+id, nil)
	if err != nil {
		return "", err
	}
	resp, err := r.http.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); resp.Body.Close() }()
	if resp.StatusCode == http.StatusNoContent {
		return "", ErrNoSkin
	}
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("skin: profile %s: status %d", id, resp.StatusCode)
	}

	var profile struct {
		Properties []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"properties"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&profile); err != nil {
		return "", err
	}
	for _, p := range profile.Properties {
		if p.Name != "textures" {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(p.Value)
		if err != nil {
			return "", err
		}
		var tex struct {
			Textures struct {
				Skin struct {
					URL string `json:"url"`
				} `json:"SKIN"`
			} `json:"textures"`
		}
		if err := json.Unmarshal(raw, &tex); err != nil {
			return "", err
		}
		if tex.Textures.Skin.URL == "" {
			// A profile with no SKIN texture is on the default Steve/Alex. There is nothing to render,
			// and inventing one would be a lie about which skin they have.
			return "", ErrNoSkin
		}
		return tex.Textures.Skin.URL, nil
	}
	return "", ErrNoSkin
}

func (r *Renderer) fetchSkin(ctx context.Context, url string) (image.Image, error) {
	// Mojang hands out http:// texture URLs. Upgrade to https so the skin is not fetched in clear.
	if strings.HasPrefix(url, "http://textures.minecraft.net/") {
		url = "https://" + strings.TrimPrefix(url, "http://")
	}
	if !strings.HasPrefix(url, "https://textures.minecraft.net/") {
		// The URL comes from a signed Mojang property, but it is still remote input that decides where
		// this daemon connects. Pin it to the texture host rather than letting it point anywhere.
		return nil, fmt.Errorf("skin: refusing texture host in %q", url)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := r.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("skin: texture: status %d", resp.StatusCode)
	}
	return png.Decode(io.LimitReader(resp.Body, maxSkin))
}

// encodeFace crops the face, composites the hat over it, and scales the result up.
//
// Skin layout (both the modern 64×64 and the legacy 64×32 share the head):
//
//	face — (8,8)  to (16,16)
//	hat  — (40,8) to (48,16), transparent where there is no hat
func encodeFace(src image.Image, size int) ([]byte, error) {
	b := src.Bounds()
	if b.Dx() < 64 || b.Dy() < 32 {
		return nil, ErrNoSkin
	}
	const tile = 8

	// Composite at 8×8 first, then scale once. Scaling each layer separately would blur the hat's
	// alpha edge against the face.
	head := image.NewNRGBA(image.Rect(0, 0, tile, tile))
	draw.Draw(head, head.Bounds(),
		src, b.Min.Add(image.Pt(8, 8)), draw.Src)
	draw.Draw(head, head.Bounds(),
		src, b.Min.Add(image.Pt(40, 8)), draw.Over) // the hat layer, alpha-blended over the face

	// Nearest-neighbour, deliberately: Minecraft skins are 8×8 pixel art. Any smoothing filter turns
	// a crisp face into mush, which is exactly what the third-party head services get right and a
	// naive image.Resize would get wrong.
	out := image.NewNRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		sy := y * tile / size
		for x := 0; x < size; x++ {
			sx := x * tile / size
			out.Set(x, y, head.At(sx, sy))
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, out); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
