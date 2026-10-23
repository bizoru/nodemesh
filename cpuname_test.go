package main

import "testing"

func TestConFabricante(t *testing.T) {
	casos := map[string]string{
		"MT6765":  "Mediatek MT6765",
		"MT8183":  "Mediatek MT8183",
		"MSM8998": "Qualcomm MSM8998",
		"SM8450":  "Qualcomm SM8450",
		"SDM660":  "Qualcomm SDM660",
		"BCM2711": "Broadcom BCM2711",
		"RK3399":  "Rockchip RK3399",
		"12th Gen Intel(R) Core(TM) i7-1255U":    "12th Gen Intel(R) Core(TM) i7-1255U",
		"AMD Ryzen 7 5700U with Radeon Graphics": "AMD Ryzen 7 5700U with Radeon Graphics",
		"Intel(R) Xeon(R) CPU @ 2.20GHz":         "Intel(R) Xeon(R) CPU @ 2.20GHz",
		"":            "",
		"Apple M4":    "Apple M4",
		"ModelThingy": "ModelThingy",
		"MyProcessor": "MyProcessor",
	}
	for in, want := range casos {
		if got := conFabricante(in); got != want {
			t.Errorf("conFabricante(%q) = %q, quería %q", in, got, want)
		}
	}
}
