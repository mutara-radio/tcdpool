package main

import "testing"

func TestCheckIdentity(t *testing.T) {
	for sig, f := range signalFuncs {
		in := f(8000)
		out := append(make([]int16, 160), in...) // 20 ms delay
		a := check(sig, in, out)
		if !a.Pass || a.DelaySamples != 160 || a.Correlation < 0.99 || a.EnergyRatio < 0.98 || a.EnergyRatio > 1.02 {
			t.Errorf("%s: %+v", sig, a)
		}
	}
	if a := check("speech", speech(8000), make([]int16, 8000)); a.Pass {
		t.Errorf("silence passed: %+v", a)
	}
	if p := purity(tone(4000, 1000), 1000); p < 0.99 || p > 1.01 {
		t.Errorf("purity %f", p)
	}
	if p := purity(tone(4000, 440), 1000); p > 0.05 {
		t.Errorf("purity at the wrong frequency %f", p)
	}
}
