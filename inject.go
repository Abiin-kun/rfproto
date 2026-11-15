// inject.go: a loopback-only control socket for the running serve process.
//
// Static analysis can only narrow the phone's inbound dispatcher down to a
// per-service listener map; which service ids are actually registered is a
// runtime fact. Rather than restart + re-pair for every probe (which changes
// the very state we are measuring), -inject exposes the live WebSocket so a
// frame can be posted into the session that is already open.
//
// Off unless -inject is set. Bind loopback: the phone must never reach it.
package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"sync"
)

// liveSession is the injectable handle to the newest WebSocket session.
type liveSession struct {
	seq  int64
	peer string
	name string
	send func(kind string, out []byte) error
}

var (
	liveMu sync.Mutex
	live   *liveSession
)

func setLive(s *liveSession) {
	liveMu.Lock()
	live = s
	if streams != nil {
		streams.controlConnected(s)
	}
	liveMu.Unlock()
}

func clearLive(s *liveSession) {
	liveMu.Lock()
	if live == s {
		live = nil
		if streams != nil {
			streams.controlDisconnected(s)
		}
	}
	liveMu.Unlock()
}

func currentLive() *liveSession {
	liveMu.Lock()
	defer liveMu.Unlock()
	return live
}

// startInjectServer serves POST /send (raw frame body) and GET /state.
func startInjectServer(addr string) {
	if addr == "" {
		return
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/send", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		ls := currentLive()
		if ls == nil {
			http.Error(w, "no live session", http.StatusConflict)
			return
		}
		log.Printf("[inject] %dB -> session c%d (%s): %s", len(body), ls.seq, ls.peer, clip(body, 500))
		if err := ls.send("inject", body); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"sent": len(body), "session": ls.seq})
	})
	mux.HandleFunc("/state", func(w http.ResponseWriter, r *http.Request) {
		ls := currentLive()
		w.Header().Set("Content-Type", "application/json")
		if ls == nil {
			_ = json.NewEncoder(w).Encode(map[string]any{"live": false})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"live": true, "session": ls.seq, "peer": ls.peer})
	})
	go func() {
		log.Printf("[inject] loopback control on http://%s (POST /send, GET /state)", addr)
		if err := http.ListenAndServe(addr, mux); err != nil {
			log.Printf("[inject] %v", err)
		}
	}()
}
