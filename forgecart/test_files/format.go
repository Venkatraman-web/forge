package main

import (
	"fmt"
	"strings"
)

func fmtMS(p *float64) string {
	if p == nil {
		return "—"
	}
	if *p >= 1000 {
		return fmt.Sprintf("%.0f", *p)
	}
	return fmt.Sprintf("%.1f", *p)
}

func fmtF(p *float64) string {
	if p == nil {
		return "—"
	}
	return fmt.Sprintf("%.1f", *p)
}

// bar renders a text bar of width w for value v on a scale of max.
func bar(v, max float64, w int) string {
	if max <= 0 || v < 0 {
		return ""
	}
	n := int(v/max*float64(w) + 0.5)
	if n == 0 && v > 0 {
		n = 1
	}
	return strings.Repeat("█", n)
}

func mdEscape(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}
