package main

import (
	"math"
	"testing"
	"time"
)

func muestra(segs int, iface string, rx, tx uint64) netSample {
	base := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	return netSample{at: base.Add(time.Duration(segs) * time.Second), iface: iface, rx: rx, tx: tx}
}

func casi(a, b float64) bool { return math.Abs(a-b) < 0.01 }

func TestLaTasaSaleDeLaDiferenciaDeBytesEnLaVentana(t *testing.T) {
	// 3.750.000 bytes en 30 s = 1.000 kb/s justos (x8 bits, /1000, /30).
	prev := muestra(0, "en0", 1_000_000, 500_000)
	cur := muestra(30, "en0", 1_000_000+3_750_000, 500_000+375_000)

	rx, tx, win, ok := netRate(prev, cur)
	if !ok {
		t.Fatal("dos muestras buenas de la misma interfaz tienen que dar tasa")
	}
	if !casi(rx, 1000) {
		t.Errorf("rx = %v kb/s, se esperaba 1000", rx)
	}
	if !casi(tx, 100) {
		t.Errorf("tx = %v kb/s, se esperaba 100", tx)
	}
	if win != 30 {
		t.Errorf("ventana = %d s, se esperaba 30", win)
	}
}

func TestLaPrimeraMuestraNoInventaTasa(t *testing.T) {
	// Sin lectura anterior no hay nada que restar. Es el caso de cada
	// arranque del proceso, y tiene que quedar en silencio, no en cero:
	// un cero se leeria como "no hay trafico".
	if _, _, _, ok := netRate(netSample{}, muestra(30, "en0", 5_000_000, 1_000_000)); ok {
		t.Fatal("la primera muestra no puede producir tasa")
	}
}

func TestCambiarDeWifiAEthernetNoProduceUnaTasaFalsa(t *testing.T) {
	// El portatil llega a casa y se engancha al cable: los contadores de la
	// interfaz nueva no tienen nada que ver con los de la vieja, y restarlos
	// daria un pico enorme que nunca ocurrio.
	prev := muestra(0, "en0", 900_000_000, 100_000_000)
	cur := muestra(30, "en5", 1_000, 500)
	if _, _, _, ok := netRate(prev, cur); ok {
		t.Fatal("dos interfaces distintas no son comparables")
	}
}

func TestContadoresQueRetrocedenSeDescartan(t *testing.T) {
	// Pasa al reiniciarse la interfaz, y en Windows al desbordar los 32 bits
	// de MIB_IFROW. La resta sin signo daria un numero astronomico.
	prev := muestra(0, "eth0", 4_000_000_000, 4_000_000_000)
	cur := muestra(30, "eth0", 12_000, 9_000)
	if _, _, _, ok := netRate(prev, cur); ok {
		t.Fatal("un contador que retrocede no puede dar tasa")
	}
}

func TestUnaVentanaDemasiadoLargaNoSePromedia(t *testing.T) {
	// El Mac estuvo cerrado tres horas. Promediar ahi dentro repartiria en
	// tres horas lo que pudo ser una descarga de tres segundos.
	prev := muestra(0, "en0", 0, 0)
	cur := muestra(3*60*60, "en0", 10_000_000_000, 1_000_000_000)
	if _, _, _, ok := netRate(prev, cur); ok {
		t.Fatal("una ventana mayor que netMaxWindow no describe nada")
	}
}

func TestUnaVentanaDeCeroNoDivideEntreCero(t *testing.T) {
	// Dos lecturas en el mismo segundo (dos peticiones seguidas si alguien
	// salta la cache): sin esto seria una division entre cero.
	prev := muestra(10, "en0", 1_000, 1_000)
	cur := muestra(10, "en0", 2_000, 2_000)
	if _, _, _, ok := netRate(prev, cur); ok {
		t.Fatal("una ventana de cero segundos no puede dar tasa")
	}
}

func TestElTraficoQuietoDaCeroYNoUnHueco(t *testing.T) {
	// Un nodo sin trafico SI tiene tasa medida, y es cero. Es distinto de "no
	// se pudo medir": el UI y las alertas necesitan poder diferenciarlos.
	prev := muestra(0, "eth0", 777, 777)
	cur := muestra(60, "eth0", 777, 777)
	rx, tx, win, ok := netRate(prev, cur)
	if !ok || rx != 0 || tx != 0 || win != 60 {
		t.Fatalf("un nodo quieto mide cero: rx=%v tx=%v win=%d ok=%v", rx, tx, win, ok)
	}
}
