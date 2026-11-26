package k8s

import (
	"math"
	"strconv"
	"strings"
)

var binarySuffix = map[string]float64{
	"Ki": 1 << 10, "Mi": 1 << 20, "Gi": 1 << 30, "Ti": 1 << 40, "Pi": 1 << 50, "Ei": 1 << 60,
}

var decimalSuffix = map[string]float64{
	"n": 1e-9, "u": 1e-6, "m": 1e-3, "k": 1e3, "M": 1e6, "G": 1e9, "T": 1e12, "P": 1e15, "E": 1e18,
}

// parseQuantity converts a Kubernetes resource quantity ("250m", "128Mi", "1.5", "2G") to a float.
func parseQuantity(q string) (float64, bool) {
	q = strings.TrimSpace(q)
	if q == "" {
		return 0, false
	}
	for suffix, mult := range binarySuffix {
		if num, ok := strings.CutSuffix(q, suffix); ok {
			v, err := strconv.ParseFloat(num, 64)
			return v * mult, err == nil
		}
	}
	if last := q[len(q)-1:]; last >= "A" && last <= "z" && !strings.ContainsAny(q, "eE") {
		if mult, ok := decimalSuffix[last]; ok {
			v, err := strconv.ParseFloat(q[:len(q)-1], 64)
			return v * mult, err == nil
		}
	}
	v, err := strconv.ParseFloat(q, 64)
	return v, err == nil
}

// milliCPU parses a CPU quantity into millicores.
func milliCPU(q string) int64 {
	v, ok := parseQuantity(q)
	if !ok {
		return 0
	}
	return int64(math.Round(v * 1000))
}

// bytesOf parses a memory quantity into bytes.
func bytesOf(q string) int64 {
	v, ok := parseQuantity(q)
	if !ok {
		return 0
	}
	return int64(math.Round(v))
}
