// encode.go turns raw wire values ([]uint16 registers, coil bools) into the
// engineering values the tag store holds, and back for writes. Pure functions
// — no conn, no driver state — so every format × word order × byte order ×
// scaling combination is a table test.
package modbus

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Word and byte order tokens. The empty string means big — the Modbus
// default, and what most devices speak. "little" for word order is
// tentacle's reverseWords (the Anybus/Omron path); "little" for byte order
// is tentacle's reverseBits.
const (
	OrderBig    = "big"
	OrderLittle = "little"
)

// Format is a parsed binding format: how many registers (or one coil) a
// value occupies and how the bits mean a number. The zero value is invalid;
// use ParseFormat. Shared with plan.go, driver.go and the codegen — a
// binding's Format string is parsed once, at validation.
type Format struct {
	// Kind is one of "int16", "uint16", "int32", "uint32", "float32",
	// "float64", "bool", "bit".
	Kind string
	// Bit is the bit number for Kind "bit" (format string "bit:N", N 0..15).
	Bit int
}

// formatKinds is the accepted set, for validation and did-you-mean errors.
var formatKinds = []string{"int16", "uint16", "int32", "uint32", "float32", "float64", "bool", "bit"}

// ParseFormat parses a binding's format string: one of the fixed kinds, or
// "bit:N" for one bit of a 16-bit register.
func ParseFormat(s string) (Format, error) {
	if kind, n, ok := strings.Cut(s, ":"); ok {
		if kind != "bit" {
			return Format{}, fmt.Errorf("unknown format %q%s", s, didYouMean(kind, formatKinds))
		}
		bit, err := strconv.Atoi(n)
		if err != nil || bit < 0 || bit > 15 {
			return Format{}, fmt.Errorf("format %q: bit number must be 0..15", s)
		}
		return Format{Kind: "bit", Bit: bit}, nil
	}
	switch s {
	case "int16", "uint16", "int32", "uint32", "float32", "float64", "bool":
		return Format{Kind: s}, nil
	case "bit":
		return Format{}, fmt.Errorf(`format "bit" needs a bit number ("bit:0".."bit:15")`)
	}
	return Format{}, fmt.Errorf("unknown format %q%s", s, didYouMean(s, formatKinds))
}

// String renders the format back to its manifest spelling.
func (f Format) String() string {
	if f.Kind == "bit" {
		return "bit:" + strconv.Itoa(f.Bit)
	}
	return f.Kind
}

// Words is how many 16-bit registers the format occupies in a register
// table. In a coil/discrete table every binding is one bit (and only "bool"
// is valid there — plan validation enforces it), so Words also serves as the
// bit count.
func (f Format) Words() int {
	switch f.Kind {
	case "int32", "uint32", "float32":
		return 2
	case "float64":
		return 4
	default:
		return 1
	}
}

// integer reports whether the format is an integer kind — the kinds that
// deliver int64 when unscaled.
func (f Format) integer() bool {
	switch f.Kind {
	case "int16", "uint16", "int32", "uint32":
		return true
	}
	return false
}

// wireOrder normalizes registers from the binding's word/byte order to
// big-endian-everything, in a fresh slice (the input may be a shared read
// snapshot). Byte order swaps within each register; word order reverses the
// register sequence of a multi-word value.
func wireOrder(regs []uint16, wordOrder, byteOrder string) []uint16 {
	out := append([]uint16(nil), regs...)
	if byteOrder == OrderLittle {
		for i, r := range out {
			out[i] = r>>8 | r<<8
		}
	}
	if wordOrder == OrderLittle {
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	return out
}

// decodeRegisters turns the raw registers behind one binding into its tag
// value. len(regs) must be exactly f.Words(). Numeric results come back as
// int64 (integer kinds, unscaled) or float64 (float kinds, or anything with
// Scale/Offset applied) — the plain scalar shapes io.Values carries. bool
// and bit:N return bool and ignore scaling.
//
// Scaling is engineering = raw*Scale + Offset, with Scale 0 meaning 1 so an
// unset manifest field is the identity, not a zeroed tag.
func decodeRegisters(f Format, regs []uint16, wordOrder, byteOrder string, scale, offset float64) (any, error) {
	if len(regs) != f.Words() {
		return nil, fmt.Errorf("format %s needs %d registers, got %d", f, f.Words(), len(regs))
	}
	w := wireOrder(regs, wordOrder, byteOrder)
	switch f.Kind {
	case "bool":
		return w[0] != 0, nil
	case "bit":
		return w[0]&(1<<f.Bit) != 0, nil
	}

	var raw float64
	switch f.Kind {
	case "int16":
		raw = float64(int16(w[0]))
	case "uint16":
		raw = float64(w[0])
	case "int32":
		raw = float64(int32(uint32(w[0])<<16 | uint32(w[1])))
	case "uint32":
		raw = float64(uint32(w[0])<<16 | uint32(w[1]))
	case "float32":
		raw = float64(math.Float32frombits(uint32(w[0])<<16 | uint32(w[1])))
	case "float64":
		raw = math.Float64frombits(uint64(w[0])<<48 | uint64(w[1])<<32 | uint64(w[2])<<16 | uint64(w[3]))
	default:
		return nil, fmt.Errorf("unknown format %q", f.Kind)
	}
	if scale == 0 {
		scale = 1
	}
	if f.integer() && scale == 1 && offset == 0 {
		return int64(raw), nil
	}
	return raw*scale + offset, nil
}

// encodeRegisters is the write direction: a tag value to the raw registers
// for one binding, inverting the scaling (raw = (engineering − Offset) /
// Scale) and applying word/byte order. Integer kinds round to nearest and
// refuse values outside the format's range — a clamped setpoint silently
// commanding something else is worse than a loud error.
//
// Kind "bit" yields one register with only that bit set or clear; merging it
// into the register's other bits is the caller's job (the driver
// read-modify-writes; use setBit).
func encodeRegisters(f Format, v any, wordOrder, byteOrder string, scale, offset float64) ([]uint16, error) {
	switch f.Kind {
	case "bool", "bit":
		on, err := toBool(v)
		if err != nil {
			return nil, err
		}
		var w uint16
		if on {
			if f.Kind == "bit" {
				w = 1 << f.Bit
			} else {
				w = 1
			}
		}
		// wireOrder applies here too: a byte-order-little device keeps
		// bit 8..15 in the other byte, and the decode side normalizes —
		// so the encode side must denormalize or the bit lands wrong.
		return wireOrder([]uint16{w}, wordOrder, byteOrder), nil
	}

	eng, err := toFloat(v)
	if err != nil {
		return nil, err
	}
	if scale == 0 {
		scale = 1
	}
	raw := (eng - offset) / scale

	var w []uint16
	switch f.Kind {
	case "int16", "uint16", "int32", "uint32":
		r := math.Round(raw)
		lo, hi := intRange(f.Kind)
		if r < lo || r > hi {
			return nil, fmt.Errorf("value %v: raw %v out of %s range", v, r, f.Kind)
		}
		u := uint64(int64(r)) // sign-extends negatives; masked below
		switch f.Kind {
		case "int16", "uint16":
			w = []uint16{uint16(u)}
		default:
			w = []uint16{uint16(u >> 16), uint16(u)}
		}
	case "float32":
		b := math.Float32bits(float32(raw))
		w = []uint16{uint16(b >> 16), uint16(b)}
	case "float64":
		b := math.Float64bits(raw)
		w = []uint16{uint16(b >> 48), uint16(b >> 32), uint16(b >> 16), uint16(b)}
	default:
		return nil, fmt.Errorf("unknown format %q", f.Kind)
	}
	// wireOrder is its own inverse (a swap and a reversal), so the one
	// function serves both directions.
	return wireOrder(w, wordOrder, byteOrder), nil
}

// setBit merges one bit into an existing register word — the
// read-modify-write step for a writable bit:N binding.
func setBit(word uint16, bit int, on bool) uint16 {
	if on {
		return word | 1<<bit
	}
	return word &^ (1 << bit)
}

// intRange is the representable span per integer kind, as floats so the
// rounded raw value can be range-checked before conversion.
func intRange(kind string) (lo, hi float64) {
	switch kind {
	case "int16":
		return math.MinInt16, math.MaxInt16
	case "uint16":
		return 0, math.MaxUint16
	case "int32":
		return math.MinInt32, math.MaxInt32
	default: // uint32
		return 0, math.MaxUint32
	}
}

// toFloat accepts the numeric shapes the runtime hands a driver (io.Values
// scalars plus what tests write).
func toFloat(v any) (float64, error) {
	switch x := v.(type) {
	case float64:
		return x, nil
	case float32:
		return float64(x), nil
	case int64:
		return float64(x), nil
	case int:
		return float64(x), nil
	case uint16:
		return float64(x), nil
	case bool:
		if x {
			return 1, nil
		}
		return 0, nil
	}
	return 0, fmt.Errorf("cannot encode %T as a number", v)
}

// toBool accepts bool or any nonzero number — the same tolerance the tag
// store shows when ST assigns an INT expression to a BOOL output.
func toBool(v any) (bool, error) {
	if b, ok := v.(bool); ok {
		return b, nil
	}
	f, err := toFloat(v)
	if err != nil {
		return false, fmt.Errorf("cannot encode %T as a bool", v)
	}
	return f != 0, nil
}

// didYouMean suggests the nearest candidate within a two-edit distance —
// the same convention ir.didYouMean gives unknown struct members, so a
// manifest typo reads like every other nautilus typo.
func didYouMean(name string, candidates []string) string {
	best, bestD := "", 3
	for _, c := range candidates {
		if d := editDistance(strings.ToLower(name), strings.ToLower(c)); d < bestD {
			best, bestD = c, d
		}
	}
	if best == "" {
		return ""
	}
	return " (did you mean " + best + "?)"
}

// editDistance is plain Levenshtein — the inputs are format names and source
// ids, short enough that the O(n·m) table is nothing.
func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, min(cur[j-1]+1, prev[j-1]+cost))
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}
