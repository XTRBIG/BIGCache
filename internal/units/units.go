// Package units parses and formats byte sizes.
package units

import (
	"fmt"
	"strconv"
	"strings"
)

// ParseSize parses a human readable byte size such as "4096", "64K", "1.5G",
// "20GiB" or "512MB". Suffixes are binary (K = 1024) regardless of whether
// they are written as "K", "KB" or "KiB".
func ParseSize(s string) (int64, error) {
	orig := s
	s = strings.TrimSpace(strings.ToUpper(s))
	if s == "" {
		return 0, fmt.Errorf("empty size")
	}
	s = strings.TrimSuffix(s, "IB")
	s = strings.TrimSuffix(s, "B")
	mult := int64(1)
	if n := len(s); n > 0 {
		switch s[n-1] {
		case 'K':
			mult = 1 << 10
		case 'M':
			mult = 1 << 20
		case 'G':
			mult = 1 << 30
		case 'T':
			mult = 1 << 40
		case 'P':
			mult = 1 << 50
		}
		if mult != 1 {
			s = s[:n-1]
		}
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("invalid size %q", orig)
	}
	if strings.ContainsAny(s, ".eE") {
		f, err := strconv.ParseFloat(s, 64)
		if err != nil || f < 0 {
			return 0, fmt.Errorf("invalid size %q", orig)
		}
		return int64(f * float64(mult)), nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("invalid size %q", orig)
	}
	if mult > 1 && v > (1<<63-1)/mult {
		return 0, fmt.Errorf("size %q overflows", orig)
	}
	return v * mult, nil
}

// FormatSize renders n as a short human readable string (binary units).
func FormatSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
