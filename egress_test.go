package main

import (
	"encoding/binary"
	"testing"
	"time"
)

// construyeRespuestaSTUN arma una respuesta válida de binding success con
// XOR-MAPPED-ADDRESS. txIDWithCookie son los 16 bytes de cookie+txID del
// request original (bytes 4..20 del header). ip es la IPv4 "antes" del XOR
// (lo que debería leer el cliente).
func construyeRespuestaSTUN(txIDWithCookie []byte, ip [4]byte) []byte {
	// Attr body: family(1)=01, pad(1)=0, port XOR cookie-hi(2), ip XOR cookie-full(4) = 8 bytes
	attr := make([]byte, 4+8)
	binary.BigEndian.PutUint16(attr[0:2], stunAttrXORMappedAddress)
	binary.BigEndian.PutUint16(attr[2:4], 8) // length
	attr[4] = 0
	attr[5] = stunFamilyIPv4
	// Puerto arbitrario 1234 XOR con cookie-hi (0x2112).
	binary.BigEndian.PutUint16(attr[6:8], 1234^0x2112)
	// Los 4 bytes de IP XOR con los 4 bytes del cookie (0x2112A442).
	// txIDWithCookie[0:4] es el cookie puro.
	for i := 0; i < 4; i++ {
		attr[8+i] = ip[i] ^ txIDWithCookie[i]
	}
	msg := make([]byte, 20+len(attr))
	binary.BigEndian.PutUint16(msg[0:2], stunBindingSuccess)
	binary.BigEndian.PutUint16(msg[2:4], uint16(len(attr)))
	copy(msg[4:20], txIDWithCookie)
	copy(msg[20:], attr)
	return msg
}

func TestParseSTUNResponseDevuelveLaIPv4XOREada(t *testing.T) {
	tx := make([]byte, 16)
	binary.BigEndian.PutUint32(tx[0:4], stunMagicCookie)
	for i := 4; i < 16; i++ {
		tx[i] = byte(i)
	}
	resp := construyeRespuestaSTUN(tx, [4]byte{203, 0, 113, 42})
	ip, err := parseSTUNResponse(resp, tx)
	if err != nil {
		t.Fatalf("parse falló: %v", err)
	}
	if ip != "203.0.113.42" {
		t.Fatalf("ip esperada 203.0.113.42, salió %q", ip)
	}
}

func TestParseSTUNResponseRechazaTxIDDistinto(t *testing.T) {
	// Simular que el request usó txID A pero la respuesta trae txID B — no
	// corresponde, no la aceptamos: podría ser un paquete de otra sesión que
	// se coló en el socket.
	txA := make([]byte, 16)
	binary.BigEndian.PutUint32(txA[0:4], stunMagicCookie)
	for i := 4; i < 16; i++ {
		txA[i] = byte(i)
	}
	txB := make([]byte, 16)
	binary.BigEndian.PutUint32(txB[0:4], stunMagicCookie)
	for i := 4; i < 16; i++ {
		txB[i] = byte(i + 100) // distintos
	}
	resp := construyeRespuestaSTUN(txB, [4]byte{203, 0, 113, 42})
	if _, err := parseSTUNResponse(resp, txA); err == nil {
		t.Fatal("una respuesta con txID distinto debe rechazarse")
	}
}

func TestParseSTUNResponseRechazaMensajeCorto(t *testing.T) {
	tx := make([]byte, 16)
	if _, err := parseSTUNResponse([]byte{0, 0, 0}, tx); err == nil {
		t.Fatal("respuesta más corta que el header debería fallar")
	}
}

// infosBase prepara un nodeInfo mínimo con placement y estado online. Lo que
// cambia test a test es el egress.
func infoConEgress(region, ip string, checkedAtOffset time.Duration) nodeInfo {
	n := nodeInfo{
		State: stateOnline,
		Placement: &PlacementInfo{
			Region: region,
			Site:   region,
		},
	}
	if ip != "" {
		n.Egress = &Egress{
			IPv4:        ip,
			Source:      "stun:x",
			CheckedAt:   time.Now().Add(checkedAtOffset).Unix(),
			CollectedAt: time.Now().Add(checkedAtOffset).Unix(),
		}
	}
	return n
}

func TestAggregateEgressDeduplicaPorRegion(t *testing.T) {
	// Dos nodos en 'co' detrás del MISMO ISP (misma IP pública) + uno en 'us'.
	// El whitelist no quiere la IP repetida, pero sí quiere que la de 'us'
	// aparezca aparte.
	infos := map[string]nodeInfo{
		"mini":     infoConEgress("co", "190.1.2.3", 0),
		"x1-nano":  infoConEgress("co", "190.1.2.3", 0),
		"entry":    infoConEgress("us", "138.68.1.1", 0),
	}
	out := aggregateEgress(infos)
	if len(out.Regions["co"]) != 1 || out.Regions["co"][0] != "190.1.2.3" {
		t.Fatalf("co debería tener una sola IP (190.1.2.3), salió: %v", out.Regions["co"])
	}
	if len(out.Regions["us"]) != 1 || out.Regions["us"][0] != "138.68.1.1" {
		t.Fatalf("us debería tener 138.68.1.1, salió: %v", out.Regions["us"])
	}
}

func TestAggregateEgressExcluyeDatosRancios(t *testing.T) {
	// Un nodo que reportó hace 3 h no cuenta — a un whitelist externo le
	// darías una IP que ya no se usa.
	infos := map[string]nodeInfo{
		"mini":  infoConEgress("co", "190.1.2.3", -3*time.Hour),
		"entry": infoConEgress("us", "138.68.1.1", 0),
	}
	out := aggregateEgress(infos)
	if _, ok := out.Regions["co"]; ok {
		t.Fatalf("co no debería aparecer: su único nodo tiene datos rancios: %v", out.Regions)
	}
	if _, ok := out.Per["mini"]; ok {
		t.Fatal("un nodo rancio tampoco debería estar en Per")
	}
}

func TestAggregateEgressIgnoraNodosOfflineOIsolated(t *testing.T) {
	infos := map[string]nodeInfo{
		"muerto": infoConEgress("co", "190.9.9.9", 0),
		"vivo":   infoConEgress("co", "190.1.2.3", 0),
	}
	n := infos["muerto"]
	n.State = stateOffline
	infos["muerto"] = n
	out := aggregateEgress(infos)
	ips := out.Regions["co"]
	if len(ips) != 1 || ips[0] != "190.1.2.3" {
		t.Fatalf("co debería listar solo la del nodo vivo: %v", ips)
	}
}

func TestAggregateEgressSinPlacementCaeEnUnknown(t *testing.T) {
	infos := map[string]nodeInfo{
		"nuevo": {
			State: stateOnline,
			Egress: &Egress{
				IPv4:      "203.0.113.5",
				CheckedAt: time.Now().Unix(),
			},
		},
	}
	out := aggregateEgress(infos)
	if len(out.Regions["unknown"]) != 1 || out.Regions["unknown"][0] != "203.0.113.5" {
		t.Fatalf("un nodo sin placement debería colgar de 'unknown': %v", out.Regions)
	}
}

func TestAggregateEgressOrdenaIPsParaSalidaEstable(t *testing.T) {
	infos := map[string]nodeInfo{
		"a": infoConEgress("co", "190.3.3.3", 0),
		"b": infoConEgress("co", "190.1.1.1", 0),
		"c": infoConEgress("co", "190.2.2.2", 0),
	}
	out := aggregateEgress(infos)
	ips := out.Regions["co"]
	if len(ips) != 3 || ips[0] != "190.1.1.1" || ips[1] != "190.2.2.2" || ips[2] != "190.3.3.3" {
		t.Fatalf("IPs deberían salir ordenadas lexicográficamente, salió: %v", ips)
	}
}
