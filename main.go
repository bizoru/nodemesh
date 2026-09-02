// nodemesh: distributed node-status service.
//
// Every node collects its own network status (wifi/ethernet, SSID, local IP,
// tailscale IP, uptime) into a per-node hash-chained append-only log, and
// replicates all peers' logs via pull gossip, so any node can answer for any
// other. HTTP API + web UI on :7777, bound to the Tailscale IP + localhost.
package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

//go:embed ui.html
var uiHTML []byte

// ---------- config ----------

type Config struct {
	Node        string   `json:"node"`
	Port        int      `json:"port"`
	Peers       []string `json:"peers"`
	DataDir     string   `json:"dataDir"`
	CollectSecs int      `json:"collectSecs"`
	GossipSecs  int      `json:"gossipSecs"`
	// Networks maps default-gateway MAC -> friendly network name, used when the
	// OS hides the SSID (macOS redacts it for daemons without location access).
	Networks map[string]string `json:"networks,omitempty"`
	// LANPeer: mDNS hostname (e.g. "other-node.local") of a sibling node on
	// the SAME physical LAN. Set only on nodes that share one. When set,
	// this node pings it periodically over the LAN — independent of
	// Tailscale — so if the peer's tsIP goes empty, the dashboard can still
	// say "alive on the LAN, just off the tailnet" instead of just "gone".
	// No credentials involved: a plain ICMP ping, not an API call.
	LANPeer string `json:"lanPeer,omitempty"`
	// TSIP: dirección de Tailscale fijada a mano, para hosts donde no se puede
	// autodetectar. Sólo hace falta en Android/Termux: allí no hay
	// CLI de Tailscale y el sandbox de la app deja net.InterfaceAddrs() vacío,
	// así que el bind al tsIP reintentaría eternamente y el nodo quedaría
	// escuchando sólo en loopback — invisible para el resto del mesh.
	// Si está vacío se usa la autodetección de siempre.
	TSIP string `json:"tsIP,omitempty"`
	// BLEStatePath: absolute path to a companion BLE beacon's state.json.
	// Set only on nodes paired over Bluetooth. That beacon runs as a
	// per-user LaunchAgent while nodemesh runs as root, so this must be an
	// absolute path (root's $HOME won't resolve to the login user's home).
	// Polled on the same cadence as the LAN check; independent signal, since
	// it works even when both Tailscale AND the shared LAN are down (it's a
	// direct Bluetooth link between the pair, no router involved).
	BLEStatePath string `json:"bleStatePath,omitempty"`
	// Locations maps node name -> site label (e.g. "gateway": "DigitalOcean").
	// Shared by every node so any dashboard can group peers by site.
	Locations map[string]string `json:"locations,omitempty"`
}

func loadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	if c.Node == "" {
		h, _ := os.Hostname()
		c.Node = strings.Split(h, ".")[0]
	}
	if c.Port == 0 {
		c.Port = 7777
	}
	if c.DataDir == "" {
		c.DataDir = filepath.Join(filepath.Dir(path), "data")
	}
	if c.CollectSecs == 0 {
		c.CollectSecs = 60
	}
	for n, loc := range c.Locations {
		nodeLocation[n] = loc
	}
	if c.GossipSecs == 0 {
		c.GossipSecs = 120
	}
	return &c, nil
}

// ---------- records & chain ----------

type Record struct {
	Node      string `json:"node"`
	Seq       int64  `json:"seq"`
	TS        int64  `json:"ts"` // unix seconds
	NetType   string `json:"netType"`
	SSID      string `json:"ssid,omitempty"`
	LocalIP   string `json:"localIP"`
	TSIP      string `json:"tsIP"`
	UptimeSec int64  `json:"uptimeSec"`
	PrevHash  string `json:"prevHash"`
	Hash      string `json:"hash"`
}

// version se inyecta en compilación con -ldflags "-X main.version=...".
// Fuera de la cadena hash a propósito: es metadato del binario, no del estado
// de red, y meterlo en Record obligaría a re-hashear todo el historial.
var version = "dev"

// hashOf computes the record hash over a canonical field ordering (excluding Hash).
func hashOf(r Record) string {
	payload := fmt.Sprintf("%s|%d|%d|%s|%s|%s|%s|%d|%s",
		r.Node, r.Seq, r.TS, r.NetType, r.SSID, r.LocalIP, r.TSIP, r.UptimeSec, r.PrevHash)
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])
}

// sameStatus reports whether two records describe the same network state
// (ignoring time/uptime/chain fields).
func sameStatus(a, b Record) bool {
	return a.NetType == b.NetType && a.SSID == b.SSID && a.LocalIP == b.LocalIP && a.TSIP == b.TSIP
}

// ---------- store ----------

type Store struct {
	mu     sync.RWMutex
	dir    string
	chains map[string][]Record // node -> ordered records
}

func openStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, chains: map[string][]Record{}}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		node := strings.TrimSuffix(e.Name(), ".jsonl")
		recs, err := readChainFile(filepath.Join(dir, e.Name()))
		if err != nil {
			log.Printf("store: skipping %s: %v", e.Name(), err)
			continue
		}
		s.chains[node] = recs
	}
	return s, nil
}

func readChainFile(path string) ([]Record, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var recs []Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var r Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			return nil, fmt.Errorf("bad line: %w", err)
		}
		recs = append(recs, r)
	}
	if err := verifyChain(recs); err != nil {
		return nil, err
	}
	return recs, sc.Err()
}

func verifyChain(recs []Record) error {
	prev := ""
	for i, r := range recs {
		if r.Seq != int64(i+1) {
			return fmt.Errorf("seq gap at %d (got %d)", i+1, r.Seq)
		}
		if r.PrevHash != prev {
			return fmt.Errorf("prevHash mismatch at seq %d", r.Seq)
		}
		if hashOf(r) != r.Hash {
			return fmt.Errorf("hash mismatch at seq %d", r.Seq)
		}
		prev = r.Hash
	}
	return nil
}

// Append validates that recs extend node's chain contiguously and persists them.
func (s *Store) Append(node string, recs []Record) error {
	if len(recs) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	chain := s.chains[node]
	prevHash := ""
	nextSeq := int64(1)
	if n := len(chain); n > 0 {
		prevHash = chain[n-1].Hash
		nextSeq = chain[n-1].Seq + 1
	}
	for _, r := range recs {
		if r.Node != node || r.Seq != nextSeq || r.PrevHash != prevHash || hashOf(r) != r.Hash {
			return fmt.Errorf("record seq %d does not extend chain for %s", r.Seq, node)
		}
		prevHash = r.Hash
		nextSeq++
	}
	f, err := os.OpenFile(filepath.Join(s.dir, node+".jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	for _, r := range recs {
		b, _ := json.Marshal(r)
		if _, err := f.Write(append(b, '\n')); err != nil {
			return err
		}
	}
	s.chains[node] = append(chain, recs...)
	return nil
}

func (s *Store) Head(node string) (Record, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c := s.chains[node]
	if len(c) == 0 {
		return Record{}, false
	}
	return c[len(c)-1], true
}

func (s *Store) After(node string, after int64) []Record {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c := s.chains[node]
	if after < 0 {
		after = 0
	}
	if int(after) >= len(c) {
		return nil
	}
	out := make([]Record, len(c)-int(after))
	copy(out, c[after:])
	return out
}

func (s *Store) Nodes() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []string
	for n := range s.chains {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func (s *Store) Verify(node string) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c := s.chains[node]
	return len(c), verifyChain(c)
}

// ---------- collectors ----------

func run(cmd string, args ...string) string {
	// Timeout duro: el CLI de Tailscale en macOS, invocado como root sin
	// sesión gráfica, no falla — SE CUELGA esperando a la GUI. Sin esto,
	// tailscaleIP() se congelaba para siempre y el nodo quedaba solo en
	// localhost tras cada reboot.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, cmd, args...)
	out, err := c.Output()
	if err != nil {
		return ""
	}
	return string(out)
}

func localIP() string {
	conn, err := net.Dial("udp", "8.8.8.8:53")
	if err != nil {
		return ""
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String()
}

func tailscaleIP() string {
	paths := []string{"tailscale"}
	switch runtime.GOOS {
	case "darwin":
		paths = append(paths, "/Applications/Tailscale.app/Contents/MacOS/Tailscale", "/usr/local/bin/tailscale")
	case "windows":
		paths = append(paths, `C:\Program Files\Tailscale\tailscale.exe`)
	}
	_, cgnat, _ := net.ParseCIDR("100.64.0.0/10")
	for _, p := range paths {
		out := strings.TrimSpace(run(p, "ip", "-4"))
		// the CLI can exit 0 while printing an error (e.g. GUI not running) —
		// only trust output that parses as a CGNAT-range IP
		if ip := net.ParseIP(strings.Split(out, "\n")[0]); ip != nil && cgnat.Contains(ip) {
			return ip.String()
		}
	}
	// fallback: scan interfaces for CGNAT 100.64.0.0/10. Las utun punto-a-punto
	// de macOS pueden llegar como *net.IPAddr, no solo *net.IPNet — aceptar ambas
	// (bug real: un nodo quedaba solo en localhost tras cada reboot).
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		var ip net.IP
		switch v := a.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		if ip != nil && cgnat.Contains(ip) {
			return ip.String()
		}
	}
	return ""
}

func defaultIface() string {
	switch runtime.GOOS {
	case "darwin":
		for _, line := range strings.Split(run("route", "-n", "get", "default"), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "interface:") {
				return strings.TrimSpace(strings.TrimPrefix(line, "interface:"))
			}
		}
	case "linux":
		b, err := os.ReadFile("/proc/net/route")
		if err != nil {
			return ""
		}
		for _, line := range strings.Split(string(b), "\n")[1:] {
			f := strings.Fields(line)
			if len(f) > 1 && f[1] == "00000000" {
				return f[0]
			}
		}
	}
	return ""
}

var ssidRe = regexp.MustCompile(`(?m)^\s*SSID\s*:?\s*(.+)$`)

// netStatus returns (netType, ssid).
func netStatus() (string, string) {
	switch runtime.GOOS {
	case "darwin":
		iface := defaultIface()
		if iface == "" {
			return "unknown", ""
		}
		ports := run("networksetup", "-listallhardwareports")
		portType := ""
		blocks := strings.Split(ports, "Hardware Port: ")
		for _, b := range blocks {
			if strings.Contains(b, "Device: "+iface+"\n") {
				portType = strings.SplitN(b, "\n", 2)[0]
				break
			}
		}
		if strings.Contains(portType, "Wi-Fi") || strings.Contains(portType, "AirPort") {
			// macOS redacts the SSID for processes without Location Services
			// permission; wdutil reports it truthfully when running as root.
			ssid := ""
			if os.Geteuid() == 0 {
				for _, line := range strings.Split(run("wdutil", "info"), "\n") {
					if i := strings.Index(line, "SSID"); i >= 0 && strings.Contains(line, ":") {
						v := strings.TrimSpace(line[strings.Index(line, ":")+1:])
						if v != "" && !strings.EqualFold(v, "none") {
							ssid = v
						}
						break
					}
				}
			}
			if ssid == "" {
				if m := ssidRe.FindStringSubmatch(run("ipconfig", "getsummary", iface)); m != nil {
					ssid = strings.TrimSpace(m[1])
				}
			}
			if ssid == "" {
				out := run("networksetup", "-getairportnetwork", iface)
				if i := strings.Index(out, ": "); i >= 0 {
					ssid = strings.TrimSpace(out[i+2:])
				}
			}
			if strings.Contains(ssid, "redacted") { // literal "<redacted>" placeholder
				ssid = ""
			}
			return "wifi", ssid
		}
		if portType != "" {
			return "ethernet", ""
		}
		return "unknown", ""
	case "linux":
		iface := defaultIface()
		if iface == "" {
			return "unknown", ""
		}
		if _, err := os.Stat("/sys/class/net/" + iface + "/wireless"); err == nil {
			ssid := ""
			out := run("iw", "dev", iface, "link")
			for _, line := range strings.Split(out, "\n") {
				if i := strings.Index(line, "SSID: "); i >= 0 {
					ssid = strings.TrimSpace(line[i+6:])
				}
			}
			return "wifi", ssid
		}
		return "ethernet", ""
	case "windows":
		// netsh prints an SSID line only when a WLAN is connected; works across
		// locales (the label "SSID" is not translated). No SSID => wired.
		out := run("netsh", "wlan", "show", "interfaces")
		if m := ssidRe.FindStringSubmatch(out); m != nil && strings.TrimSpace(m[1]) != "" {
			return "wifi", strings.TrimSpace(m[1])
		}
		return "ethernet", ""
	}
	return "unknown", ""
}

func collect(cfg *Config) Record {
	netType, ssid := netStatus()
	if ssid == "" && len(cfg.Networks) > 0 {
		if name := cfg.Networks[gatewayMAC()]; name != "" {
			ssid = name
		}
	}
	tsIP := cfg.TSIP
	if tsIP == "" {
		tsIP = tailscaleIP()
	}
	return Record{
		Node:      cfg.Node,
		TS:        time.Now().Unix(),
		NetType:   netType,
		SSID:      ssid,
		LocalIP:   localIP(),
		TSIP:      tsIP,
		UptimeSec: uptimeSeconds(), // per-platform file
	}
}

var macPartRe = regexp.MustCompile(`(?i)([0-9a-f]{1,2})[:-]([0-9a-f]{1,2})[:-]([0-9a-f]{1,2})[:-]([0-9a-f]{1,2})[:-]([0-9a-f]{1,2})[:-]([0-9a-f]{1,2})`)

// gatewayMAC returns the default gateway's MAC, normalized to lowercase
// zero-padded colon form (e.g. aa:bb:cc:dd:ee:ff), or "".
func gatewayMAC() string {
	gw := ""
	switch runtime.GOOS {
	case "darwin":
		for _, line := range strings.Split(run("route", "-n", "get", "default"), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "gateway:") {
				gw = strings.TrimSpace(strings.TrimPrefix(line, "gateway:"))
			}
		}
		if gw == "" {
			return ""
		}
		return normMAC(run("arp", "-n", gw))
	case "linux":
		b, _ := os.ReadFile("/proc/net/route")
		var iface, hexGW string
		for _, line := range strings.Split(string(b), "\n")[1:] {
			f := strings.Fields(line)
			if len(f) > 2 && f[1] == "00000000" {
				iface, hexGW = f[0], f[2]
				break
			}
		}
		if hexGW == "" {
			return ""
		}
		var ip [4]int64
		fmt.Sscanf(hexGW, "%02x%02x%02x%02x", &ip[3], &ip[2], &ip[1], &ip[0])
		gw = fmt.Sprintf("%d.%d.%d.%d", ip[0], ip[1], ip[2], ip[3])
		return normMAC(run("ip", "neigh", "show", gw, "dev", iface))
	case "windows":
		for _, line := range strings.Split(run("route", "print", "0.0.0.0"), "\n") {
			f := strings.Fields(line)
			if len(f) >= 3 && f[0] == "0.0.0.0" && f[1] == "0.0.0.0" {
				gw = f[2]
				break
			}
		}
		if gw == "" {
			return ""
		}
		return normMAC(run("arp", "-a", gw))
	}
	return ""
}

func normMAC(s string) string {
	m := macPartRe.FindStringSubmatch(s)
	if m == nil {
		return ""
	}
	parts := make([]string, 6)
	for i := 0; i < 6; i++ {
		p := strings.ToLower(m[i+1])
		if len(p) == 1 {
			p = "0" + p
		}
		parts[i] = p
	}
	return strings.Join(parts, ":")
}

// ---------- location & LAN check ----------

// Physical topology: node name -> human-readable site label, loaded from the
// "locations" map in the config file. It also defines the set of nodes the
// dashboard knows about, so a node absent from every peer's log still shows
// up as offline instead of vanishing.
var nodeLocation = map[string]string{}

// lanPeerNode derives the mesh node name from an mDNS hostname by stripping
// ".local" — true for both "host.local" and "host", so one config field
// (LANPeer) is enough.
func lanPeerNode(cfg *Config) string {
	return strings.TrimSuffix(cfg.LANPeer, ".local")
}

type lanCheckState struct {
	mu        sync.RWMutex
	reachable bool
	checkedAt int64
}

var lanState lanCheckState

func pingHost(host string) bool {
	var args []string
	switch runtime.GOOS {
	case "darwin":
		// -W es en MILISEGUNDOS en macOS, no en segundos.
		args = []string{"-c", "2", "-W", "2000", host}
	case "linux":
		args = []string{"-c", "2", "-W", "2", host}
	default:
		args = []string{"-n", "2", host}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "ping", args...).Run() == nil
}

// lanPeerLoop pings cfg.LANPeer on the same cadence as the collector, so a
// tailscale outage on the peer shows up as "alive on LAN" within one cycle.
func lanPeerLoop(cfg *Config) {
	if cfg.LANPeer == "" {
		return
	}
	for {
		ok := pingHost(cfg.LANPeer)
		lanState.mu.Lock()
		lanState.reachable = ok
		lanState.checkedAt = time.Now().Unix()
		lanState.mu.Unlock()
		time.Sleep(time.Duration(cfg.CollectSecs) * time.Second)
	}
}

// bleState mirrors lanState but the value comes from an external process
// (nodebeacon) instead of a check nodemesh runs itself — polled off disk
// rather than computed in-process.
type bleCheckState struct {
	mu    sync.RWMutex
	check *BLECheck
}

var bleState bleCheckState

// nodebeaconState is the shape nodebeacon writes to state.json on every
// beacon it validates (overwritten in place, not appended — always just
// "the latest thing I heard"). Kept separate from BLECheck (the API/gossip
// shape) so a field rename on either side doesn't silently break the other.
type nodebeaconState struct {
	Status   string `json:"status"`
	RSSI     int    `json:"rssi"`
	LastSeen int64  `json:"last_seen"`
}

// bleWatchLoop polls nodebeacon's state.json rather than parsing its log —
// a structured file that's always "just the latest" is far less fragile
// than grepping log lines, and matches the state.json convention
// presencia-agent already uses for ble_scan.py.
func bleWatchLoop(cfg *Config) {
	if cfg.BLEStatePath == "" {
		return
	}
	for {
		if data, err := os.ReadFile(cfg.BLEStatePath); err == nil {
			var raw nodebeaconState
			if json.Unmarshal(data, &raw) == nil {
				bleState.mu.Lock()
				bleState.check = &BLECheck{PeerStatus: raw.Status, RSSI: raw.RSSI, LastSeen: raw.LastSeen}
				bleState.mu.Unlock()
			}
		}
		time.Sleep(time.Duration(cfg.CollectSecs) * time.Second)
	}
}

// ---------- gossip ----------

type LANCheck struct {
	Reachable bool  `json:"reachable"`
	CheckedAt int64 `json:"checkedAt"`
}

// BLECheck: like LANCheck but sourced from nodebeacon's HMAC-authenticated
// Bluetooth beacon instead of a ping — works even if the shared LAN itself
// is down, since it's a direct radio link between the pair, no router or
// switch involved. PeerStatus is nodebeacon's own view of ITS internet
// (ok|sin_internet), not a reachability flag for the peer's mesh port.
type BLECheck struct {
	PeerStatus string `json:"peerStatus"`
	RSSI       int    `json:"rssi"`
	LastSeen   int64  `json:"lastSeen"`
}

type nodeInfo struct {
	Record
	State    string    `json:"state"` // online | stale | offline
	TSActive bool      `json:"tsActive"`
	Location string    `json:"location,omitempty"`
	Version  string    `json:"version,omitempty"`  // del propio binario; el de los pares llega por gossip
	LANCheck *LANCheck `json:"lanCheck,omitempty"` // only set by whichever node actually checked
	BLECheck *BLECheck `json:"bleCheck,omitempty"` // only set by whichever node actually checked
}

// Versiones de los pares, aprendidas en cada ronda de gossip al leer su
// /api/nodes. No se persisten: son del binario que corre AHORA, así que
// reconstruirlas en cada arranque es lo correcto.
var peerVersions = struct {
	mu sync.RWMutex
	m  map[string]string
}{m: map[string]string{}}

func setPeerVersion(node, v string) {
	if v == "" {
		return
	}
	peerVersions.mu.Lock()
	peerVersions.m[node] = v
	peerVersions.mu.Unlock()
}

func getPeerVersion(node string) string {
	peerVersions.mu.RLock()
	defer peerVersions.mu.RUnlock()
	return peerVersions.m[node]
}

// peerNodeName averigua cuál de los nodos que devuelve un peer es el peer
// mismo, comparando su IP de Tailscale con la que acabamos de consultar. Sirve
// para quedarnos SOLO con la versión que ese nodo conoce de primera mano: la
// que reporta de terceros es copia de su propia caché y propagarla acabaría
// sirviendo versiones viejas indefinidamente.
func peerNodeName(infos map[string]nodeInfo, peerIP string) string {
	for n, info := range infos {
		if info.TSIP != "" && info.TSIP == peerIP {
			return n
		}
	}
	return ""
}

func gossipOnce(cfg *Config, store *Store, client *http.Client) {
	known := map[string]bool{}
	for _, n := range store.Nodes() {
		known[n] = true
	}
	for _, peer := range cfg.Peers {
		base := fmt.Sprintf("http://%s:%d", peer, cfg.Port)
		if strings.Contains(peer, ":") { // peer with explicit port
			base = "http://" + peer
		}
		// learn peer's node set
		var infos map[string]nodeInfo
		if err := getJSON(client, base+"/api/nodes", &infos); err != nil {
			continue // peer offline; normal
		}
		for n, info := range infos {
			known[n] = true
			// Cada nodo solo conoce con certeza SU propia versión; la que
			// reporta de terceros es de segunda mano, así que se ignora.
			if n == peerNodeName(infos, strings.Split(peer, ":")[0]) {
				setPeerVersion(n, info.Version)
			}
		}
		// pull chain extensions for every known node
		for n := range known {
			if n == cfg.Node {
				continue // own chain is authoritative locally
			}
			var have int64
			if head, ok := store.Head(n); ok {
				have = head.Seq
			}
			var recs []Record
			url := fmt.Sprintf("%s/api/log/%s?after=%d", base, n, have)
			if err := getJSON(client, url, &recs); err != nil || len(recs) == 0 {
				continue
			}
			if err := store.Append(n, recs); err != nil {
				log.Printf("gossip: reject %d recs for %s from %s: %v", len(recs), n, peer, err)
			}
		}
	}
}

func getJSON(client *http.Client, url string, v any) error {
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("http %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// ---------- http ----------

func newMux(cfg *Config, store *Store) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(uiHTML)
	})
	mux.HandleFunc("/api/nodes", func(w http.ResponseWriter, r *http.Request) {
		out := map[string]nodeInfo{}
		now := time.Now().Unix()
		// Un nodo sin cadena (o con cadena vacía) NO se esconde: se reporta
		// offline. Antes esto hacía `continue` y el nodo simplemente
		// desaparecía de la página — justo lo contrario de lo que uno quiere
		// ver cuando algo se cayó o nunca llegó a reportar.
		emitOffline := func(n string) {
			out[n] = nodeInfo{Record: Record{Node: n}, State: "offline", Location: nodeLocation[n]}
		}
		for _, n := range store.Nodes() {
			head, ok := store.Head(n)
			if !ok {
				emitOffline(n)
				continue
			}
			// heartbeats land every 10 min; gossip adds up to ~4 min of lag
			state := "online"
			age := now - head.TS
			if age > 45*60 {
				state = "offline"
			} else if age > 16*60 {
				state = "stale"
			}
			info := nodeInfo{Record: head, State: state, TSActive: head.TSIP != "", Location: nodeLocation[n]}
			if n == cfg.Node {
				info.Version = version
			} else {
				info.Version = getPeerVersion(n)
			}
			if n == lanPeerNode(cfg) {
				lanState.mu.RLock()
				if lanState.checkedAt > 0 {
					info.LANCheck = &LANCheck{Reachable: lanState.reachable, CheckedAt: lanState.checkedAt}
				}
				lanState.mu.RUnlock()

				bleState.mu.RLock()
				if bleState.check != nil {
					c := *bleState.check
					info.BLECheck = &c
				}
				bleState.mu.RUnlock()
			}
			out[n] = info
		}
		// Nodos que la topología fija conoce pero de los que este nodo no
		// tiene cadena todavía (recién agregados al tailnet, o nunca vistos):
		// también salen, offline, en vez de faltar en la página.
		for n := range nodeLocation {
			if _, ok := out[n]; !ok {
				emitOffline(n)
			}
		}
		writeJSON(w, out)
	})
	mux.HandleFunc("/api/log/", func(w http.ResponseWriter, r *http.Request) {
		node := strings.TrimPrefix(r.URL.Path, "/api/log/")
		var after int64
		fmt.Sscanf(r.URL.Query().Get("after"), "%d", &after)
		writeJSON(w, store.After(node, after))
	})
	mux.HandleFunc("/api/history/", func(w http.ResponseWriter, r *http.Request) {
		node := strings.TrimPrefix(r.URL.Path, "/api/history/")
		limit := 50
		fmt.Sscanf(r.URL.Query().Get("limit"), "%d", &limit)
		recs := store.After(node, 0)
		// newest first
		out := make([]Record, 0, limit)
		for i := len(recs) - 1; i >= 0 && len(out) < limit; i-- {
			out = append(out, recs[i])
		}
		writeJSON(w, out)
	})
	mux.HandleFunc("/api/ports", portsHandler)
	mux.HandleFunc("/api/host", hostHandler)
	// localhost-only: exit so the supervisor (launchd/systemd/task KeepAlive)
	// restarts us with a freshly-deployed binary — no sudo needed for upgrades.
	mux.HandleFunc("/api/restart", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasPrefix(r.RemoteAddr, "127.0.0.1:") {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.Write([]byte("restarting\n"))
		go func() { time.Sleep(300 * time.Millisecond); os.Exit(0) }()
	})
	// Prometheus text-format exposition — only emits the BLE beacon signal
	// for now (that's what was asked for), not a full re-export of
	// /api/nodes. Only ever non-empty on nodes with BLEStatePath set.
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		bleState.mu.RLock()
		c := bleState.check
		bleState.mu.RUnlock()
		if c == nil {
			return
		}
		peer := lanPeerNode(cfg)
		up := 0
		if c.PeerStatus == "ok" {
			up = 1
		}
		fmt.Fprintf(w, "# HELP nodemesh_ble_peer_up 1 if the peer's last BLE beacon reported internet ok, 0 if sin_internet\n")
		fmt.Fprintf(w, "# TYPE nodemesh_ble_peer_up gauge\n")
		fmt.Fprintf(w, "nodemesh_ble_peer_up{node=%q,peer=%q} %d\n", cfg.Node, peer, up)
		fmt.Fprintf(w, "# HELP nodemesh_ble_peer_rssi_dbm last RSSI seen for the BLE peer, in dBm\n")
		fmt.Fprintf(w, "# TYPE nodemesh_ble_peer_rssi_dbm gauge\n")
		fmt.Fprintf(w, "nodemesh_ble_peer_rssi_dbm{node=%q,peer=%q} %d\n", cfg.Node, peer, c.RSSI)
		fmt.Fprintf(w, "# HELP nodemesh_ble_last_seen_timestamp_seconds unix timestamp of the last valid BLE beacon received\n")
		fmt.Fprintf(w, "# TYPE nodemesh_ble_last_seen_timestamp_seconds gauge\n")
		fmt.Fprintf(w, "nodemesh_ble_last_seen_timestamp_seconds{node=%q,peer=%q} %d\n", cfg.Node, peer, c.LastSeen)
	})
	mux.HandleFunc("/api/verify/", func(w http.ResponseWriter, r *http.Request) {
		node := strings.TrimPrefix(r.URL.Path, "/api/verify/")
		n, err := store.Verify(node)
		res := map[string]any{"node": node, "length": n, "valid": err == nil}
		if err != nil {
			res["error"] = err.Error()
		}
		writeJSON(w, res)
	})
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// serveOn keeps trying to bind addr (laptops get their tailscale IP late).
func serveOn(addr string, mux *http.ServeMux) {
	avisado := false
	for {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			// un error silencioso aquí costó una hora de arqueología: loguear
			// la primera falla (no cada reintento, que inundaría el log)
			if !avisado {
				log.Printf("bind %s: %v (reintentando cada 15s)", addr, err)
				avisado = true
			}
			time.Sleep(15 * time.Second)
			continue
		}
		avisado = false
		log.Printf("listening on %s", addr)
		if err := http.Serve(ln, mux); err != nil {
			log.Printf("serve %s: %v; rebinding", addr, err)
		}
		time.Sleep(5 * time.Second)
	}
}

// ---------- main ----------

func main() {
	cfgPath := flag.String("config", "nodemesh.json", "path to config file")
	flag.Parse()
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	store, err := openStore(cfg.DataDir)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	log.Printf("nodemesh starting: node=%s port=%d peers=%v", cfg.Node, cfg.Port, cfg.Peers)

	// collector loop
	go func() {
		heartbeat := int64(600)
		for {
			r := collect(cfg)
			head, ok := store.Head(cfg.Node)
			if !ok || !sameStatus(head, r) || r.TS-head.TS >= heartbeat {
				r.Seq = 1
				if ok {
					r.Seq = head.Seq + 1
					r.PrevHash = head.Hash
				}
				r.Hash = hashOf(r)
				if err := store.Append(cfg.Node, []Record{r}); err != nil {
					log.Printf("collect: %v", err)
				}
			}
			time.Sleep(time.Duration(cfg.CollectSecs) * time.Second)
		}
	}()

	// gossip loop
	go func() {
		client := &http.Client{Timeout: 10 * time.Second}
		for {
			gossipOnce(cfg, store, client)
			time.Sleep(time.Duration(cfg.GossipSecs) * time.Second)
		}
	}()

	// LAN peer check loop (only when LANPeer is set — see Config)
	go lanPeerLoop(cfg)
	// BLE beacon state watch loop (only when BLEStatePath is set — see Config)
	go bleWatchLoop(cfg)

	mux := newMux(cfg, store)
	go serveOn("127.0.0.1:"+fmt.Sprint(cfg.Port), mux)
	// bind tailscale IP (retry until present, rebind if it changes)
	debugUnaVez := false
	for {
		ip := cfg.TSIP
		if ip == "" {
			ip = tailscaleIP()
		}
		if ip == "" {
			if !debugUnaVez {
				addrs, _ := net.InterfaceAddrs()
				log.Printf("tailscaleIP vacío; interfaces vistas: %v (fija \"tsIP\" en el config si este host no puede autodetectarla)", addrs)
				debugUnaVez = true
			}
			time.Sleep(15 * time.Second)
			continue
		}
		serveOn(ip+":"+fmt.Sprint(cfg.Port), mux)
	}
}
