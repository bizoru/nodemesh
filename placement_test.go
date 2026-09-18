package main

import "testing"

// reinicia deja las tablas como si el proceso acabara de arrancar. Sin esto un
// test contamina al siguiente: el catálogo y lo aprendido son globales.
func reinicia(cat map[string]Site) {
	sitios.mu.Lock()
	sitios.catalogo = map[string]Site{}
	sitios.aprendido = map[string]huellaAprendida{}
	sitios.fichero = "" // no se persiste nada durante los tests
	for k, v := range cat {
		sitios.catalogo[k] = v
	}
	sitios.mu.Unlock()
}

func TestNormalizaSSIDJuntaLaMismaRedEscritaDistinto(t *testing.T) {
	// Los dos conviven HOY en la flota: Windows reporta el primero y macOS el
	// segundo, y son la misma casa de Bucaramanga.
	a := normalizaSSID("FAMILIA-HERNANDEZ 5G")
	b := normalizaSSID("Familia Hernandez 5G")
	if a != b {
		t.Fatalf("deberían ser la misma red: %q vs %q", a, b)
	}
	if normalizaSSID("STEVEN.SIERRA_5G") != normalizaSSID("STEVEN.SIERRA") {
		t.Fatal("la banda no debería cambiar la red")
	}
	// Lo que NO puede pasar: que la normalización junte redes distintas.
	if normalizaSSID("Sierra Networks") == normalizaSSID("STEVEN.SIERRA") {
		t.Fatal("dos redes distintas de Socorro no pueden colapsar en una")
	}
}

func TestUnaSubredEnDosSedesNoUbicaANadie(t *testing.T) {
	// El caso real: 192.168.1.0/24 es la LAN de Familia Hernandez en
	// Bucaramanga Y la de Starlink en Socorro. Que los números altos caigan
	// del lado de Socorro es casualidad, no regla.
	reinicia(map[string]Site{
		"Bucaramanga": {Region: "co", Subnets: []string{"192.168.1.0/24"}},
		"Socorro":     {Region: "co", Subnets: []string{"192.168.1.0/24", "192.168.101.0/24"}},
	})
	if s := porSubred("192.168.1.163"); s != "" {
		t.Fatalf("una subred compartida no puede resolver, devolvió %q", s)
	}
	if s := porSubred("192.168.101.93"); s != "Socorro" {
		t.Fatalf("la subred que solo reclama una sede sí resuelve: %q", s)
	}
}

func TestElGatewayParteEnDosLasDosRedesIguales(t *testing.T) {
	// Mismo 192.168.1.1 en las dos sedes, routers distintos: es la señal que
	// desempata.
	reinicia(map[string]Site{
		"Bucaramanga": {Region: "co", Gateways: []string{"08:f6:06:99:7c:5c"}},
		"Socorro":     {Region: "co", Gateways: []string{"74:24:9f:71:da:85"}},
	})
	if s := porGateway("74:24:9f:71:da:85"); s != "Socorro" {
		t.Fatalf("Socorro por MAC del router, devolvió %q", s)
	}
	// macOS imprime los octetos sin el cero de relleno.
	if s := porGateway("8:f6:6:99:7c:5c"); s != "Bucaramanga" {
		t.Fatalf("la MAC sin relleno es la misma MAC, devolvió %q", s)
	}
}

func TestElSitioDeclaradoGanaEnUnNodoFijo(t *testing.T) {
	reinicia(map[string]Site{"Socorro": {Region: "co", SSIDs: []string{"STEVEN.SIERRA"}}})
	got := resuelve(
		Placement{Site: "Bucaramanga", Class: ClassFixed, Region: "co"},
		señales{SSID: "STEVEN.SIERRA"},
	)
	// Que una máquina declarada fija aparezca en otra red es un cambio de
	// config, no algo que deba adivinar quien la mira.
	if got.Site != "Bucaramanga" || got.Source != SourceManual {
		t.Fatalf("un fijo no se reubica solo: %+v", got)
	}
}

func TestElMovilSeUbicaPorLaSeñalMasFiableQueTenga(t *testing.T) {
	reinicia(map[string]Site{
		"Bucaramanga": {Region: "co", SSIDs: []string{"Familia Hernandez"}},
		"Socorro": {Region: "co", Gateways: []string{"74:24:9f:71:da:85"},
			Prefixes: []string{"153.67.115.0/24"}},
	})
	movil := Placement{Class: ClassMobile, Homebase: "Bucaramanga"}

	// Con SSID manda el SSID. La IP pública es la de Bucaramanga de verdad
	// (la que comparten el mini y el R1): darle aquí la de Socorro sería
	// inventar un nodo imposible, y además enseñaría a la tabla que ese
	// prefijo es de las dos sedes, que es justo lo que la deja sin resolver.
	got := resuelve(movil, señales{SSID: "FAMILIA-HERNANDEZ 5G", PublicIP: "181.55.23.9"})
	if got.Site != "Bucaramanga" || got.Source != SourceSSID {
		t.Fatalf("el SSID manda: %+v", got)
	}

	// Sin SSID —macbook-pro, que es justo el que viaja— resuelve el router.
	got = resuelve(movil, señales{Gateway: "74:24:9f:71:da:85", PublicIP: "153.67.115.231"})
	if got.Site != "Socorro" || got.Source != SourceGateway || got.Confidence != ConfCertain {
		t.Fatalf("sin SSID debe resolver el gateway: %+v", got)
	}

	// Sin SSID y sin ARP —el R1, donde Android no deja leerla— queda el
	// prefijo público, que es estimación y tiene que decirlo.
	got = resuelve(movil, señales{PublicIP: "153.67.115.231"})
	if got.Site != "Socorro" || got.Source != SourcePublicIP || got.Confidence != ConfHigh {
		t.Fatalf("el prefijo debe ubicar y marcarse como estimación: %+v", got)
	}
	if !got.Estimado() {
		t.Fatal("una ubicación por prefijo es estimada y se pinta marcada")
	}
}

func TestSinNingunaSeñalNoSeAfirmaElHomebase(t *testing.T) {
	reinicia(map[string]Site{"Bucaramanga": {Region: "co"}})
	got := resuelve(
		Placement{Class: ClassMobile, Homebase: "Bucaramanga"},
		señales{SSID: "Wifi de un café", LocalIP: "10.44.0.7"},
	)
	if got.Source != SourceHomebase || got.Confidence != ConfNone {
		t.Fatalf("sin señal no se sabe, y hay que decirlo: %+v", got)
	}
	if got.Estimado() {
		t.Fatal("el homebase NO es una estimación: es el valor por defecto")
	}
	// La etiqueta no puede afirmar que está ahí.
	if et := etiquetaSitio(got); et != "Móvil (Bucaramanga)" {
		t.Fatalf("la etiqueta no debe afirmar el sitio: %q", et)
	}
}

func TestSoloSeAprendeDeLoQueSeSabeConCerteza(t *testing.T) {
	reinicia(map[string]Site{
		"Bucaramanga": {Region: "co", SSIDs: []string{"Familia Hernandez"}},
		"Socorro":     {Region: "co", Prefixes: []string{"153.67.115.0/24"}},
	})
	movil := Placement{Class: ClassMobile, Homebase: "Bucaramanga"}

	// Certeza por SSID: lo que se ve desde aquí queda apuntado, y con eso el
	// siguiente que llegue sin SSID se ubica.
	resuelve(movil, señales{SSID: "Familia Hernandez", Gateway: "08:f6:06:99:7c:5c"})
	if s := porGateway("08:f6:06:99:7c:5c"); s != "Bucaramanga" {
		t.Fatalf("debería haber aprendido el router de Bucaramanga, dio %q", s)
	}

	// Estimación por prefijo: NO se aprende. Encadenar deducciones sobre
	// deducciones acaba en un mapa que se confirma a sí mismo.
	reinicia(map[string]Site{"Socorro": {Region: "co", Prefixes: []string{"153.67.115.0/24"}}})
	resuelve(movil, señales{PublicIP: "153.67.115.231", Gateway: "aa:bb:cc:dd:ee:ff"})
	if s := porGateway("aa:bb:cc:dd:ee:ff"); s != "" {
		t.Fatalf("no se aprende de una estimación, aprendió %q", s)
	}
}

func TestLaEtiquetaDistingueUbicadoDeEstimado(t *testing.T) {
	fijo := PlacementInfo{Site: "Socorro", Class: ClassFixed, Source: SourceManual}
	if etiquetaSitio(fijo) != "Socorro" {
		t.Fatal("un fijo se pinta con su sitio a secas")
	}
	cierto := PlacementInfo{Site: "Socorro", Class: ClassMobile,
		Source: SourceGateway, Confidence: ConfCertain}
	if etiquetaSitio(cierto) != "Móvil · Socorro" {
		t.Fatalf("un móvil ubicado con certeza no lleva interrogante: %q", etiquetaSitio(cierto))
	}
	estimado := PlacementInfo{Site: "Socorro", Class: ClassMobile,
		Source: SourcePublicIP, Confidence: ConfHigh}
	if etiquetaSitio(estimado) != "Móvil · Socorro?" {
		t.Fatalf("una estimación se marca: %q", etiquetaSitio(estimado))
	}
}

func TestElCatalogoSeUneEnVezDePisarse(t *testing.T) {
	// Sembrar una red nueva en UN nodo tiene que llegar al resto. Con "la
	// primera que llegó gana" no llegaba nunca: los demás ya tenían esa sede,
	// solo que sin la señal nueva.
	reinicia(map[string]Site{
		"Socorro": {Region: "co", SSIDs: []string{"STEVEN.SIERRA"}},
	})
	mezclaSitios(map[string]Site{
		"Socorro":  {Region: "co", Gateways: []string{"74:24:9f:71:da:85"}},
		"Helsinki": {Region: "eu"},
	}, nil)

	if s := porGateway("74:24:9f:71:da:85"); s != "Socorro" {
		t.Fatalf("la señal nueva debe quedarse, dio %q", s)
	}
	if s := porSSID("STEVEN.SIERRA_5G"); s != "Socorro" {
		t.Fatalf("y la que ya estaba no se pierde, dio %q", s)
	}
	sitios.mu.RLock()
	_, hayNueva := sitios.catalogo["Helsinki"]
	sitios.mu.RUnlock()
	if !hayNueva {
		t.Fatal("una sede que no se conocía se añade entera")
	}
}
