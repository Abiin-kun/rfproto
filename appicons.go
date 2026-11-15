// appicons.go: probe the HTTPS service the phone advertises on serviceId 17.
//
// Evidence (docs/INVESTIGATION.md E31/E33, docs/03-screen-mirror.md §11.3):
//
// The phone pushes its launchable-app catalog unprompted:
//
//	serviceId 17, cmd 1
//	{"appLimit":5,
//	 "apps":[{"cmp":"…SplashActivity","iconName":"…png","name":"1.1.1.1",
//	          "pkg":"com.cloudflare…","userId":0}, … 207 total],
//	 "autoDownload":false,
//	 "fingerprint":"b6ad7293…466b",   // sha256 of the server cert, hex
//	 "port":50423,                    // changes every session
//	 "token":"lS2NO5UK…uk5S",
//	 "wpApps":[], "cmd":1, "ver":"1"}
//
// The PC half fetches icons with `downloadAppIcons`, whose log literals are
// `downloadAppIcons->ForwardServicePort: realDeviceId: %s, forwardPort: %d,
// port: %d` and `IsScWebserverHomePage, url.Host: %s, url.Path: %s`. So the
// phone runs an HTTPS webserver, the PC reaches it through a tunnel
// port-forward, and the cert is pinned by `fingerprint`.
//
// This probe skips the tunnel and tries the LAN address directly (the phone is
// on the same subnet). It is pure recon: fetch the home page, report the cert
// fingerprint and whether it matches, then try the plausible icon paths. Every
// response is written under -appiconout so the answer survives the run.
//
// Nothing here is copied from the target; the path candidates are guesses and
// the point of the probe is to find out which one the server recognises.
package main

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const svcAppList = 17

// appListMsg is the service-17 cmd-1 payload observed in capture/c1/020 and
// capture/c10/017 (207 apps, identical apart from port/token/fingerprint).
type appListMsg struct {
	AppLimit     int    `json:"appLimit"`
	AutoDownload bool   `json:"autoDownload"`
	Fingerprint  string `json:"fingerprint"`
	Port         int    `json:"port"`
	Token        string `json:"token"`
	Ver          string `json:"ver"`
	Apps         []struct {
		Cmp      string `json:"cmp"`
		IconName string `json:"iconName"`
		Name     string `json:"name"`
		Pkg      string `json:"pkg"`
		UserID   int    `json:"userId"`
	} `json:"apps"`
}

// App-icon probe configuration, set from the serve flags.
var (
	appIconsOn   bool
	appIconsHost string
	appIconsOut  string
	appIconsMax  int
)

// appIconResult is one request attempt, persisted for later reading.
type appIconResult struct {
	Scheme      string `json:"scheme"`
	URL         string `json:"url"`
	TokenVia    string `json:"tokenVia"`
	Status      int    `json:"status"`
	Error       string `json:"error,omitempty"`
	ContentType string `json:"contentType,omitempty"`
	Bytes       int    `json:"bytes,omitempty"`
	CertSHA256  string `json:"certSha256,omitempty"`
	CertMatches bool   `json:"certMatches,omitempty"`
	BodyFile    string `json:"bodyFile,omitempty"`
	Preview     string `json:"preview,omitempty"`
}

// appIconReport is the whole run's output for one service-17 advertisement.
type appIconReport struct {
	Host        string          `json:"host"`
	Port        int             `json:"port"`
	AppLimit    int             `json:"appLimit"`
	Apps        int             `json:"apps"`
	Fingerprint string          `json:"fingerprint"`
	Token       string          `json:"token"`
	Results     []appIconResult `json:"results"`
}

// probeAppList reacts to the phone's app-list advertisement. It returns
// immediately; all network work happens on its own goroutine so the websocket
// read loop keeps draining frames.
func (p *probeSession) probeAppList(seq int, msg []byte) {
	var m appListMsg
	if err := json.Unmarshal(msg, &m); err != nil {
		log.Printf("[appicons] #%d serviceId=%d parse: %v", seq, svcAppList, err)
		return
	}
	if m.Port <= 0 {
		log.Printf("[appicons] #%d app list has no port (port=%d), skipping", seq, m.Port)
		return
	}
	host := appIconsHost
	if host == "" {
		host = p.peerHost
	}
	if host == "" {
		log.Printf("[appicons] #%d no peer host known, skipping", seq)
		return
	}
	log.Printf("[appicons] #%d app list: apps=%d appLimit=%d port=%d token=%s fingerprint=%s host=%s",
		seq, len(m.Apps), m.AppLimit, m.Port, m.Token, m.Fingerprint, host)
	go runAppIconProbe(host, &m)
}

// runAppIconProbe does the actual HTTP(S) reconnaissance.
func runAppIconProbe(host string, m *appListMsg) {
	dir := filepath.Join(appIconsOut, fmt.Sprintf("%s_%d", sanitizeHost(host), m.Port))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("[appicons] mkdir %s: %v", dir, err)
		return
	}
	rep := appIconReport{
		Host: host, Port: m.Port, AppLimit: m.AppLimit, Apps: len(m.Apps),
		Fingerprint: m.Fingerprint, Token: m.Token,
	}
	// The app list itself is evidence; keep it next to the results.
	if b, err := json.MarshalIndent(m, "", " "); err == nil {
		_ = os.WriteFile(filepath.Join(dir, "applist.json"), b, 0o644)
	}

	// Probe the root first: the PC code has a literal `IsScWebserverHomePage`,
	// so a recognisable home page tells us we are talking to the right server
	// and which scheme/token placement it accepts.
	schemes := []string{"https", "http"}
	tokenVias := []string{"query", "header-token", "header-x-token", "bearer"}
	var winner *appIconResult
	for _, scheme := range schemes {
		for _, via := range tokenVias {
			r := doProbe(scheme, host, m.Port, "/", m.Token, via, m.Fingerprint, dir, "home")
			rep.Results = append(rep.Results, r)
			if r.Status >= 200 && r.Status < 400 {
				winner = &rep.Results[len(rep.Results)-1]
				break
			}
			// A TLS handshake failure will not get better with another token
			// placement; move on to the next scheme.
			if scheme == "https" && r.Error != "" && r.Status == 0 && !strings.Contains(r.Error, "EOF") {
				break
			}
		}
		if winner != nil {
			break
		}
	}
	if winner == nil {
		log.Printf("[appicons] no reachable webserver at %s:%d (tried https+http, 4 token placements); "+
			"the port is probably tunnel-only and needs ForwardServicePort", host, m.Port)
		writeReport(dir, &rep)
		return
	}
	log.Printf("[appicons] webserver at %s:%d: scheme=%s tokenVia=%s status=%d ct=%s certMatches=%v",
		host, m.Port, winner.Scheme, winner.TokenVia, winner.Status, winner.ContentType, winner.CertMatches)

	// Icon paths. `iconName` is already a filename like
	// "<pkg>_<activity>.png", so try it bare and under the plausible prefixes
	// the host code hints at (download*, icon*, app*).
	prefixes := []string{"/", "/icon/", "/appicon/", "/download/", "/app/", "/file/"}
	n := appIconsMax
	if n <= 0 || n > len(m.Apps) {
		n = len(m.Apps)
	}
	for i := 0; i < n; i++ {
		app := m.Apps[i]
		if app.IconName == "" {
			continue
		}
		name := strings.TrimPrefix(app.IconName, "/")
		var got *appIconResult
		for _, pre := range prefixes {
			r := doProbe(winner.Scheme, host, m.Port, pre+name, m.Token, winner.TokenVia,
				m.Fingerprint, dir, fmt.Sprintf("app%d", i))
			rep.Results = append(rep.Results, r)
			if r.Status == 200 && r.Bytes > 0 {
				got = &rep.Results[len(rep.Results)-1]
				break
			}
		}
		if got != nil {
			log.Printf("[appicons] app %q icon: %s -> %d (%s, %dB)", app.Name, got.URL, got.Status, got.ContentType, got.Bytes)
		} else {
			log.Printf("[appicons] app %q icon: no prefix worked for %q", app.Name, app.IconName)
		}
	}
	writeReport(dir, &rep)
}

// doProbe performs one GET and persists the outcome (and body) to dir.
func doProbe(scheme, host string, port int, path, token, via, fingerprint, dir, label string) appIconResult {
	r := appIconResult{Scheme: scheme, TokenVia: via}
	hostport := net.JoinHostPort(host, fmt.Sprint(port))

	tr := &http.Transport{
		DisableKeepAlives: true,
		TLSClientConfig: &tls.Config{
			// We pin by comparing sha256(cert.Raw) to the advertised fingerprint
			// ourselves: InsecureSkipVerify only disables the CA check.
			InsecureSkipVerify: true,
			VerifyConnection: func(cs tls.ConnectionState) error {
				if len(cs.PeerCertificates) == 0 {
					return nil
				}
				sum := sha256.Sum256(cs.PeerCertificates[0].Raw)
				r.CertSHA256 = hex.EncodeToString(sum[:])
				r.CertMatches = fingerprint == "" || strings.EqualFold(r.CertSHA256, fingerprint)
				return nil
			},
		},
	}
	client := &http.Client{Transport: tr, Timeout: 6 * time.Second}

	u := url.URL{Scheme: scheme, Host: hostport, Path: path}
	q := u.Query()
	switch via {
	case "query":
		if token != "" {
			q.Set("token", token)
		}
	case "header-token", "header-x-token", "bearer":
		// added below
	}
	u.RawQuery = q.Encode()
	r.URL = u.String()

	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		r.Error = err.Error()
		return r
	}
	switch via {
	case "header-token":
		req.Header.Set("token", token)
	case "header-x-token":
		req.Header.Set("X-Token", token)
	case "bearer":
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := client.Do(req)
	if err != nil {
		r.Error = err.Error()
		return r
	}
	defer resp.Body.Close()
	r.Status = resp.StatusCode
	r.ContentType = resp.Header.Get("Content-Type")
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	r.Bytes = len(body)
	if len(body) > 0 {
		r.Preview = strings.Map(func(c rune) rune {
			if c < 32 || c > 126 {
				return '.'
			}
			return c
		}, string(body[:minInt(len(body), 160)]))
		safe := sanitizeHost(strings.Trim(path, "/"))
		if safe == "" {
			safe = "root"
		}
		fn := filepath.Join(dir, fmt.Sprintf("%s_%s_%s.bin", label, via, safe))
		if len(fn) > 240 {
			fn = fn[:240]
		}
		if err := os.WriteFile(fn, body, 0o644); err == nil {
			r.BodyFile = fn
		}
	}
	return r
}

func writeReport(dir string, rep *appIconReport) {
	b, err := json.MarshalIndent(rep, "", " ")
	if err != nil {
		return
	}
	p := filepath.Join(dir, "report.json")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		log.Printf("[appicons] write %s: %v", p, err)
		return
	}
	log.Printf("[appicons] report: %s (%d attempts)", p, len(rep.Results))
}

func sanitizeHost(s string) string {
	s = strings.TrimSpace(s)
	s = strings.NewReplacer(":", "_", "/", "_", "\\", "_", " ", "_").Replace(s)
	if s == "" {
		return "unknown"
	}
	return s
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// peerHostOf strips the port from a net.Addr-style "host:port" string.
func peerHostOf(remoteAddr string) string {
	if h, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return h
	}
	return remoteAddr
}
