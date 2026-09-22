package main

import (
	"sync"
	"time"
)

// NetLink es la velocidad de red del nodo, y son DOS cosas distintas que
// conviene no confundir nunca:
//
//   - LinkMbps es la velocidad NEGOCIADA del primer salto (1000baseT, la tasa
//     PHY del wifi). Es capacidad teórica del cable o del aire, NO lo que baja
//     de internet: un portátil pegado al router negocia 866 Mb/s aunque la
//     línea de la casa dé 50.
//   - RxKbps/TxKbps es el tráfico REALMENTE observado, derivado de los
//     contadores de bytes de la interfaz.
//
// Las dos son PASIVAS: se leen contadores que el sistema ya lleva, no se
// genera un solo byte de tráfico. Esa es la razón de que aquí no haya
// speedtest — medir activamente gasta datos y batería, y esta flota tiene
// nodos que no pueden pagarlo (el R1 y los móviles por batería, gcp-east por
// ser una e2-micro con salida medida). El coste de todo este fichero es leer
// dos ficheros de /sys cada 30 s.
//
// El pico observado (RxPeakKbps) es lo más cerca de "cuánta red hay de verdad"
// que se llega sin medir activamente, y hay que leerlo como lo que es: un PISO
// DEMOSTRADO, no una capacidad. Si alguna vez bajó a 40 Mb/s es que hay al
// menos 40 Mb/s; que nunca haya pasado de 2 Mb/s no dice que no haya más, dice
// que nadie lo ha pedido.
//
// Va FUERA de la cadena hash, dentro de Specs, por la misma razón que la RAM
// usada: la cadena registra CAMBIOS DE ESTADO DE RED y estos números cambian
// cada minuto — meterlos en Record la convertiría en una serie temporal sin
// tope (ver el comentario de cabecera de Specs).
type NetLink struct {
	Iface      string  `json:"iface,omitempty"`
	LinkMbps   int     `json:"linkMbps,omitempty"`   // 0 = el sistema no lo publica
	LinkSource string  `json:"linkSource,omitempty"` // de dónde salió LinkMbps
	RSSI       int     `json:"rssi,omitempty"`       // dBm, sólo wifi
	RxKbps     float64 `json:"rxKbps,omitempty"`
	TxKbps     float64 `json:"txKbps,omitempty"`
	RxPeakKbps float64 `json:"rxPeakKbps,omitempty"`
	TxPeakKbps float64 `json:"txPeakKbps,omitempty"`
	WindowSec  int     `json:"windowSec,omitempty"` // ventana sobre la que se promedió
	PeakSince  int64   `json:"peakSince,omitempty"` // unix; los picos se reinician con el proceso
}

// netMaxWindow: ventana máxima que se acepta para calcular una tasa. Si entre
// dos lecturas pasó más que esto —el proceso estuvo dormido, el portátil
// cerrado— el promedio no describe nada: repartiría entre tres horas un pico
// de tres segundos. Mejor no publicar tasa esa ronda y medir en la siguiente.
const netMaxWindow = 15 * time.Minute

// netSample es una lectura de los contadores en un instante. Una tasa necesita
// dos, asi que la primera ronda tras arrancar siempre sale sin tasa: siembra la
// referencia.
type netSample struct {
	at     time.Time
	iface  string
	rx, tx uint64
}

// netRate calcula la tasa entre dos muestras, o dice que no se puede.
//
// No se puede en tres casos, y los tres pasan de verdad en esta flota:
//   - cambio la interfaz: saltar de wifi a ethernet (o al reves, que es lo que
//     hacen los portatiles al llegar a casa) empieza contadores nuevos, y
//     restarlos daria una tasa inventada;
//   - los contadores retrocedieron: se reinicio la interfaz, o —en Windows—
//     desbordaron los 32 bits de MIB_IFROW;
//   - la ventana es absurda: ver netMaxWindow.
//
// Cuando devuelve ok=false la ronda solo siembra la referencia y la siguiente
// ya mide. Es preferible un hueco a un numero que no significa nada.
func netRate(prev, cur netSample) (rxKbps, txKbps float64, windowSec int, ok bool) {
	if prev.at.IsZero() || prev.iface != cur.iface || cur.rx < prev.rx || cur.tx < prev.tx {
		return 0, 0, 0, false
	}
	win := cur.at.Sub(prev.at)
	if win < time.Second || win > netMaxWindow {
		return 0, 0, 0, false
	}
	secs := win.Seconds()
	return float64(cur.rx-prev.rx) * 8 / 1000 / secs,
		float64(cur.tx-prev.tx) * 8 / 1000 / secs,
		int(win.Round(time.Second) / time.Second), true
}

var netSamples = struct {
	mu             sync.Mutex
	prev           netSample
	peakRx, peakTx float64
	since          int64
}{}

// localNetLink devuelve la velocidad de red de ESTE nodo, o nil si no se pudo
// identificar la interfaz por la que sale (nodo sin ruta por defecto).
func localNetLink() *NetLink {
	n, rx, tx := platformNet()
	if n.Iface == "" {
		return nil
	}
	cur := netSample{at: time.Now(), iface: n.Iface, rx: rx, tx: tx}

	netSamples.mu.Lock()
	defer netSamples.mu.Unlock()
	if netSamples.since == 0 {
		netSamples.since = cur.at.Unix()
	}
	prev := netSamples.prev
	netSamples.prev = cur

	if rxKbps, txKbps, win, ok := netRate(prev, cur); ok {
		n.RxKbps, n.TxKbps, n.WindowSec = rxKbps, txKbps, win
		if rxKbps > netSamples.peakRx {
			netSamples.peakRx = rxKbps
		}
		if txKbps > netSamples.peakTx {
			netSamples.peakTx = txKbps
		}
	}
	// Los picos se devuelven aunque esta ronda no haya podido medir: son
	// historia acumulada, no dependen de que la ventana de ahora sirviera.
	n.RxPeakKbps, n.TxPeakKbps = netSamples.peakRx, netSamples.peakTx
	n.PeakSince = netSamples.since
	return &n
}
