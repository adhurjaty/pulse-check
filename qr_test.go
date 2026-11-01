package main

import (
	"crypto/tls"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestJoinURL(t *testing.T) {
	for _, tc := range []struct {
		name, host, proto string
		tls               bool
		want              string
	}{
		{"plain http", "localhost:8080", "", false, "http://localhost:8080/?r=ABCD"},
		{"direct TLS", "example.com", "", true, "https://example.com/?r=ABCD"},
		{"behind Caddy", "pulse.5.78.111.152.sslip.io", "https", false, "https://pulse.5.78.111.152.sslip.io/?r=ABCD"},
		{"junk proto ignored", "example.com", "javascript", false, "http://example.com/?r=ABCD"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			r.Host = tc.host
			if tc.proto != "" {
				r.Header.Set("X-Forwarded-Proto", tc.proto)
			}
			if tc.tls {
				r.TLS = &tls.ConnectionState{}
			}
			if got := joinURL(r, "ABCD"); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestQRSVG(t *testing.T) {
	svg, err := qrSVG("https://pulse.5.78.111.152.sslip.io/?r=ABCD")
	must(t, err)
	var doc struct {
		XMLName xml.Name `xml:"svg"`
		ViewBox string   `xml:"viewBox,attr"`
		Path    struct {
			D string `xml:"d,attr"`
		} `xml:"path"`
	}
	must(t, xml.Unmarshal([]byte(svg), &doc))
	if doc.Path.D == "" || !strings.HasPrefix(doc.ViewBox, "0 0 ") {
		t.Fatalf("unexpected svg: viewBox=%q, path length %d", doc.ViewBox, len(doc.Path.D))
	}
	// The quiet zone: no module is drawn in the 4-module border.
	if strings.Contains(doc.Path.D, "M0 ") || strings.Contains(doc.Path.D, "M3 ") {
		t.Fatal("QR modules drawn inside the quiet zone")
	}
}

func TestHostPageQRAndDownloads(t *testing.T) {
	e := newTestEnv(t, testPin)
	code := e.createRoom().Code

	req, _ := http.NewRequest("GET", e.srv.URL+"/api/rooms/"+code+"/qr.svg", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	resp, err := http.DefaultClient.Do(req)
	must(t, err)
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/svg+xml" {
		t.Fatalf("qr: status %d, type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	e.expect(http.StatusNotFound, "GET", "/api/rooms/ZZZZ/qr.svg", "", nil)

	if body := string(e.expect(http.StatusOK, "GET", "/host", "", nil)); !strings.Contains(body, "Pulse Check host") {
		t.Fatal("/host did not serve the host page")
	}
	for _, name := range []string{"sample-questions.json", "questions.schema.json"} {
		want, err := os.ReadFile(name)
		must(t, err)
		if got := e.expect(http.StatusOK, "GET", "/"+name, "", nil); string(got) != string(want) {
			t.Fatalf("/%s differs from the repo file", name)
		}
	}
}
