package main

// IP pública de salida: la dirección desde la que este nodo sale a internet.
//
// Para qué: alimentar whitelists / firewalls externos a los que hay que
// decirles "estas son las IPs públicas desde las que mi flota puede llegar".
// El dato viaja en el gossip igual que [Specs], con CollectedAt para que la
// copia más nueva gane; NO va en la cadena hash (es volátil y la haría crecer
// sin tope).
//
// Fuente primaria: STUN (RFC 5389). Un binding request UDP de 20 bytes
// obtiene la IP:puerto desde la que el servidor vio llegar el datagrama —
// que es exactamente la IP pública de salida. Hecho a mano con stdlib para
// no tocar go.mod (ver nota de Go 1.20 en go.mod). Fallback a servicios
// HTTP si STUN está bloqueado (raro en esta flota) o si todos los servidores
// STUN fallan.
//
// Separado del `ipPublicaLoop` de placement.go a propósito: aquel sirve al
// placement y solo se activa cuando el nodo tiene PublicIPURL configurado
// (hoy, el R1). Esto reporta en TODOS los nodos, sin configurar nada.

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Egress es la IP pública de salida con su procedencia. Replicable: lleva
// CollectedAt (mismo patrón que [Specs]).
type Egress struct {
	IPv4        string `json:"ipv4,omitempty"`
	Source      string `json:"source,omitempty"` // "stun:host:port" | "http:url" | ""
	CheckedAt   int64  `json:"checkedAt,omitempty"`
	LastError   string `json:"lastError,omitempty"`
	CollectedAt int64  `json:"collectedAt,omitempty"`
}

// Servidores en orden de preferencia. STUN primero (barato, protocolo
// pensado para esto), HTTP de respaldo. Se cae uno, se prueba el siguiente;
// en cuanto uno responde, se usa esa.
var stunServers = []string{
	"stun.l.google.com:19302",
	"stun.cloudflare.com:3478",
	"stun.nextcloud.com:443",
}

var httpFallbacks = []string{
	"https://api.ipify.org",
	"https://ifconfig.co/ip",
}

const (
	egressInterval  = 15 * time.Minute
	egressHTTPTimeo = 10 * time.Second
	egressSTUNTimeo = 3 * time.Second
)

// ---------- STUN mínimo ----------
//
// RFC 5389, binding request + XOR-MAPPED-ADDRESS.
//
//	Header (20 bytes):
//	  type (2)   = 0x0001
//	  length (2) = 0
//	  cookie (4) = 0x2112A442
//	  txID (12)  = aleatorio
//
// Respuesta: mismo header con type 0x0101, length>0, seguido de atributos TLV.
// El atributo 0x0020 (XOR-MAPPED-ADDRESS) trae la IP:puerto XOR-eado con el
// cookie (los dos primeros bytes del cookie para el puerto, los cuatro para
// la IPv4).

const (
	stunBindingRequest       uint16 = 0x0001
	stunBindingSuccess       uint16 = 0x0101
	stunMagicCookie          uint32 = 0x2112A442
	stunAttrXORMappedAddress uint16 = 0x0020
	stunAttrMappedAddress    uint16 = 0x0001 // legacy, por si el servidor es viejo
	stunFamilyIPv4           uint8  = 0x01
)

// stunQuery abre un socket UDP, manda un binding request y devuelve la IPv4
// pública tal como la vio el servidor. Devuelve "" y error si algo falla.
func stunQuery(server string) (string, error) {
	conn, err := net.DialTimeout("udp", server, egressSTUNTimeo)
	if err != nil {
		return "", fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(egressSTUNTimeo))

	req := make([]byte, 20)
	binary.BigEndian.PutUint16(req[0:2], stunBindingRequest)
	binary.BigEndian.PutUint16(req[2:4], 0) // length
	binary.BigEndian.PutUint32(req[4:8], stunMagicCookie)
	if _, err := rand.Read(req[8:20]); err != nil {
		return "", fmt.Errorf("txID: %w", err)
	}
	if _, err := conn.Write(req); err != nil {
		return "", fmt.Errorf("write: %w", err)
	}
	buf := make([]byte, 512)
	n, err := conn.Read(buf)
	if err != nil {
		return "", fmt.Errorf("read: %w", err)
	}
	return parseSTUNResponse(buf[:n], req[4:20])
}

// parseSTUNResponse extrae la IPv4 del XOR-MAPPED-ADDRESS (o MAPPED-ADDRESS
// como respaldo) de una respuesta STUN. txIDWithCookie es cookie+txID (16B)
// del request, usado para validar que la respuesta corresponde.
func parseSTUNResponse(resp []byte, txIDWithCookie []byte) (string, error) {
	if len(resp) < 20 {
		return "", fmt.Errorf("respuesta corta: %d bytes", len(resp))
	}
	msgType := binary.BigEndian.Uint16(resp[0:2])
	if msgType != stunBindingSuccess {
		return "", fmt.Errorf("tipo inesperado 0x%04x", msgType)
	}
	msgLen := int(binary.BigEndian.Uint16(resp[2:4]))
	if 20+msgLen > len(resp) {
		return "", fmt.Errorf("longitud declarada %d excede el paquete (%d)", msgLen, len(resp))
	}
	// Validar que cookie+txID coincide con el request — si no, no es nuestra.
	if len(txIDWithCookie) != 16 || !bytesEqual(resp[4:20], txIDWithCookie) {
		return "", fmt.Errorf("cookie/txID no coincide")
	}
	attrs := resp[20 : 20+msgLen]
	for len(attrs) >= 4 {
		attrType := binary.BigEndian.Uint16(attrs[0:2])
		attrLen := int(binary.BigEndian.Uint16(attrs[2:4]))
		if 4+attrLen > len(attrs) {
			return "", fmt.Errorf("atributo truncado")
		}
		val := attrs[4 : 4+attrLen]
		switch attrType {
		case stunAttrXORMappedAddress:
			if len(val) >= 8 && val[1] == stunFamilyIPv4 {
				// Bytes 4..8 son la IPv4 XOR-eada con los 4 bytes altos del cookie.
				ip := make([]byte, 4)
				for i := 0; i < 4; i++ {
					ip[i] = val[4+i] ^ resp[4+i]
				}
				return net.IPv4(ip[0], ip[1], ip[2], ip[3]).String(), nil
			}
		case stunAttrMappedAddress:
			if len(val) >= 8 && val[1] == stunFamilyIPv4 {
				return net.IPv4(val[4], val[5], val[6], val[7]).String(), nil
			}
		}
		// Atributos vienen alineados a 4 bytes.
		padded := (attrLen + 3) &^ 3
		if 4+padded > len(attrs) {
			break
		}
		attrs = attrs[4+padded:]
	}
	return "", fmt.Errorf("sin XOR-MAPPED-ADDRESS v4 en la respuesta")
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---------- fallback HTTP ----------

func httpQueryIP(url string) (string, error) {
	cli := &http.Client{Timeout: egressHTTPTimeo}
	resp, err := cli.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("http %d", resp.StatusCode)
	}
	buf := make([]byte, 64)
	n, _ := io.ReadFull(resp.Body, buf)
	s := strings.TrimSpace(string(buf[:n]))
	// Validar que es IPv4.
	if ip := net.ParseIP(s); ip != nil && ip.To4() != nil {
		return ip.To4().String(), nil
	}
	return "", fmt.Errorf("respuesta no es IPv4: %q", s)
}

// lookupEgress intenta STUN y luego HTTP; devuelve el primero que responda.
// La fuente queda registrada para que la UI diga de dónde salió.
func lookupEgress() (ip, source string, err error) {
	errs := []string{}
	for _, srv := range stunServers {
		if ip, e := stunQuery(srv); e == nil && ip != "" {
			return ip, "stun:" + srv, nil
		} else if e != nil {
			errs = append(errs, "stun "+srv+": "+e.Error())
		}
	}
	for _, u := range httpFallbacks {
		if ip, e := httpQueryIP(u); e == nil && ip != "" {
			return ip, "http:" + u, nil
		} else if e != nil {
			errs = append(errs, "http "+u+": "+e.Error())
		}
	}
	return "", "", fmt.Errorf("todas las fuentes fallaron: %s", strings.Join(errs, "; "))
}

// ---------- estado local y de peers ----------

var localEgress = struct {
	mu  sync.RWMutex
	val Egress
}{}

func setLocalEgress(e Egress) {
	localEgress.mu.Lock()
	localEgress.val = e
	localEgress.mu.Unlock()
}

func getLocalEgress() Egress {
	localEgress.mu.RLock()
	defer localEgress.mu.RUnlock()
	return localEgress.val
}

var peerEgress = struct {
	mu sync.RWMutex
	m  map[string]Egress
}{m: map[string]Egress{}}

// setPeerEgress acepta datos de segunda mano: lleva CollectedAt, así que se
// queda con la copia más nueva. Mismo contrato que setPeerSpecs.
func setPeerEgress(node string, e *Egress) {
	if e == nil || e.CollectedAt == 0 {
		return
	}
	peerEgress.mu.Lock()
	defer peerEgress.mu.Unlock()
	if cur, ok := peerEgress.m[node]; ok && cur.CollectedAt >= e.CollectedAt {
		return
	}
	peerEgress.m[node] = *e
}

func getPeerEgress(node string) *Egress {
	peerEgress.mu.RLock()
	defer peerEgress.mu.RUnlock()
	e, ok := peerEgress.m[node]
	if !ok {
		return nil
	}
	return &e
}

// ---------- loop ----------

// egressTrigger permite forzar un chequeo inmediato (p.ej. al detectar un
// cambio de SSID/IP local). Buffer de 1 para no bloquear al emisor.
var egressTrigger = make(chan struct{}, 1)

// pokeEgress pide un chequeo inmediato sin bloquear si el loop está ocupado.
func pokeEgress() {
	select {
	case egressTrigger <- struct{}{}:
	default:
	}
}

// egressLoop corre un chequeo al arranque y cada egressInterval, o antes si
// alguien llama a pokeEgress(). La última IP buena se preserva aunque el
// chequeo nuevo falle — lo que decide alimentar un whitelist es la última
// medida válida, no la última intentada.
func egressLoop() {
	do := func() {
		ip, src, err := lookupEgress()
		now := time.Now().Unix()
		cur := getLocalEgress()
		next := Egress{
			IPv4:        cur.IPv4,
			Source:      cur.Source,
			CheckedAt:   now,
			CollectedAt: now,
		}
		if err != nil {
			next.LastError = err.Error()
		} else {
			next.IPv4 = ip
			next.Source = src
			next.LastError = ""
		}
		setLocalEgress(next)
	}
	do()
	ticker := time.NewTicker(egressInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
		case <-egressTrigger:
		}
		do()
	}
}

// ---------- agregación por región ----------

// EgressByRegion es lo que consume el whitelist externo: IPs únicas por región,
// con el detalle crudo de cada nodo aparte para diagnóstico.
type EgressByRegion struct {
	Regions map[string][]string `json:"regions"` // region -> IPv4s únicas, ordenadas
	Per     map[string]Egress   `json:"per"`     // nodo -> egress (puede estar vacío)
}

// aggregateEgress junta los egress de /api/nodes por región del placement.
// Se calcula al servir, no se almacena — igual que capacityOf.
//
// Excluye nodos offline/isolated y nodos con egress rancio (más viejo que
// egressTTL): un dato así es peor que no tenerlo, porque un whitelist no
// debe apuntar a una IP que ya no se usa.
func aggregateEgress(infos map[string]nodeInfo) EgressByRegion {
	const egressTTL = int64(2 * 60 * 60) // 2h: cubre varias rondas de 15 min
	now := time.Now().Unix()
	out := EgressByRegion{
		Regions: map[string][]string{},
		Per:     map[string]Egress{},
	}
	bucket := map[string]map[string]bool{} // region -> set de IPs
	for name, info := range infos {
		if info.Egress == nil || info.Egress.IPv4 == "" {
			continue
		}
		if now-info.Egress.CheckedAt > egressTTL {
			continue
		}
		if info.State == stateOffline || info.State == stateIsolated {
			continue
		}
		region := ""
		if info.Placement != nil && info.Placement.Region != "" {
			region = info.Placement.Region
		} else {
			region = "unknown"
		}
		if bucket[region] == nil {
			bucket[region] = map[string]bool{}
		}
		bucket[region][info.Egress.IPv4] = true
		out.Per[name] = *info.Egress
	}
	for region, ips := range bucket {
		list := make([]string, 0, len(ips))
		for ip := range ips {
			list = append(list, ip)
		}
		sort.Strings(list)
		out.Regions[region] = list
	}
	return out
}
