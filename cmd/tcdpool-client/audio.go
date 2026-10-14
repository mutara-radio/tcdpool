package main

import (
	"encoding/binary"
	"math"
	"os"
)

const rate = 8000

var signalFuncs = map[string]func(n int) []int16{
	"tone1k":  func(n int) []int16 { return tone(n, 1000) },
	"tone440": func(n int) []int16 { return tone(n, 440) },
	"speech":  speech,
}

var toneFreq = map[string]float64{"tone1k": 1000, "tone440": 440}

func tone(n int, f float64) []int16 {
	out := make([]int16, n)
	for i := range out {
		out[i] = int16(10000 * math.Sin(2*math.Pi*f*float64(i)/rate))
	}
	return out
}

// speech is a speech-like signal: a voiced source with a moving pitch
// (90..150 Hz), three moving formants, syllables at about 4 Hz and a
// pause of 0.4 s every 2 s. It is the same each time.
func speech(n int) []int16 {
	const harmonics = 40
	x := make([]float64, n)
	var phase [harmonics + 1]float64
	peak := 0.0
	for i := range x {
		t := float64(i) / rate
		f0 := 120 + 30*math.Sin(2*math.Pi*0.7*t)
		f1 := 650 + 150*math.Sin(2*math.Pi*1.3*t)
		f2 := 1600 + 400*math.Sin(2*math.Pi*0.9*t+1)
		f3 := 2600.0
		v := 0.0
		for k := 1; k <= harmonics; k++ {
			f := f0 * float64(k)
			phase[k] += 2 * math.Pi * f / rate
			if f > 3400 {
				continue
			}
			w := res(f, f1, 90) + 0.6*res(f, f2, 120) + 0.3*res(f, f3, 150)
			v += w * math.Sin(phase[k])
		}
		syl := math.Max(0, math.Sin(2*math.Pi*4*t))
		env := math.Sqrt(syl)
		if math.Mod(t, 2) > 1.6 {
			env = 0
		}
		x[i] = v * env
		peak = math.Max(peak, math.Abs(x[i]))
	}
	out := make([]int16, n)
	for i := range x {
		out[i] = int16(12000 * x[i] / peak)
	}
	return out
}

func res(f, fc, bw float64) float64 { d := (f - fc) / bw; return 1 / (1 + d*d) }

func pcmBytes(s []int16) []byte {
	b := make([]byte, 2*len(s))
	for i, v := range s {
		binary.LittleEndian.PutUint16(b[2*i:], uint16(v))
	}
	return b
}

func pcmSamples(b []byte) []int16 {
	s := make([]int16, len(b)/2)
	for i := range s {
		s[i] = int16(binary.LittleEndian.Uint16(b[2*i:]))
	}
	return s
}

type audioCheck struct {
	DelaySamples int     `json:"delay_samples"`
	Correlation  float64 `json:"correlation"`   // waveform, at the best delay
	EnvelopeCorr float64 `json:"envelope_corr"` // 5 ms RMS envelope
	EnergyRatio  float64 `json:"energy_ratio"`  // out / in
	TonePurity   float64 `json:"tone_purity,omitempty"`
	Pass         bool    `json:"pass"`
}

const block = 40 // 5 ms

func envelope(s []int16) []float64 {
	e := make([]float64, len(s)/block)
	for i := range e {
		var sum float64
		for _, v := range s[i*block : (i+1)*block] {
			sum += float64(v) * float64(v)
		}
		e[i] = math.Sqrt(sum / block)
	}
	return e
}

func pearson(a, b []float64) float64 {
	n := min(len(a), len(b))
	if n < 2 {
		return 0
	}
	var ma, mb float64
	for i := range n {
		ma += a[i]
		mb += b[i]
	}
	ma /= float64(n)
	mb /= float64(n)
	var sab, saa, sbb float64
	for i := range n {
		sab += (a[i] - ma) * (b[i] - mb)
		saa += (a[i] - ma) * (a[i] - ma)
		sbb += (b[i] - mb) * (b[i] - mb)
	}
	if saa == 0 || sbb == 0 {
		return 0
	}
	return sab / math.Sqrt(saa*sbb)
}

// normCorr is the normalized correlation of in and out delayed by lag.
func normCorr(in, out []int16, lag int) float64 {
	var sab, saa, sbb float64
	for i := 0; i+lag < len(out) && i < len(in); i++ {
		a, b := float64(in[i]), float64(out[i+lag])
		sab += a * b
		saa += a * a
		sbb += b * b
	}
	if saa == 0 || sbb == 0 {
		return 0
	}
	return sab / math.Sqrt(saa*sbb)
}

func energy(s []int16) float64 {
	var e float64
	for _, v := range s {
		e += float64(v) * float64(v)
	}
	return e
}

// purity is the part of the energy at f (1 for a pure tone), with the
// Goertzel algorithm over 50 ms blocks.
func purity(s []int16, f float64) float64 {
	const n = 400
	var at, total float64
	for start := 0; start+n <= len(s); start += n {
		w := 2 * math.Cos(2*math.Pi*f/rate)
		var s1, s2 float64
		for _, v := range s[start : start+n] {
			s0 := float64(v) + w*s1 - s2
			s2, s1 = s1, s0
		}
		at += s1*s1 + s2*s2 - w*s1*s2
		total += energy(s[start : start+n])
	}
	if total == 0 {
		return 0
	}
	return 2 * at / (n * total)
}

// check compares the output with the input. Vocoders keep the spectrum
// and the envelope but not the waveform phase, so the waveform
// correlation of a vocoder can be low; the pass test uses the envelope
// (speech) and the tone purity (tones).
func check(sig string, in, out []int16) audioCheck {
	var a audioCheck
	ein, eout := envelope(in), envelope(out)
	best, bestLag := -2.0, 0
	for lag := 0; lag <= 60 && lag < len(eout); lag++ { // up to 300 ms
		if c := pearson(ein, eout[lag:]); c > best {
			best, bestLag = c, lag
		}
	}
	a.EnvelopeCorr = best
	center := bestLag * block
	bc, bl := -2.0, center
	for lag := max(0, center-block); lag <= center+block; lag++ {
		if c := normCorr(in, out, lag); c > bc {
			bc, bl = c, lag
		}
	}
	a.DelaySamples, a.Correlation = bl, bc
	n := min(len(in), len(out)-bl)
	if n > 0 {
		if e := energy(in[:n]); e > 0 {
			a.EnergyRatio = energy(out[bl:bl+n]) / e
		}
	}
	energyOK := a.EnergyRatio > 0.1 && a.EnergyRatio < 10
	if f, ok := toneFreq[sig]; ok {
		skip := min(len(out), bl+rate/10)
		a.TonePurity = purity(out[skip:], f)
		a.Pass = energyOK && a.TonePurity > 0.5
	} else {
		a.Pass = energyOK && a.EnvelopeCorr > 0.6
	}
	return a
}

func writeWAV(path string, s []int16) error {
	b := make([]byte, 44, 44+2*len(s))
	copy(b, "RIFF")
	binary.LittleEndian.PutUint32(b[4:], uint32(36+2*len(s)))
	copy(b[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(b[16:], 16)
	binary.LittleEndian.PutUint16(b[20:], 1) // PCM
	binary.LittleEndian.PutUint16(b[22:], 1) // mono
	binary.LittleEndian.PutUint32(b[24:], rate)
	binary.LittleEndian.PutUint32(b[28:], rate*2)
	binary.LittleEndian.PutUint16(b[32:], 2)
	binary.LittleEndian.PutUint16(b[34:], 16)
	copy(b[36:], "data")
	binary.LittleEndian.PutUint32(b[40:], uint32(2*len(s)))
	return os.WriteFile(path, append(b, pcmBytes(s)...), 0o644)
}
