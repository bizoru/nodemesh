package main

import "testing"

// Un nodo movil que se va no es un incidente. Sin esta exencion, el dead-man
// alertaba por un portatil cerrado o una tablet guardada, que es justo el ruido
// que hace que se dejen de leer las alertas que si importan.
func TestEsMovilSoloExentaALosDeLaLista(t *testing.T) {
	cfg.Moviles = []string{"macbook-pro", "legion", "rabbit-r1", "rigby"}

	for _, n := range cfg.Moviles {
		if !esMovil(n) {
			t.Errorf("%s esta en la lista de moviles y deberia estar exento", n)
		}
	}
	// Los fijos tienen que seguir alertando: son los que sostienen el cluster.
	for _, n := range []string{"entry", "athena", "gcp-east", "x1-nano", "steven-mini", "dell-xps"} {
		if esMovil(n) {
			t.Errorf("%s es un nodo fijo: que se vaya SI es un incidente", n)
		}
	}
}

func TestSinListaNadieQuedaExento(t *testing.T) {
	cfg.Moviles = nil
	if esMovil("macbook-pro") {
		t.Error("sin lista configurada no debe eximir a nadie")
	}
}

// MCL-199: el mensaje del dead-man tiene que traer memoria cuando la hay, y
// no inventar un "0/0 MB (0%)" cuando un cliente viejo (o un nodo movil que
// nunca la mando) no la trajo nunca.
func TestMemInfoSuffixConDatos(t *testing.T) {
	ns := &NodeState{MemUsedMB: 3502, MemTotalMB: 4096}
	got := memInfoSuffix(ns)
	want := " Última memoria conocida: 3502/4096 MB (85%)."
	if got != want {
		t.Errorf("memInfoSuffix() = %q, want %q", got, want)
	}
}

func TestMemInfoSuffixSinDatos(t *testing.T) {
	ns := &NodeState{}
	if got := memInfoSuffix(ns); got != "" {
		t.Errorf("memInfoSuffix() sin memoria = %q, want vacio", got)
	}
}
