package main

import "testing"

const now = int64(1_000_000)

// todoOnline: observador siempre vivo. La mayoría de los casos no están
// probando esa salvaguarda, así que no tienen por qué cargar con ella.
func todoOnline(string) bool { return true }

// lan: observación medida hace checkAge y sosteniendo el mismo resultado desde
// hace forAge. Ese "desde hace" es lo que decide, no la edad de la cadena.
func lan(reachable bool, checkAge, forAge int64, by string) *LANCheck {
	return &LANCheck{Reachable: reachable, CheckedAt: now - checkAge, Since: now - forAge, By: by}
}

// overlay: contacto por el overlay medido hace atAge, con el mismo resultado
// desde hace forAge.
func overlay(ok bool, atAge, forAge int64, by string) *OverlayCheck {
	return &OverlayCheck{OK: ok, At: now - atAge, Since: now - forAge, By: by}
}

func ble(age int64, by string) *BLECheck {
	return &BLECheck{PeerStatus: "ok", RSSI: -80, LastSeen: now - age, By: by}
}

// state: el sujeto lleva age sin extender su cadena.
func state(age int64, r reachability, obs func(string) bool) string {
	return crossCheckedState(stateByAge(age), now-age, r, now, 180, 300, obs)
}

func TestStateByAge(t *testing.T) {
	for _, c := range []struct {
		age  int64
		want string
	}{
		{60, stateOnline},
		{15 * 60, stateOnline},
		{20 * 60, stateStale},
		{60 * 60, stateOffline},
	} {
		if got := stateByAge(c.age); got != c.want {
			t.Errorf("stateByAge(%d) = %q, quería %q", c.age, got, c.want)
		}
	}
}

// El caso que motivó todo esto: el 2026-09-07 steven-mini estuvo 6 minutos
// apagado por un corte de luz y nodemesh lo reportó "online" de principio a
// fin, porque el hueco cabía entre dos heartbeats. Con el hermano de LAN
// confirmando que no responde, la caída se ve mientras está pasando.
func TestCaidaCortaSeVeCuandoElHermanoDeLANLaConfirma(t *testing.T) {
	age := int64(6 * 60) // el nodo lleva 6 min sin extender su cadena

	if got := state(age, reachability{}, todoOnline); got != stateOnline {
		t.Fatalf("sin observación el veredicto por tiempo no cambia: %q", got)
	}

	r := reachability{Overlay: overlay(false, 30, 4*60, "entry"), LAN: lan(false, 30, 4*60, "dell-xps")}
	if got := state(age, r, todoOnline); got != stateOffline {
		t.Errorf("con el LAN confirmando la caída = %q, quería %q", got, stateOffline)
	}
}

// Con la radio viéndolo, "offline" sería mentira: la máquina está encendida,
// lo que se cayó es la red. Ese fue literalmente el apagón: el router murió
// con la luz y el Bluetooth fue el único testigo que quedó en pie.
func TestBLEVetaElOfflineYLoDejaEnIsolated(t *testing.T) {
	r := reachability{Overlay: overlay(false, 30, 4*60, "entry"),
		LAN: lan(false, 30, 4*60, "dell-xps"), BLE: ble(60, "dell-xps")}
	if got := state(6*60, r, todoOnline); got != stateIsolated {
		t.Errorf("LAN caído pero BLE vivo = %q, quería %q", got, stateIsolated)
	}
}

// La otra dirección, que ya era la promesa del README: una prueba de vida
// directa impide declarar caído a un nodo que solo perdió el overlay.
func TestElLANVivoImpideElOfflinePorTiempo(t *testing.T) {
	r := reachability{LAN: lan(true, 30, 3*60*60, "dell-xps")}
	if got := state(3*60*60, r, todoOnline); got != stateStale {
		t.Errorf("3h sin reportar pero vivo en LAN = %q, quería %q", got, stateStale)
	}
	rb := reachability{BLE: ble(60, "dell-xps")}
	if got := state(3*60*60, rb, todoOnline); got != stateIsolated {
		t.Errorf("3h sin reportar pero la radio lo ve = %q, quería %q", got, stateIsolated)
	}
}

// El grace existe para que un blip de red no tumbe a un nodo sano: hace falta
// que el ping lleve VARIOS ciclos fallando, no uno suelto.
func TestUnPingFallidoSueltoNoTumbaANadie(t *testing.T) {
	r := reachability{Overlay: overlay(false, 10, 30, "entry"), LAN: lan(false, 10, 30, "dell-xps")} // falla hace 30 s
	if got := state(6*60, r, todoOnline); got != stateOnline {
		t.Errorf("un blip de 30 s = %q, quería %q", got, stateOnline)
	}
}

// El caso que destapó la prueba en vivo: la cadena solo crece en cambios de
// estado y en el heartbeat de 10 min, así que un nodo SANO pasa minutos sin
// escribir. Juzgar por la edad de la cadena marcaba offline nodos vivos; lo
// que decide es que el sujeto no haya escrito nada DESPUÉS de que el ping
// empezara a fallar.
func TestUnNodoVivoQueLlevaRatoCalladoNoSeMarcaCaido(t *testing.T) {
	// Lo destapó la prueba en vivo: el ping fallaba siempre (mDNS que no
	// resuelve) y el nodo llevaba minutos sin escribir por el heartbeat lento,
	// así que las dos condiciones se cumplían con el nodo perfectamente vivo.
	// Lo que lo desmiente es que el gossip SÍ le habla y contesta.
	r := reachability{Overlay: overlay(true, 30, 10*60, "entry"),
		LAN: lan(false, 30, 10*60, "dell-xps")}
	if got := state(9*60, r, todoOnline); got != stateOnline {
		t.Errorf("contesta el gossip pero no el ping = %q, quería %q", got, stateOnline)
	}
	// Nueve minutos callado es normal con heartbeat de 10 min: sin ping que
	// falle sostenidamente, no hay caída que declarar.
	if got := state(9*60, reachability{}, todoOnline); got != stateOnline {
		t.Errorf("9 min callado y sin observación = %q, quería %q", got, stateOnline)
	}
}

// Falso positivo clásico: al observador se le cae el WiFi, su ping al vecino
// falla, y sin este filtro reportaría su propia avería como una caída ajena.
func TestNoSeCreeAUnObservadorCaido(t *testing.T) {
	r := reachability{Overlay: overlay(false, 30, 4*60, "entry"), LAN: lan(false, 30, 4*60, "dell-xps")}
	obs := func(n string) bool { return n != "dell-xps" }
	if got := state(6*60, r, obs); got != stateOnline {
		t.Errorf("observación de un observador caído = %q, quería %q", got, stateOnline)
	}
}

// Un negativo viejo no puede declarar caído a un nodo para siempre: cuando el
// nodo vuelve, su cadena manda otra vez.
func TestLaObservacionRanciaSeIgnora(t *testing.T) {
	r := reachability{Overlay: overlay(false, 20*60, 30*60, "entry"),
		LAN: lan(false, 20*60, 30*60, "dell-xps")} // medidas hace 20 min > obsTTL
	if got := state(6*60, r, todoOnline); got != stateOnline {
		t.Errorf("observación caducada = %q, quería %q", got, stateOnline)
	}
}

// Relevo por gossip: gana la copia más nueva y queda anotado quién observó,
// que es lo que permite después exigir que ese nodo estuviera online.
func TestSetPeerChecksSeQuedaConLoMasNuevo(t *testing.T) {
	peerChecks.lan = map[string]LANCheck{}
	peerChecks.ble = map[string]BLECheck{}

	setPeerChecks("steven-mini", "dell-xps", &LANCheck{Reachable: true, CheckedAt: 100}, nil)
	setPeerChecks("steven-mini", "entry", &LANCheck{Reachable: false, CheckedAt: 50}, nil)
	if got := getPeerChecks("steven-mini"); got.LAN.CheckedAt != 100 || !got.LAN.Reachable {
		t.Errorf("una observación vieja pisó a la nueva: %+v", got.LAN)
	}
	if got := getPeerChecks("steven-mini"); got.LAN.By != "dell-xps" {
		t.Errorf("autor = %q, quería dell-xps", got.LAN.By)
	}

	setPeerChecks("steven-mini", "dell-xps", &LANCheck{Reachable: false, CheckedAt: 200}, nil)
	if got := getPeerChecks("steven-mini"); got.LAN.Reachable {
		t.Error("la observación más nueva no reemplazó a la anterior")
	}
	// Sin timestamp no hay forma de ordenarla: se descarta en vez de pisar.
	setPeerChecks("steven-mini", "dell-xps", &LANCheck{Reachable: true, CheckedAt: 0}, nil)
	if got := getPeerChecks("steven-mini"); got.LAN.CheckedAt != 200 {
		t.Error("una observación sin timestamp entró igual")
	}
}

// Una sola vía negativa no basta: que YO no lo alcance por el overlay no es lo
// mismo que que nadie lo alcance, y puede ser mi ruta la rota.
func TestUnaSolaViaNegativaNoDeclaraCaida(t *testing.T) {
	solo := reachability{Overlay: overlay(false, 30, 10*60, "entry")}
	if got := state(6*60, solo, todoOnline); got != stateOnline {
		t.Errorf("solo el overlay caído = %q, quería %q", got, stateOnline)
	}
	// Y sigue valiendo el veredicto por tiempo cuando de verdad lleva horas.
	if got := state(3*60*60, solo, todoOnline); got != stateOffline {
		t.Errorf("3h sin nada = %q, quería %q", got, stateOffline)
	}
}

// El contacto por overlay manda sobre todo lo demás: es prueba de vida directa.
func TestElContactoPorOverlayGanaATodo(t *testing.T) {
	r := reachability{Overlay: overlay(true, 30, 60, "entry"),
		LAN: lan(false, 30, 60*60, "dell-xps")}
	if got := state(3*60*60, r, todoOnline); got != stateOnline {
		t.Errorf("contesta ahora mismo = %q, quería %q", got, stateOnline)
	}
}

// setOverlayCheck conserva el "desde cuándo" mientras el resultado no cambie:
// es lo que permite exigir varios ciclos fallando en vez de uno suelto.
func TestSetOverlayCheckConservaElSince(t *testing.T) {
	peerChecks.overlay = map[string]OverlayCheck{}
	setOverlayCheck("steven-mini", "entry", false, 1000)
	setOverlayCheck("steven-mini", "entry", false, 1120)
	if got := getPeerChecks("steven-mini"); got.Overlay.Since != 1000 {
		t.Errorf("since = %d, quería 1000 (lleva fallando desde entonces)", got.Overlay.Since)
	}
	setOverlayCheck("steven-mini", "entry", true, 1240) // volvió
	if got := getPeerChecks("steven-mini"); got.Overlay.Since != 1240 {
		t.Errorf("al cambiar el resultado el since se reinicia: %d", got.Overlay.Since)
	}
}
