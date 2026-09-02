package modbus

import (
	"math"
	"strings"
	"testing"
)

func mustFormat(t *testing.T, s string) Format {
	t.Helper()
	f, err := ParseFormat(s)
	if err != nil {
		t.Fatalf("ParseFormat(%q): %v", s, err)
	}
	return f
}

func TestParseFormat(t *testing.T) {
	for _, kind := range []string{"int16", "uint16", "int32", "uint32", "float32", "float64", "bool"} {
		f := mustFormat(t, kind)
		if f.Kind != kind || f.String() != kind {
			t.Errorf("ParseFormat(%q) = %+v", kind, f)
		}
	}
	if f := mustFormat(t, "bit:0"); f.Kind != "bit" || f.Bit != 0 {
		t.Errorf("bit:0 = %+v", f)
	}
	if f := mustFormat(t, "bit:15"); f.Bit != 15 || f.String() != "bit:15" {
		t.Errorf("bit:15 = %+v", f)
	}
	for _, bad := range []string{"bit:16", "bit:-1", "bit:x", "bit", "int64", ""} {
		if _, err := ParseFormat(bad); err == nil {
			t.Errorf("ParseFormat(%q): want error", bad)
		}
	}
	// A typo suggests the real format.
	_, err := ParseFormat("flaot32")
	if err == nil || !strings.Contains(err.Error(), "did you mean float32?") {
		t.Errorf("flaot32: %v", err)
	}
}

func TestFormatWords(t *testing.T) {
	words := map[string]int{
		"int16": 1, "uint16": 1, "bool": 1, "bit:7": 1,
		"int32": 2, "uint32": 2, "float32": 2, "float64": 4,
	}
	for s, want := range words {
		if got := mustFormat(t, s).Words(); got != want {
			t.Errorf("%s.Words() = %d, want %d", s, got, want)
		}
	}
}

func TestDecodeGolden(t *testing.T) {
	cases := []struct {
		name      string
		format    string
		regs      []uint16
		word, byt string
		scale     float64
		offset    float64
		want      any
	}{
		{name: "int16 negative", format: "int16", regs: []uint16{0xFFFE}, want: int64(-2)},
		{name: "uint16 high", format: "uint16", regs: []uint16{0xFFFE}, want: int64(65534)},
		{name: "int32", format: "int32", regs: []uint16{0xFFFF, 0xFFFE}, want: int64(-2)},
		{name: "uint32", format: "uint32", regs: []uint16{0x0001, 0x0000}, want: int64(65536)},
		{name: "float32", format: "float32", regs: []uint16{0x42F6, 0xE979}, want: float64(float32(123.456))},
		{name: "float64", format: "float64", regs: []uint16{0x3FF8, 0, 0, 0}, want: 1.5},
		{name: "bool set", format: "bool", regs: []uint16{5}, want: true},
		{name: "bool clear", format: "bool", regs: []uint16{0}, want: false},
		{name: "bit set", format: "bit:3", regs: []uint16{0x0008}, want: true},
		{name: "bit clear", format: "bit:2", regs: []uint16{0x0008}, want: false},
		// tentacle reverseWords: least-significant register first.
		{name: "float32 word little", format: "float32", regs: []uint16{0xE979, 0x42F6}, word: OrderLittle, want: float64(float32(123.456))},
		// tentacle reverseBits: bytes swapped within each register.
		{name: "uint16 byte little", format: "uint16", regs: []uint16{0x3412}, byt: OrderLittle, want: int64(0x1234)},
		{name: "float32 both little", format: "float32", regs: []uint16{0x79E9, 0xF642}, word: OrderLittle, byt: OrderLittle, want: float64(float32(123.456))},
		// engineering = raw*Scale + Offset; Scale 0 means 1.
		{name: "scaled uint16", format: "uint16", regs: []uint16{123}, scale: 0.1, offset: -40, want: 123*0.1 - 40},
		{name: "offset only", format: "int16", regs: []uint16{10}, offset: 5, want: float64(15)},
		{name: "scale on float", format: "float32", regs: []uint16{0x3FC0, 0x0000}, scale: 2, want: 3.0}, // 1.5 * 2
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeRegisters(mustFormat(t, tc.format), tc.regs, tc.word, tc.byt, tc.scale, tc.offset)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got != tc.want {
				t.Errorf("decode = %v (%T), want %v (%T)", got, got, tc.want, tc.want)
			}
		})
	}
}

func TestDecodeWrongLength(t *testing.T) {
	if _, err := decodeRegisters(mustFormat(t, "float32"), []uint16{1}, "", "", 0, 0); err == nil {
		t.Fatal("float32 from one register: want error")
	}
}

// TestRoundTrip encodes and decodes every format under every word/byte
// order and scaling combination — the §7.1 table. Samples are RAW device
// values; for a scaled combination the engineering value raw*Scale+Offset
// goes in and must come back exactly (the scale is a power of two and the
// raws are small enough that every step is float64-exact). With identity
// scaling, integer formats must come back as int64, not float64.
func TestRoundTrip(t *testing.T) {
	rawSamples := map[string][]float64{
		"int16":   {-2, 0, 32767, -32768},
		"uint16":  {0, 1, 65535},
		"int32":   {-123456, 2147483647},
		"uint32":  {0, 4000000000},
		"float32": {1.5, -0.25, 0},
		"float64": {1.5, -1234.0625, 0},
	}
	type scaling struct{ scale, offset float64 }
	scalings := []scaling{{0, 0}, {1, 0}, {0.5, 10}}
	for formatStr, raws := range rawSamples {
		f := mustFormat(t, formatStr)
		for _, word := range []string{"", OrderBig, OrderLittle} {
			for _, byt := range []string{"", OrderBig, OrderLittle} {
				for _, sc := range scalings {
					identity := (sc.scale == 0 || sc.scale == 1) && sc.offset == 0
					for _, raw := range raws {
						var input, want any
						switch {
						case identity && f.integer():
							input, want = int64(raw), int64(raw)
						case identity:
							input, want = raw, raw
						default:
							eng := raw*sc.scale + sc.offset
							input, want = eng, eng
						}
						regs, err := encodeRegisters(f, input, word, byt, sc.scale, sc.offset)
						if err != nil {
							t.Fatalf("%s %v word=%q byte=%q %+v: encode: %v", formatStr, input, word, byt, sc, err)
						}
						if len(regs) != f.Words() {
							t.Fatalf("%s: encoded %d registers, want %d", formatStr, len(regs), f.Words())
						}
						got, err := decodeRegisters(f, regs, word, byt, sc.scale, sc.offset)
						if err != nil {
							t.Fatalf("%s %v: decode: %v", formatStr, input, err)
						}
						if got != want {
							t.Errorf("%s raw %v word=%q byte=%q %+v: round trip = %v (%T), want %v (%T)",
								formatStr, raw, word, byt, sc, got, got, want, want)
						}
					}
				}
			}
		}
	}
	// bool and bit:N ignore scaling entirely and round-trip as bool.
	for _, formatStr := range []string{"bool", "bit:0", "bit:15"} {
		f := mustFormat(t, formatStr)
		for _, v := range []bool{true, false} {
			regs, err := encodeRegisters(f, v, "", OrderLittle, 0.5, 10)
			if err != nil {
				t.Fatalf("%s %v: encode: %v", formatStr, v, err)
			}
			got, err := decodeRegisters(f, regs, "", OrderLittle, 0.5, 10)
			if err != nil || got != v {
				t.Errorf("%s %v: round trip = %v, %v", formatStr, v, got, err)
			}
		}
	}
}

// The encoder inverts scaling before converting, so an engineering value
// writes the raw the device expects: 25.0 with scale 0.1 is raw 250.
func TestEncodeInvertsScaling(t *testing.T) {
	regs, err := encodeRegisters(mustFormat(t, "uint16"), 25.0, "", "", 0.1, 0)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if regs[0] != 250 {
		t.Errorf("raw = %d, want 250", regs[0])
	}
}

// Integer encodes refuse out-of-range raws instead of clamping — a clamped
// setpoint silently commanding something else is worse than a loud error.
func TestEncodeRangeErrors(t *testing.T) {
	cases := []struct {
		format string
		v      any
	}{
		{"int16", int64(40000)},
		{"int16", int64(-40000)},
		{"uint16", int64(-1)},
		{"uint16", int64(70000)},
		{"uint32", int64(-1)},
		{"int32", float64(3e9)},
	}
	for _, tc := range cases {
		if _, err := encodeRegisters(mustFormat(t, tc.format), tc.v, "", "", 0, 0); err == nil {
			t.Errorf("%s %v: want range error", tc.format, tc.v)
		}
	}
	if _, err := encodeRegisters(mustFormat(t, "int16"), "nope", "", "", 0, 0); err == nil {
		t.Error("string value: want error")
	}
}

func TestEncodeIntRounds(t *testing.T) {
	regs, err := encodeRegisters(mustFormat(t, "int16"), 41.6, "", "", 0, 0)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if int16(regs[0]) != 42 {
		t.Errorf("raw = %d, want 42", int16(regs[0]))
	}
}

// bit:N encodes to a lone bit; the driver read-modify-writes with setBit.
func TestEncodeBitAndSetBit(t *testing.T) {
	regs, err := encodeRegisters(mustFormat(t, "bit:5"), true, "", "", 0, 0)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if regs[0] != 1<<5 {
		t.Errorf("regs = %v", regs)
	}
	if got := setBit(0x00FF, 5, false); got != 0x00DF {
		t.Errorf("setBit clear = %04X", got)
	}
	if got := setBit(0x0000, 15, true); got != 0x8000 {
		t.Errorf("setBit set = %04X", got)
	}
}

func TestEncodeBoolFromNumber(t *testing.T) {
	regs, err := encodeRegisters(mustFormat(t, "bool"), int64(7), "", "", 0, 0)
	if err != nil || regs[0] != 1 {
		t.Fatalf("bool from 7: regs=%v err=%v", regs, err)
	}
}

func TestEncodeFloat64Golden(t *testing.T) {
	regs, err := encodeRegisters(mustFormat(t, "float64"), 1.5, "", "", 0, 0)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	bits := math.Float64bits(1.5)
	want := []uint16{uint16(bits >> 48), uint16(bits >> 32), uint16(bits >> 16), uint16(bits)}
	for i := range want {
		if regs[i] != want[i] {
			t.Fatalf("regs = %04X, want %04X", regs, want)
		}
	}
}

func TestDidYouMean(t *testing.T) {
	if s := didYouMean("FTIRI", []string{"FTIR_I", "SCR"}); !strings.Contains(s, "FTIR_I") {
		t.Errorf("didYouMean = %q", s)
	}
	if s := didYouMean("zzzz", []string{"FTIR_I", "SCR"}); s != "" {
		t.Errorf("far miss should suggest nothing, got %q", s)
	}
}
