package main

import (
	"net/http"
	"runtime"
	"sort"
	"sync"
	"time"
)

// Specs describe la máquina: lo estático (modelo, CPU, núcleos, RAM y disco
// totales) y lo volátil (RAM ocupada, disco libre, carga).
//
// Va FUERA de la cadena hash a propósito, por la misma razón que `version`: la
// cadena registra CAMBIOS DE ESTADO DE RED y solo crece cuando sameStatus()
// detecta uno. La RAM ocupada cambia cada minuto, así que meterla en Record
// convertiría un log de transiciones en una serie temporal sin tope. Los specs
// viajan por gossip como campo vivo de /api/nodes, con CollectedAt para poder
// distinguir un dato fresco de uno rancio.
type Specs struct {
	Model       string  `json:"model,omitempty"`
	OS          string  `json:"os,omitempty"`
	Arch        string  `json:"arch,omitempty"`
	CPU         string  `json:"cpu,omitempty"`
	CoresPhys   int     `json:"coresPhys,omitempty"`
	CoresLog    int     `json:"coresLog,omitempty"`
	MemTotalMB  int64   `json:"memTotalMB,omitempty"`
	MemUsedMB   int64   `json:"memUsedMB,omitempty"`
	DiskTotalGB float64 `json:"diskTotalGB,omitempty"` // el volumen de DataDir (ver specsDiskPath)
	DiskFreeGB  float64 `json:"diskFreeGB,omitempty"`
	Load1       float64 `json:"load1,omitempty"` // solo unix: Windows no tiene loadavg
	CollectedAt int64   `json:"collectedAt,omitempty"`
}

// specsTTL: cada cuánto se refresca la parte volátil. El gossip y la UI piden
// /api/nodes cada 30 s y ambos comparten esta caché, así que el coste real es
// un vm_stat (o cero syscalls en Linux) cada medio minuto, no uno por petición.
const specsTTL = 30 * time.Second

// specsDiskPath es el volumen que se mide, fijado en main() al DataDir del
// nodo. NO se usa "/": en Android la raíz es la partición de sistema, de solo
// lectura y siempre al 100% (1,2 GB en el R1), mientras el almacenamiento real
// —los 104 GB donde nodemesh de hecho escribe— cuelga de otro punto de montaje.
// Medir donde el proceso escribe es además lo que uno quiere saber: es el disco
// que se puede llenar.
var specsDiskPath = "/"

var specsCache = struct {
	mu     sync.Mutex
	once   sync.Once
	static Specs
	last   Specs
	at     time.Time
}{}

// localSpecs devuelve los specs de ESTE nodo. Lo estático se paga una sola vez
// por proceso (en Windows es un powershell, que no se quiere en cada consulta);
// lo volátil se recalcula como mucho cada specsTTL.
func localSpecs() Specs {
	specsCache.mu.Lock()
	defer specsCache.mu.Unlock()
	if !specsCache.at.IsZero() && time.Since(specsCache.at) < specsTTL {
		return specsCache.last
	}
	specsCache.once.Do(func() {
		s := platformStatic()
		s.Arch = runtime.GOARCH
		if s.CoresLog == 0 {
			s.CoresLog = runtime.NumCPU()
		}
		if s.CoresPhys == 0 {
			s.CoresPhys = s.CoresLog
		}
		specsCache.static = s
	})
	s := specsCache.static
	s.MemUsedMB = memUsedMB()
	s.DiskTotalGB, s.DiskFreeGB = diskGB(specsDiskPath)
	s.Load1 = load1()
	s.CollectedAt = time.Now().Unix()
	specsCache.last, specsCache.at = s, time.Now()
	return s
}

var peerSpecs = struct {
	mu sync.RWMutex
	m  map[string]Specs
}{m: map[string]Specs{}}

// setPeerSpecs guarda los specs de otro nodo aprendidos por gossip.
//
// A diferencia de las versiones, aquí SÍ se acepta información de segunda mano
// (un peer contándonos de un tercero al que no llegamos directamente): Specs
// lleva CollectedAt, así que siempre se puede conservar la copia más nueva y
// descartar la rancia. Ese timestamp es justo lo que le faltaba a Version para
// poder relevarse sin acabar sirviendo datos viejos para siempre.
func setPeerSpecs(node string, s *Specs) {
	if s == nil || s.CollectedAt == 0 {
		return
	}
	peerSpecs.mu.Lock()
	defer peerSpecs.mu.Unlock()
	if cur, ok := peerSpecs.m[node]; ok && cur.CollectedAt >= s.CollectedAt {
		return
	}
	peerSpecs.m[node] = *s
}

func getPeerSpecs(node string) *Specs {
	peerSpecs.mu.RLock()
	defer peerSpecs.mu.RUnlock()
	s, ok := peerSpecs.m[node]
	if !ok {
		return nil
	}
	return &s
}

// Capacity es la suma de la flota: lo que antes había que ir a sacar a mano
// nodo por nodo con ssh. Solo cuenta los nodos que NO están offline —un equipo
// apagado no aporta capacidad— y expone en Missing los que sí cuentan pero de
// los que todavía no llegaron specs, para que el total nunca mienta por omisión.
type Capacity struct {
	Nodes       int              `json:"nodes"`
	CoresPhys   int              `json:"coresPhys"`
	CoresLog    int              `json:"coresLog"`
	MemTotalGB  float64          `json:"memTotalGB"`
	MemUsedGB   float64          `json:"memUsedGB"`
	DiskTotalGB float64          `json:"diskTotalGB"`
	DiskFreeGB  float64          `json:"diskFreeGB"`
	Missing     []string         `json:"missing,omitempty"`
	Per         map[string]Specs `json:"per"`
}

func capacityOf(infos map[string]nodeInfo) Capacity {
	c := Capacity{Per: map[string]Specs{}}
	for name, info := range infos {
		if info.State == "offline" {
			continue
		}
		if info.Specs == nil {
			c.Missing = append(c.Missing, name)
			continue
		}
		s := *info.Specs
		c.Nodes++
		c.CoresPhys += s.CoresPhys
		c.CoresLog += s.CoresLog
		c.MemTotalGB += float64(s.MemTotalMB) / 1024
		c.MemUsedGB += float64(s.MemUsedMB) / 1024
		c.DiskTotalGB += s.DiskTotalGB
		c.DiskFreeGB += s.DiskFreeGB
		c.Per[name] = s
	}
	sort.Strings(c.Missing)
	return c
}

func specsHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, localSpecs())
}
