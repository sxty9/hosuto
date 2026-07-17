// This file is how the Windows desktop client gets its first credential, and it is deliberately the
// only unauthenticated corner of the API.
//
// The app starts with no session and must never ask for the account password. The user, however, is
// already signed in to the dashboard — so the browser mints a short code (startPairing) and the app
// trades it for a bearer token (claimPairing), which it then keeps in the OS credential store. From
// that moment the app is an ordinary caller: resolveCaller reads its token like any other, and the
// user's live OS groups decide what it may do, exactly as they do for a browser. Nothing about the
// desktop client is privileged; it merely holds a different shape of the same identity.
package api

import (
	"encoding/json"
	"net/http"
	"time"

	"hosuto/internal/auth"
	"hosuto/internal/rights"
)

// startPairing mints a short-lived code for the signed-in user.
//
// The response names the host as well, because the app needs to know WHERE to claim: handing the user
// one string to carry ("<host>/<code>") beats asking them to remember their own server's address and
// type it into a second field.
func (s *Server) startPairing(w http.ResponseWriter, r *http.Request, u *auth.User) {
	code, exp, err := s.pair.Issue(u.Username)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not create a pairing code")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"code": code, "expires": exp.Unix(), "host": r.Host,
	})
}

// claimPairing trades a pairing code for a bearer token.
//
// The token is account-wide (no server scope) because the app is the player's whole hosuto: it lists
// every server they can reach and syncs any of them. It is long-lived for the same reason a desktop
// client exists at all — a client that made people re-pair every month would be a worse mods.zip.
// Revocation stays where it already is: the token routes list and drop a user's tokens.
func (s *Server) claimPairing(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "Malformed request")
		return
	}
	subject, ok := s.pair.Claim(body.Code)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "That pairing code is unknown or has expired")
		return
	}
	// The code proved who minted it, minutes ago. Whether that account still exists and may still play
	// is the kernel's answer, asked now — the same question every other request on this surface asks.
	u, ok := s.v.Resolve(subject)
	if !ok || !u.Can(rights.GroupPlay) {
		writeErr(w, http.StatusForbidden, "That account may not use the desktop client")
		return
	}
	days := s.cfg.Int("desktopTokenDays", 365)
	if days <= 0 || days > 3650 {
		days = 365
	}
	token, exp, err := s.tok.Mint(subject, "", time.Duration(days)*24*time.Hour)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not create a token")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"token": token, "expires": exp.Unix(), "user": subject,
	})
}
