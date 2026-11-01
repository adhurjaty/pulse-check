package main

import (
	"fmt"
	"net/http"
	"strings"

	"rsc.io/qr"
)

// joinURL is the link a student's phone opens to join a room. It uses the
// host and scheme the request came in on, so it matches whatever URL the
// host page was opened with (behind Caddy, X-Forwarded-Proto is https).
func joinURL(r *http.Request, code string) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := r.Header.Get("X-Forwarded-Proto"); p == "https" || p == "http" {
		scheme = p
	}
	return fmt.Sprintf("%s://%s/?r=%s", scheme, r.Host, code)
}

// qrSVG renders text as a QR code in SVG, with the standard 4-module quiet
// zone, so it stays sharp at any size on a projector.
func qrSVG(text string) (string, error) {
	code, err := qr.Encode(text, qr.M)
	if err != nil {
		return "", err
	}
	const quiet = 4
	n := code.Size + 2*quiet
	var path strings.Builder
	for y := 0; y < code.Size; y++ {
		for x := 0; x < code.Size; x++ {
			if code.Black(x, y) {
				fmt.Fprintf(&path, "M%d %dh1v1h-1z", x+quiet, y+quiet)
			}
		}
	}
	return fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" shape-rendering="crispEdges">`+
		`<rect width="%d" height="%d" fill="#fff"/><path fill="#000" d="%s"/></svg>`,
		n, n, n, n, path.String()), nil
}

func (s *Server) handleQR(w http.ResponseWriter, r *http.Request, room *Room) {
	svg, err := qrSVG(joinURL(r, room.Code()))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "couldn't make QR code")
		return
	}
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write([]byte(svg))
}
