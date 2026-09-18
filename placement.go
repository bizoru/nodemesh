package main

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Dónde está cada nodo, y cómo se sabe.
//
// Sustituye al mapa `locations` (nodo -> cadena) que había que replicar en la
// config de LOS TRECE nodos: por eso honey, andromeda, x1-nano y el Air
// llevaban meses saliendo con "?" — se dieron de alta y nadie volvió a tocar
// los otros doce ficheros. Aquí cada nodo declara SOLO lo suyo y el dato viaja
// por gossip como los [Specs], con UpdatedAt para quedarse con la copia nueva.
//
// El `locations` viejo sigue leyéndose como respaldo, así que no hay día de
// corte: un nodo que todavía no traiga Placement se sigue pintando.

// Clases de nodo. Mismo vocabulario que las etiquetas `mcl.node-class` del
// cluster, a propósito: dos idiomas para la misma idea es como se acaba con
// dos verdades.
const (
	ClassFixed  = "fixed"
	ClassMobile = "mobile"
)

// De dónde salió el sitio que se está publicando. Va SIEMPRE junto al sitio:
// una estimación pintada igual que una medida es peor que no tenerla.
const (
	SourceManual   = "manual"   // lo dice la config del nodo
	SourceSSID     = "ssid"     // la red WiFi está en el catálogo
	SourceGateway  = "gateway"  // la MAC del router coincide
	SourcePublicIP = "publicIP" // el prefijo de salida coincide
	SourceSubnet   = "subnet"   // la subred local coincide y es única
	SourcePeer     = "peer"     // se ve a un vecino de la flota que sí sabe dónde está
	SourceHomebase = "homebase" // no se sabe: se enseña la base como referencia
)

const (
	ConfCertain = "certain"
	ConfHigh    = "high"
	ConfNone    = "none"
)

// Placement es lo que el nodo declara de sí mismo en su config.
type Placement struct {
	Region   string `json:"region,omitempty"`   // co | us | eu — igual que mcl.region
	Site     string `json:"site,omitempty"`     // sitio fijo; en los móviles se deja vacío y se estima
	Class    string `json:"class,omitempty"`    // fixed | mobile
	Homebase string `json:"homebase,omitempty"` // solo móviles; informativo, nadie lo consume
}

// PlacementInfo es lo que se publica en /api/nodes: el sitio ya resuelto más
// la procedencia del dato.
type PlacementInfo struct {
	Region     string `json:"region,omitempty"`
	Site       string `json:"site,omitempty"`
	Class      string `json:"class,omitempty"`
	Homebase   string `json:"homebase,omitempty"`
	Source     string `json:"siteSource,omitempty"`
	Confidence string `json:"siteConfidence,omitempty"`
	UpdatedAt  int64  `json:"updatedAt,omitempty"`
}

// Estimado dice si el sitio es una deducción y no un dato declarado. Lo usa
// quien pinta, para marcarlo (en el informe del R1 sale "Socorro?").
func (p PlacementInfo) Estimado() bool {
	return p.Source != SourceManual && p.Source != "" && p.Source != SourceHomebase
}

// Site es una sede vista como huella, no como nombre: el conjunto de cosas
// observables desde ahí. Lo escrito a mano en la config y lo aprendido por los
// nodos que sí saben dónde están se mezclan en la misma tabla.
type Site struct {
	Region   string   `json:"region,omitempty"`
	SSIDs    []string `json:"ssids,omitempty"`
	Subnets  []string `json:"subnets,omitempty"`        // CIDR
	Gateways []string `json:"gateways,omitempty"`       // MAC del router
	Prefixes []string `json:"publicPrefixes,omitempty"` // CIDR de la IP de salida
}

// huellaAprendida es una observación que hizo un nodo desde un sitio que
// conocía con certeza. Caduca: las IP públicas residenciales cambian al
// reiniciar el router y Starlink va por CGNAT rotando sola, así que una huella
// que nadie refresca deja de contar — mismo criterio que obsTTLSecs con las
// observaciones de caída.
type huellaAprendida struct {
	Site      string `json:"site"`
	Region    string `json:"region,omitempty"`
	Gateway   string `json:"gateway,omitempty"`
	Prefix    string `json:"prefix,omitempty"`
	Subnet    string `json:"subnet,omitempty"`
	SSID      string `json:"ssid,omitempty"`
	By        string `json:"by,omitempty"` // quién la levantó
	UpdatedAt int64  `json:"updatedAt"`
}

// huellaTTL: lo que vale una huella sin refrescar. Generoso a propósito —los
// nodos fijos la renuevan cada ronda, así que solo caduca la de una sede donde
// ya no queda nadie que sepa dónde está.
const huellaTTL = 30 * 24 * time.Hour

var sitios = struct {
	mu        sync.RWMutex
	catalogo  map[string]Site            // de la config
	aprendido map[string]huellaAprendida // clave: sitio|gateway|prefijo|subred
	fichero   string
}{catalogo: map[string]Site{}, aprendido: map[string]huellaAprendida{}}

// ---------- normalización de SSID ----------

// normalizaSSID deja comparables los nombres de la MISMA red escritos de
// formas distintas. Hoy conviven en la flota "FAMILIA-HERNANDEZ 5G" (lo que
// reporta Windows) y "Familia Hernandez 5G" (lo que reporta macOS): son la
// misma casa. Se quitan mayúsculas, separadores y el sufijo de banda.
//
// Lo que la normalización NO puede adivinar va como SSID aparte en el
// catálogo: "FLIA HERNANDEZ-5G" no se parece a "FAMILIA" por ninguna regla
// sensata, y forzarla para que encajen haría que colisionaran redes distintas.
func normalizaSSID(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	for _, suf := range []string{
		"5g extended", "5ghz extended", "extended",
		"_5ghz", "-5ghz", " 5ghz", "5ghz",
		"_5g", "-5g", " 5g", "5g",
		"_2.4g", "-2.4g", " 2.4g",
	} {
		if strings.HasSuffix(s, suf) {
			s = strings.TrimSpace(strings.TrimSuffix(s, suf))
			break
		}
	}
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Para normalizar MAC se reusa normMAC() de main.go, que ya extrae y rellena
// a dos dígitos los octetos que macOS imprime sin el cero ("8:f6:6:...").

// esGlobal descarta todo lo que no es una IP de internet.
//
// Es imprescindible, y no un adorno: el latido a gcp-east va por MagicDNS
// mientras el tailnet esté sano, así que la IP que ve el colector suele ser la
// **100.x del tailnet**, no la de salida. Aprender eso como huella sería
// catastrófico — la 100.64.0.0/10 la comparten los trece nodos, así que
// "ubicaría" a toda la flota en la misma sede.
func esGlobal(ip net.IP) bool {
	if ip == nil || ip.IsLoopback() || ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
		return false
	}
	// CGNAT: 100.64.0.0/10. Es el rango de Tailscale y también el que usan
	// algunos operadores móviles, y en los dos casos no identifica una sede.
	_, cgnat, _ := net.ParseCIDR("100.64.0.0/10")
	return !cgnat.Contains(ip)
}

// prefijoDe reduce una IP pública a su red, que es lo estable. La IP exacta no
// vale: cambia al reiniciar el router. Se usa /24 en IPv4 y /48 en IPv6, que es
// lo que suele conservar un mismo abonado.
func prefijoDe(ip string) string {
	p := net.ParseIP(strings.TrimSpace(ip))
	if p == nil {
		return ""
	}
	if v4 := p.To4(); v4 != nil {
		return (&net.IPNet{IP: v4.Mask(net.CIDRMask(24, 32)), Mask: net.CIDRMask(24, 32)}).String()
	}
	return (&net.IPNet{IP: p.Mask(net.CIDRMask(48, 128)), Mask: net.CIDRMask(48, 128)}).String()
}

// subredDe reduce la IP local a su /24. A diferencia del prefijo público, aquí
// lo normal es una dirección privada: es justo lo que se quiere comparar.
func subredDe(ip string) string { return prefijoDe(ip) }

// ---------- carga ----------

func cargaSitios(cfg *Config) {
	sitios.mu.Lock()
	defer sitios.mu.Unlock()
	for n, s := range cfg.Sites {
		sitios.catalogo[n] = s
	}
	sitios.fichero = filepath.Join(cfg.DataDir, "sitios-aprendidos.json")
	b, err := os.ReadFile(sitios.fichero)
	if err != nil {
		return
	}
	var m map[string]huellaAprendida
	if json.Unmarshal(b, &m) == nil {
		for k, v := range m {
			sitios.aprendido[k] = v
		}
	}
}

// guardaAprendido persiste lo aprendido. Se escribe por fichero temporal y
// rename: un corte de luz a media escritura dejaría un JSON roto, y este
// fichero lo lee el arranque.
func guardaAprendido() {
	sitios.mu.RLock()
	fichero := sitios.fichero
	m := make(map[string]huellaAprendida, len(sitios.aprendido))
	for k, v := range sitios.aprendido {
		m[k] = v
	}
	sitios.mu.RUnlock()
	if fichero == "" {
		return
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return
	}
	tmp := fichero + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		os.Rename(tmp, fichero)
	}
}

// ---------- aprendizaje ----------

func claveHuella(h huellaAprendida) string {
	return strings.Join([]string{h.Site, h.Gateway, h.Prefix, h.Subnet}, "|")
}

// aprende guarda una huella nueva o refresca la que ya estaba. Solo la llama
// quien resolvió su sitio CON CERTEZA: aprender de una estimación es como se
// construye un mapa que se confirma a sí mismo.
func aprende(h huellaAprendida) {
	if h.Site == "" || (h.Gateway == "" && h.Prefix == "" && h.Subnet == "") {
		return
	}
	if h.UpdatedAt == 0 {
		h.UpdatedAt = time.Now().Unix()
	}
	k := claveHuella(h)
	sitios.mu.Lock()
	cur, ok := sitios.aprendido[k]
	if ok && cur.UpdatedAt >= h.UpdatedAt {
		sitios.mu.Unlock()
		return
	}
	sitios.aprendido[k] = h
	sitios.mu.Unlock()
	guardaAprendido()
}

// huellasVivas devuelve lo aprendido que todavía vale.
func huellasVivas() []huellaAprendida {
	corte := time.Now().Add(-huellaTTL).Unix()
	sitios.mu.RLock()
	defer sitios.mu.RUnlock()
	out := make([]huellaAprendida, 0, len(sitios.aprendido))
	for _, h := range sitios.aprendido {
		if h.UpdatedAt >= corte {
			out = append(out, h)
		}
	}
	sort.Slice(out, func(i, j int) bool { return claveHuella(out[i]) < claveHuella(out[j]) })
	return out
}

// ---------- resolución ----------

// señales es lo que este nodo ve de sí mismo ahora mismo.
type señales struct {
	SSID     string
	LocalIP  string
	Gateway  string // MAC del router
	PublicIP string
}

// unico devuelve el sitio si —y solo si— exactamente uno lo reclama. Una señal
// que reclaman dos sedes no resuelve nada y hay que decirlo: la subred
// 192.168.1.0/24 es la de Familia Hernandez en Bucaramanga Y la de Starlink en
// Socorro, así que por sí sola no ubica a nadie.
func unico(cands map[string]bool) string {
	if len(cands) != 1 {
		return ""
	}
	for s := range cands {
		return s
	}
	return ""
}

func porSSID(ssid string) string {
	if ssid == "" {
		return ""
	}
	n := normalizaSSID(ssid)
	if n == "" {
		return ""
	}
	cands := map[string]bool{}
	sitios.mu.RLock()
	for nombre, s := range sitios.catalogo {
		for _, x := range s.SSIDs {
			if normalizaSSID(x) == n {
				cands[nombre] = true
			}
		}
	}
	sitios.mu.RUnlock()
	for _, h := range huellasVivas() {
		if h.SSID != "" && normalizaSSID(h.SSID) == n {
			cands[h.Site] = true
		}
	}
	return unico(cands)
}

func porGateway(mac string) string {
	mac = normMAC(mac)
	if mac == "" {
		return ""
	}
	cands := map[string]bool{}
	sitios.mu.RLock()
	for nombre, s := range sitios.catalogo {
		for _, x := range s.Gateways {
			if normMAC(x) == mac {
				cands[nombre] = true
			}
		}
	}
	sitios.mu.RUnlock()
	for _, h := range huellasVivas() {
		if normMAC(h.Gateway) == mac {
			cands[h.Site] = true
		}
	}
	return unico(cands)
}

func enCIDR(cidr, ip string) bool {
	_, red, err := net.ParseCIDR(strings.TrimSpace(cidr))
	if err != nil {
		return false
	}
	p := net.ParseIP(strings.TrimSpace(ip))
	return p != nil && red.Contains(p)
}

func porPrefijo(ip string) string {
	if ip == "" {
		return ""
	}
	pref := prefijoDe(ip)
	cands := map[string]bool{}
	sitios.mu.RLock()
	for nombre, s := range sitios.catalogo {
		for _, x := range s.Prefixes {
			if enCIDR(x, ip) {
				cands[nombre] = true
			}
		}
	}
	sitios.mu.RUnlock()
	for _, h := range huellasVivas() {
		if h.Prefix != "" && h.Prefix == pref {
			cands[h.Site] = true
		}
	}
	return unico(cands)
}

func porSubred(ip string) string {
	if ip == "" {
		return ""
	}
	sub := subredDe(ip)
	cands := map[string]bool{}
	sitios.mu.RLock()
	for nombre, s := range sitios.catalogo {
		for _, x := range s.Subnets {
			if enCIDR(x, ip) {
				cands[nombre] = true
			}
		}
	}
	sitios.mu.RUnlock()
	for _, h := range huellasVivas() {
		if h.Subnet != "" && h.Subnet == sub {
			cands[h.Site] = true
		}
	}
	return unico(cands)
}

// regionDe busca la región de un sitio, primero en el catálogo escrito y luego
// en lo aprendido.
func regionDe(sitio string) string {
	sitios.mu.RLock()
	s, ok := sitios.catalogo[sitio]
	sitios.mu.RUnlock()
	if ok && s.Region != "" {
		return s.Region
	}
	for _, h := range huellasVivas() {
		if h.Site == sitio && h.Region != "" {
			return h.Region
		}
	}
	return ""
}

// resuelve decide el sitio de ESTE nodo y, si lo sabe con certeza, deja
// constancia de lo que se ve desde aquí para que el siguiente que llegue sin
// SSID pueda ubicarse. La flota levanta su propio mapa.
//
// El orden importa: primero lo que el nodo declara, después lo medido de más
// a menos fiable, y el homebase al final — que no es una estimación, es el
// valor por defecto cuando no se sabe.
func resuelve(decl Placement, s señales) PlacementInfo {
	out := PlacementInfo{
		Region:    decl.Region,
		Class:     decl.Class,
		Homebase:  decl.Homebase,
		UpdatedAt: time.Now().Unix(),
	}
	if out.Class == "" {
		out.Class = ClassFixed
	}

	// Un nodo fijo dice dónde está y no hay nada que estimar. Que se mueva una
	// máquina declarada fija es un cambio de config, no algo que deba adivinar
	// el que la está mirando.
	if decl.Site != "" && out.Class == ClassFixed {
		out.Site, out.Source, out.Confidence = decl.Site, SourceManual, ConfCertain
		if out.Region == "" {
			out.Region = regionDe(decl.Site)
		}
		aprendeDesde(out.Site, out.Region, s)
		return out
	}

	tipo := ""
	sitio := ""
	conf := ""
	switch {
	case porSSID(s.SSID) != "":
		sitio, tipo, conf = porSSID(s.SSID), SourceSSID, ConfCertain
	case porGateway(s.Gateway) != "":
		sitio, tipo, conf = porGateway(s.Gateway), SourceGateway, ConfCertain
	case primero(porVecino(s.LocalIP)) != "":
		// Un ping que llega es una prueba física: esas dos máquinas comparten
		// cable o router. Por eso va por delante del prefijo público.
		sitio, tipo, conf = primero(porVecino(s.LocalIP)), SourcePeer, ConfCertain
	case porPrefijo(s.PublicIP) != "":
		sitio, tipo, conf = porPrefijo(s.PublicIP), SourcePublicIP, ConfHigh
	case porSubred(s.LocalIP) != "":
		sitio, tipo, conf = porSubred(s.LocalIP), SourceSubnet, ConfHigh
	}

	if sitio == "" {
		// Ni una señal encaja. El homebase se enseña como referencia, nunca
		// como respuesta: decir "está en Bucaramanga" porque ahí vive es
		// exactamente la mentira que este campo existe para no contar.
		out.Site, out.Source, out.Confidence = decl.Site, SourceHomebase, ConfNone
		if out.Site == "" {
			out.Site = decl.Homebase
		}
		if out.Region == "" && out.Site != "" {
			out.Region = regionDe(out.Site)
		}
		return out
	}

	out.Site, out.Source, out.Confidence = sitio, tipo, conf
	if out.Region == "" {
		out.Region = regionDe(sitio)
	}
	// Solo se aprende de lo que se sabe con certeza. Aprender de una
	// estimación encadena deducciones sobre deducciones y acaba en un mapa que
	// se confirma a sí mismo.
	if conf == ConfCertain {
		aprendeDesde(out.Site, out.Region, s)
	}
	return out
}

func aprendeDesde(sitio, region string, s señales) {
	if sitio == "" {
		return
	}
	ahora := time.Now().Unix()
	if mac := normMAC(s.Gateway); mac != "" {
		aprende(huellaAprendida{Site: sitio, Region: region, Gateway: mac, SSID: s.SSID,
			By: nodoLocal, UpdatedAt: ahora})
	}
	if p := prefijoDe(s.PublicIP); p != "" && esGlobal(net.ParseIP(s.PublicIP)) {
		aprende(huellaAprendida{Site: sitio, Region: region, Prefix: p, SSID: s.SSID,
			By: nodoLocal, UpdatedAt: ahora})
	}
	if sub := subredDe(s.LocalIP); sub != "" {
		aprende(huellaAprendida{Site: sitio, Region: region, Subnet: sub, SSID: s.SSID,
			By: nodoLocal, UpdatedAt: ahora})
	}
}

// nodoLocal lo fija main() para poder firmar las huellas sin arrastrar la
// config hasta aquí.
var nodoLocal string

// gatewayActual cachea la MAC del router: gatewayMAC() es un exec en macOS y
// Windows, y /api/nodes lo piden el gossip y la UI cada 30 s. El router no
// cambia entre consultas; cambia cuando alguien se lleva el portátil, y para
// eso llega de sobra con un minuto.
var gwCache = struct {
	mu  sync.Mutex
	val string
	at  time.Time
}{}

func gatewayActual() string {
	gwCache.mu.Lock()
	defer gwCache.mu.Unlock()
	if !gwCache.at.IsZero() && time.Since(gwCache.at) < time.Minute {
		return gwCache.val
	}
	gwCache.val, gwCache.at = gatewayMAC(), time.Now()
	return gwCache.val
}

// La IP pública de salida NO se le pregunta a ningún servicio de fuera: el
// latido a gcp-east ya cruza internet plano, así que el colector ve la IP de
// origen gratis y la devuelve en su respuesta. Ver heartbeat.go.
var ipPublica = struct {
	mu sync.RWMutex
	v  string
	at time.Time
}{}

func fijaIPPublica(ip string) {
	if !esGlobal(net.ParseIP(strings.TrimSpace(ip))) {
		return
	}
	ipPublica.mu.Lock()
	ipPublica.v, ipPublica.at = strings.TrimSpace(ip), time.Now()
	ipPublica.mu.Unlock()
}

// ipPublicaActual caduca sola: un nodo que deja de mandar latido —porque se
// quedó sin internet plano o cambió de red— no debe seguir ubicándose por la
// IP con la que salía hace horas.
func ipPublicaActual() string {
	ipPublica.mu.RLock()
	defer ipPublica.mu.RUnlock()
	if time.Since(ipPublica.at) > 30*time.Minute {
		return ""
	}
	return ipPublica.v
}

// ---------- lo propio y lo de los pares ----------

var propio = struct {
	mu   sync.RWMutex
	decl Placement
	info PlacementInfo
}{}

func fijaDeclaracion(p *Placement) {
	if p == nil {
		return
	}
	propio.mu.Lock()
	propio.decl = *p
	propio.mu.Unlock()
}

func declaracion() Placement {
	propio.mu.RLock()
	defer propio.mu.RUnlock()
	return propio.decl
}

func refrescaPropio(s señales) PlacementInfo {
	info := resuelve(declaracion(), s)
	propio.mu.Lock()
	propio.info = info
	propio.mu.Unlock()
	return info
}

func placementPropio() *PlacementInfo {
	propio.mu.RLock()
	defer propio.mu.RUnlock()
	if propio.info.Site == "" && propio.info.Class == "" {
		return nil
	}
	i := propio.info
	return &i
}

var peerPlacements = struct {
	mu sync.RWMutex
	m  map[string]PlacementInfo
}{m: map[string]PlacementInfo{}}

// setPeerPlacement acepta de segunda mano, como los specs y por el mismo
// motivo: lleva UpdatedAt, así que la copia rancia se descarta sola y un nodo
// conoce la flota entera aunque no llegue directamente a todos.
func setPeerPlacement(node string, p *PlacementInfo) {
	if p == nil || p.UpdatedAt == 0 {
		return
	}
	peerPlacements.mu.Lock()
	defer peerPlacements.mu.Unlock()
	if cur, ok := peerPlacements.m[node]; ok && cur.UpdatedAt >= p.UpdatedAt {
		return
	}
	peerPlacements.m[node] = *p
}

func getPeerPlacement(node string) *PlacementInfo {
	peerPlacements.mu.RLock()
	defer peerPlacements.mu.RUnlock()
	p, ok := peerPlacements.m[node]
	if !ok {
		return nil
	}
	return &p
}

// etiquetaSitio es el texto que se pinta, y el que rellena el campo `location`
// de siempre para no romper a quien ya lo consume (SimacotaViewer en el R1 y
// meshflow leen `location`, no este objeto).
//
//	fijo             -> "Bucaramanga"
//	móvil ubicado    -> "Móvil · Socorro"
//	móvil estimado   -> "Móvil · Socorro?"
//	móvil sin ubicar -> "Móvil" (y el homebase entre paréntesis si lo hay)
func etiquetaSitio(p PlacementInfo) string {
	if p.Class != ClassMobile {
		return p.Site
	}
	if p.Source == SourceHomebase || p.Site == "" {
		if p.Homebase != "" {
			return "Móvil (" + p.Homebase + ")"
		}
		return "Móvil"
	}
	sitio := p.Site
	if p.Estimado() && p.Confidence != ConfCertain {
		sitio += "?"
	}
	return "Móvil · " + sitio
}

// ---------- API ----------

// handlePlacement: GET devuelve lo resuelto; POST cambia la declaración en
// caliente y la persiste.
//
// Existe para que mover un nodo de clase o de sede sea UN curl contra ESA
// máquina, sin editar ficheros ni reiniciar el servicio — y el cambio se
// propaga solo por gossip en un ciclo. Es tailnet-only, como /api/restart, que
// ya escribe bastante más que esto (mata el proceso).
func handlePlacement(w http.ResponseWriter, r *http.Request, cfg *Config, cfgPath string) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodGet {
		json.NewEncoder(w).Encode(map[string]any{
			"declared": declaracion(),
			"resolved": placementPropio(),
			"learned":  huellasVivas(),
		})
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Se parte de lo declarado y se pisan solo los campos que vengan: un POST
	// con {"class":"mobile"} no puede borrar la región sin querer.
	nueva := declaracion()
	var patch map[string]*string
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		http.Error(w, "json inválido", http.StatusBadRequest)
		return
	}
	aplica := func(campo string, dst *string) {
		if v, ok := patch[campo]; ok && v != nil {
			*dst = strings.TrimSpace(*v)
		}
	}
	aplica("region", &nueva.Region)
	aplica("site", &nueva.Site)
	aplica("class", &nueva.Class)
	aplica("homebase", &nueva.Homebase)
	if nueva.Class != "" && nueva.Class != ClassFixed && nueva.Class != ClassMobile {
		http.Error(w, "class: "+ClassFixed+" o "+ClassMobile, http.StatusBadRequest)
		return
	}
	fijaDeclaracion(&nueva)
	cfg.Placement = &nueva
	if err := guardaPlacementEnConfig(cfgPath, nueva); err != nil {
		// El cambio ya está aplicado en memoria; lo que falla es que sobreviva
		// a un reinicio. Se dice, en vez de devolver un ok que mentiría.
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]any{
			"applied": nueva, "persisted": false, "error": err.Error(),
		})
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"applied": nueva, "persisted": true})
}

// guardaPlacementEnConfig reescribe SOLO la clave "placement" del fichero,
// conservando el resto tal cual. Se parsea a map y no a Config a propósito: la
// config de cada nodo tiene campos que este binario podría no conocer
// —tokens, ajustes a mano— y volcarla desde el struct los borraría.
func guardaPlacementEnConfig(path string, p Placement) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	pb, err := json.Marshal(p)
	if err != nil {
		return err
	}
	m["placement"] = pb
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// handleSites publica el catálogo y lo aprendido, que es lo que el gossip se
// lleva de aquí.
func handleSites(w http.ResponseWriter, r *http.Request) {
	sitios.mu.RLock()
	cat := make(map[string]Site, len(sitios.catalogo))
	for k, v := range sitios.catalogo {
		cat[k] = v
	}
	sitios.mu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"catalog": cat,
		"learned": huellasVivas(),
	})
}

// mezclaSitios incorpora lo que un peer sabe. El catálogo escrito a mano no se
// pisa: lo aprendido es una capa aparte que solo suma.
func mezclaSitios(cat map[string]Site, aprendidas []huellaAprendida) {
	sitios.mu.Lock()
	for n, s := range cat {
		if _, ok := sitios.catalogo[n]; !ok {
			sitios.catalogo[n] = s
		}
	}
	sitios.mu.Unlock()
	for _, h := range aprendidas {
		aprende(h)
	}
}

// ipPublicaLoop pregunta cada tanto por dónde sale este nodo.
//
// Va contra el /ip del colector propio, no contra un servicio de terceros: es
// el mismo aparato que ya recibe los latidos, así que no se añade ninguna
// dependencia externa ni se le cuenta a nadie de fuera que esta flota existe.
//
// Cadencia baja a propósito: la IP de salida cambia cuando alguien se lleva el
// portátil o se reinicia el router, no cada minuto, y el nodo puede estar con
// batería.
func ipPublicaLoop(cfg *Config) {
	if cfg.PublicIPURL == "" {
		return
	}
	cliente := &http.Client{Timeout: 15 * time.Second}
	for {
		if resp, err := cliente.Get(cfg.PublicIPURL); err == nil {
			cuerpo := make([]byte, 64)
			n, _ := resp.Body.Read(cuerpo)
			resp.Body.Close()
			for _, campo := range strings.Fields(string(cuerpo[:n])) {
				fijaIPPublica(campo)
			}
		}
		time.Sleep(10 * time.Minute)
	}
}

// ---------- el vecino de LAN ----------

// vecino es un nodo de la flota del que se sabe su IP de LAN y dónde dice
// estar.
type vecino struct {
	Nombre  string
	LocalIP string
	Sitio   string
	Fuente  string
}

var vecindario = struct {
	mu sync.RWMutex
	v  []vecino
}{}

func fijaVecinos(v []vecino) {
	vecindario.mu.Lock()
	vecindario.v = v
	vecindario.mu.Unlock()
}

// pingCache evita repetir el ping en cada vuelta del colector. En el R1 esto
// corre con batería, y aquí la batería manda: un vecino no cambia de LAN entre
// un minuto y el siguiente.
var pingCache = struct {
	mu sync.Mutex
	m  map[string]struct {
		ok bool
		at time.Time
	}
}{m: map[string]struct {
	ok bool
	at time.Time
}{}}

const pingTTL = 5 * time.Minute

func alcanzaLAN(ip string) bool {
	pingCache.mu.Lock()
	if e, ok := pingCache.m[ip]; ok && time.Since(e.at) < pingTTL {
		pingCache.mu.Unlock()
		return e.ok
	}
	pingCache.mu.Unlock()
	ok := pingHost(ip)
	pingCache.mu.Lock()
	pingCache.m[ip] = struct {
		ok bool
		at time.Time
	}{ok, time.Now()}
	pingCache.mu.Unlock()
	return ok
}

// primero se queda con el sitio y tira la cuenta de pings, que solo interesa
// para no barrer la red.
func primero(sitio string, _ int) string { return sitio }

// porVecino ubica un nodo por la compañía que tiene en su LAN.
//
// Es la señal más fuerte que hay y la única que no depende de que el sistema
// operativo deje leer nada: si alcanzo por LAN a una máquina que sabe con
// certeza dónde está, estoy donde ella. Es lo que ubica al R1, al que Android
// le esconde el SSID y la tabla ARP a la vez.
//
// Estar en la misma /24 NO basta y por eso se hace ping: 192.168.1.0/24 es la
// LAN de Bucaramanga Y la de Starlink en Socorro, así que la subred sola deja
// dos candidatos. El ping los separa, porque son dos redes físicas distintas
// sin ruta entre ellas.
//
// Solo cuentan los vecinos que saben dónde están de PRIMERA mano. Aceptar a
// uno que a su vez se ubicó por un vecino encadenaría deducciones, y dos nodos
// podrían acabar confirmándose el sitio el uno al otro sin que ninguno lo
// supiera — el mismo círculo que crossCheckedState ya evita con las caídas.
func porVecino(miIP string) (string, int) {
	if miIP == "" {
		return "", 0
	}
	miSub := subredDe(miIP)
	if miSub == "" {
		return "", 0
	}
	vecindario.mu.RLock()
	lista := append([]vecino(nil), vecindario.v...)
	vecindario.mu.RUnlock()

	cands := map[string]bool{}
	pings := 0
	for _, v := range lista {
		if v.Sitio == "" || v.LocalIP == "" || v.LocalIP == miIP {
			continue
		}
		if v.Fuente != SourceManual && v.Fuente != SourceSSID && v.Fuente != SourceGateway {
			continue
		}
		if subredDe(v.LocalIP) != miSub {
			continue
		}
		// Tope de pings por vuelta: en un aparato con batería esto no puede
		// convertirse en un barrido de la red.
		if pings >= 3 {
			break
		}
		pings++
		if alcanzaLAN(v.LocalIP) {
			cands[v.Sitio] = true
		}
	}
	return unico(cands), pings
}
