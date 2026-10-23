package main

import "strings"

// conFabricante antepone el fabricante a un codename ARM corto cuando el
// prefijo lo identifica con seguridad: "MT6765" → "Mediatek MT6765". Si no
// reconoce el prefijo, o si el string ya trae un nombre humano (hay espacios o
// @, p.ej. "AMD Ryzen 7 5700U with Radeon Graphics" o "Intel(R) Xeon(R) CPU @
// 2.20GHz"), lo devuelve igual.
//
// Vive aquí y no en specs_linux.go para que corra el test en darwin también.
func conFabricante(model string) string {
	if model == "" || strings.ContainsAny(model, " @") {
		return model
	}
	tabla := []struct {
		pref, fab string
	}{
		{"MSM", "Qualcomm"}, // MSM8998, MSM8974
		{"SDM", "Qualcomm"}, // SDM845, SDM660
		{"APQ", "Qualcomm"}, // APQ8084
		{"QCS", "Qualcomm"}, // QCS605
		{"SDA", "Qualcomm"},
		{"SM", "Qualcomm"},  // SM8450, SM7250 — Snapdragon moderno
		{"MT", "Mediatek"},  // MT6765 (R1), MT8183
		{"BCM", "Broadcom"}, // BCM2711 (Raspberry Pi 4)
		{"RK", "Rockchip"},  // RK3399
		{"AC", "Allwinner"},
	}
	for _, p := range tabla {
		if strings.HasPrefix(model, p.pref) && len(model) > len(p.pref) {
			// El carácter que sigue tiene que ser un dígito: así "MT6765"
			// cuenta y palabras como "Model" no.
			if r := model[len(p.pref)]; r >= '0' && r <= '9' {
				return p.fab + " " + model
			}
		}
	}
	return model
}
