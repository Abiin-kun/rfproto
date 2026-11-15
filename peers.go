package main

import (
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"
)

// These records are local state, not a vendor file format. Wire requirements:
// docs/INVESTIGATION.md E24/E35; docs/05-route-a-crypto.md §12.2.
type pairedPhone struct {
	RealDeviceID string    `json:"realDeviceId"`
	Name         string    `json:"deviceName"`
	Key          string    `json:"key"`
	PairedAt     time.Time `json:"pairedAt"`
}

type peerStore struct {
	mu    sync.Mutex
	dir   string
	lock  *os.File
	peers map[string]pairedPhone
}

var trustedAdults *peerStore

func readPeers(dir string) (map[string]pairedPhone, error) {
	b, err := os.ReadFile(filepath.Join(dir, "peers.json"))
	if errors.Is(err, os.ErrNotExist) {
		return make(map[string]pairedPhone), nil
	}
	if err != nil {
		return nil, err
	}
	var records []pairedPhone
	if err := json.Unmarshal(b, &records); err != nil {
		return nil, fmt.Errorf("invalid peers.json: %w", err)
	}
	peers := make(map[string]pairedPhone, len(records))
	for _, p := range records {
		if p.RealDeviceID == "" || !validPairKey(p.Key) {
			return nil, errors.New("invalid phone identity or key in peers.json")
		}
		if _, exists := peers[p.RealDeviceID]; exists {
			return nil, errors.New("duplicate phone identity in peers.json")
		}
		peers[p.RealDeviceID] = p
	}
	return peers, nil
}

func openPeerStore(dir string) (*peerStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(dir, "peers.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("trust store is in use; stop the other server before changing peers: %w", err)
	}
	peers, err := readPeers(dir)
	if err != nil {
		lock.Close()
		return nil, err
	}
	return &peerStore{dir: dir, lock: lock, peers: peers}, nil
}

func (s *peerStore) close() error { return s.lock.Close() }

func validPairKey(key string) bool {
	b, err := base64.StdEncoding.Strict().DecodeString(key)
	return err == nil && len(b) == 32 && base64.StdEncoding.EncodeToString(b) == key
}

func sameSecret(a, b string) bool {
	return a != "" && b != "" && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func (s *peerStore) lookup(id string) (pairedPhone, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.peers[id]
	return p, ok
}

// remember never silently replaces an existing pairing secret, even with a QR.
func (s *peerStore) remember(p pairedPhone) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.RealDeviceID == "" || !validPairKey(p.Key) {
		return errors.New("pair-confirm requires a stable identity and a base64 32-byte key")
	}
	if old, ok := s.peers[p.RealDeviceID]; ok {
		if !sameSecret(old.Key, p.Key) {
			return errors.New("phone key changed; forget the old pairing first")
		}
		return nil
	}
	p.PairedAt = time.Now().UTC()
	next := make(map[string]pairedPhone, len(s.peers)+1)
	for id, old := range s.peers {
		next[id] = old
	}
	next[p.RealDeviceID] = p
	if err := s.save(next); err != nil {
		return err
	}
	s.peers = next
	return nil
}

func (s *peerStore) forget(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.peers[id]; !ok {
		return errors.New("unknown phone identity")
	}
	next := make(map[string]pairedPhone, len(s.peers)-1)
	for key, p := range s.peers {
		if key != id {
			next[key] = p
		}
	}
	if err := s.save(next); err != nil {
		return err
	}
	s.peers = next
	return nil
}

func sortedPeers(peers map[string]pairedPhone) []pairedPhone {
	records := make([]pairedPhone, 0, len(peers))
	for _, p := range peers {
		records = append(records, p)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].RealDeviceID < records[j].RealDeviceID })
	return records
}

func (s *peerStore) save(peers map[string]pairedPhone) error {
	b, err := json.MarshalIndent(sortedPeers(peers), "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(s.dir, ".peers-*") // mode 0600, same filesystem
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err := f.Write(b); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), filepath.Join(s.dir, "peers.json")); err != nil {
		return err
	}
	d, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func peersCommand(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("peers", flag.ContinueOnError)
	dir := fs.String("dir", "", "state dir (default: $XDG_STATE_HOME/rfproto or ~/.local/state/rfproto)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	resolvedDir, err := resolveStateDir(*dir)
	if err != nil {
		return err
	}
	*dir = resolvedDir
	if fs.NArg() == 1 && fs.Arg(0) == "list" {
		peers, err := readPeers(*dir)
		if err != nil {
			return err
		}
		// Deliberately omit the secret field entirely from CLI output.
		type publicPhone struct {
			RealDeviceID string    `json:"realDeviceId"`
			Name         string    `json:"deviceName"`
			PairedAt     time.Time `json:"pairedAt"`
		}
		list := make([]publicPhone, 0, len(peers))
		for _, p := range sortedPeers(peers) {
			list = append(list, publicPhone{p.RealDeviceID, p.Name, p.PairedAt})
		}
		return json.NewEncoder(out).Encode(list)
	}
	if fs.NArg() == 2 && fs.Arg(0) == "forget" {
		s, err := openPeerStore(*dir)
		if err != nil {
			return err
		}
		defer s.close()
		return s.forget(fs.Arg(1))
	}
	return errors.New("usage: rfproto peers [-dir PATH] list | forget <realDeviceId>")
}

// authHello is the documented manager hello, not a generic service message.
type authHello struct {
	ServiceID    *int `json:"serviceId"`
	SrcServiceID *int `json:"srcServiceId"`
	Data         struct {
		Cmd    *int      `json:"cmd"`
		Paired int       `json:"paired"`
		Token  string    `json:"token"`
		Device *pcDevice `json:"device"`
	} `json:"data"`
}

type peerAuth struct {
	realID  string
	key     string
	trusted bool
}

func authenticateHello(msg []byte, invite string, store *peerStore) (peerAuth, error) {
	var h authHello
	if store == nil || json.Unmarshal(msg, &h) != nil || h.ServiceID == nil || *h.ServiceID != 0 ||
		h.SrcServiceID == nil || *h.SrcServiceID != 0 || h.Data.Cmd == nil || *h.Data.Cmd != 0 ||
		h.Data.Device == nil || h.Data.Device.DeviceID == "" {
		return peerAuth{}, errors.New("expected a manager hello with phone identity")
	}
	id := h.Data.Device.RealDeviceID
	if h.Data.Paired == 1 {
		if id == "" {
			id = h.Data.Device.DeviceID
		}
		p, ok := store.lookup(id)
		if !ok || !sameSecret(p.Key, h.Data.Device.Key) {
			return peerAuth{}, errors.New("unknown phone or incorrect reconnect key")
		}
		return peerAuth{realID: id, key: p.Key, trusted: true}, nil
	}
	if h.Data.Paired != 0 || !sameSecret(invite, h.Data.Token) {
		return peerAuth{}, errors.New("invalid QR invite token")
	}
	return peerAuth{realID: id}, nil
}

func (a *peerAuth) confirm(msg []byte, store *peerStore) (string, error) {
	var p struct {
		Data struct {
			Device pcDevice `json:"device"`
		} `json:"data"`
	}
	if err := json.Unmarshal(msg, &p); err != nil {
		return "", err
	}
	d := p.Data.Device
	id := d.RealDeviceID
	if id == "" {
		id = d.DeviceID
	}
	if (a.realID != "" && a.realID != id) || (a.trusted && !sameSecret(a.key, d.Key)) {
		return "", errors.New("pair-confirm changed the authenticated phone identity or key")
	}
	if err := store.remember(pairedPhone{RealDeviceID: id, Name: d.DeviceName, Key: d.Key}); err != nil {
		return "", err
	}
	a.realID, a.key, a.trusted = id, d.Key, true
	return d.Key, nil
}
