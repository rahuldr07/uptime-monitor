package web

import (
	"fmt"
	"html/template"
	"strings"
	"time"

	"uptimemon/internal/store"
)

// latencySVG renders a latency line chart for checks between from and to,
// shading incidents in red. Checks must be oldest first.
func latencySVG(checks []store.Check, incidents []store.Incident, from, to time.Time) template.HTML {
	const w, h, padL, padB, padT = 800.0, 200.0, 58.0, 22.0, 8.0
	if len(checks) == 0 {
		return template.HTML(`<svg viewBox="0 0 800 200" xmlns="http://www.w3.org/2000/svg"><text x="400" y="100" text-anchor="middle" fill="#9a9a94" font-size="14">No checks in this range</text></svg>`)
	}
	span := to.Sub(from).Seconds()
	if span <= 0 {
		span = 1
	}
	var maxMS int64 = 1
	for _, c := range checks {
		if c.OK && c.LatencyMS > maxMS {
			maxMS = c.LatencyMS
		}
	}
	maxMS = niceCeil(maxMS)
	x := func(t time.Time) float64 { return padL + (w-padL)*(t.Sub(from).Seconds()/span) }
	y := func(ms int64) float64 { return padT + (h-padB-padT)*(1-float64(ms)/float64(maxMS)) }

	var b strings.Builder
	fmt.Fprintf(&b, `<svg viewBox="0 0 %.0f %.0f" xmlns="http://www.w3.org/2000/svg" role="img" aria-label="Latency chart">`, w, h)
	// incident shading
	for _, in := range incidents {
		s, e := in.StartedAt, to
		if in.EndedAt != nil {
			e = *in.EndedAt
		}
		if e.Before(from) || s.After(to) {
			continue
		}
		if s.Before(from) {
			s = from
		}
		if e.After(to) {
			e = to
		}
		x0, x1 := x(s), x(e)
		if x1-x0 < 2 {
			x1 = x0 + 2
		}
		fmt.Fprintf(&b, `<rect x="%.1f" y="%.0f" width="%.1f" height="%.0f" fill="#e24b4a" opacity="0.18"/>`, x0, padT, x1-x0, h-padB-padT)
	}
	// gridlines + labels
	for i := 0; i <= 4; i++ {
		ms := maxMS * int64(i) / 4
		yy := y(ms)
		fmt.Fprintf(&b, `<line x1="%.0f" y1="%.1f" x2="%.0f" y2="%.1f" stroke="#9a9a94" stroke-opacity="0.25"/>`, padL, yy, w, yy)
		label := fmt.Sprintf("%d", ms)
		if i == 4 {
			label += " ms"
		}
		fmt.Fprintf(&b, `<text x="%.0f" y="%.1f" text-anchor="end" fill="#9a9a94" font-size="11">%s</text>`, padL-6, yy+4, label)
	}
	// time labels
	for i := 0; i <= 4; i++ {
		t := from.Add(time.Duration(float64(to.Sub(from)) * float64(i) / 4))
		anchor := "middle"
		if i == 0 {
			anchor = "start"
		} else if i == 4 {
			anchor = "end"
		}
		label := t.Local().Format("15:04")
		if to.Sub(from) > 36*time.Hour {
			label = t.Local().Format("Jan 2")
		}
		fmt.Fprintf(&b, `<text x="%.1f" y="%.0f" text-anchor="%s" fill="#9a9a94" font-size="11">%s</text>`, x(t), h-6, anchor, label)
	}
	// line segments: break the line at failed checks and draw them as marks
	var pts []string
	flush := func() {
		if len(pts) == 1 {
			b.WriteString(`<circle r="2" fill="#378add" cx="` + strings.Split(pts[0], ",")[0] + `" cy="` + strings.Split(pts[0], ",")[1] + `"/>`)
		} else if len(pts) > 1 {
			fmt.Fprintf(&b, `<polyline fill="none" stroke="#378add" stroke-width="1.5" points="%s"/>`, strings.Join(pts, " "))
		}
		pts = pts[:0]
	}
	for _, c := range downsample(checks, 600) {
		if !c.OK {
			flush()
			fmt.Fprintf(&b, `<line x1="%.1f" y1="%.0f" x2="%.1f" y2="%.0f" stroke="#e24b4a" stroke-width="1.5"/>`, x(c.CheckedAt), h-padB-6, x(c.CheckedAt), h-padB)
			continue
		}
		pts = append(pts, fmt.Sprintf("%.1f,%.1f", x(c.CheckedAt), y(c.LatencyMS)))
	}
	flush()
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

func niceCeil(v int64) int64 {
	steps := []int64{50, 100, 200, 250, 500, 1000, 2000, 5000, 10000, 20000, 30000, 60000}
	for _, s := range steps {
		if v <= s {
			return s
		}
	}
	return (v/10000 + 1) * 10000
}

// downsample keeps at most n checks, always keeping failures so outages
// never disappear from the chart.
func downsample(checks []store.Check, n int) []store.Check {
	if len(checks) <= n {
		return checks
	}
	step := float64(len(checks)) / float64(n)
	out := make([]store.Check, 0, n+len(checks)/10)
	next := 0.0
	for i, c := range checks {
		if !c.OK || float64(i) >= next {
			out = append(out, c)
			if float64(i) >= next {
				next += step
			}
		}
	}
	return out
}

// sparkSVG draws a tiny latency sparkline for the overview rows. Failed
// checks are drawn as red ticks so outages stay visible at this size.
func sparkSVG(checks []store.Check, from, to time.Time) template.HTML {
	const w, h = 100.0, 24.0
	if len(checks) == 0 {
		return template.HTML(`<svg viewBox="0 0 100 24" width="100" height="24" aria-hidden="true"><line x1="0" y1="12" x2="100" y2="12" stroke="#9a9a94" stroke-opacity=".3" stroke-dasharray="2 3"/></svg>`)
	}
	span := to.Sub(from).Seconds()
	if span <= 0 {
		span = 1
	}
	var maxMS int64 = 1
	for _, c := range checks {
		if c.OK && c.LatencyMS > maxMS {
			maxMS = c.LatencyMS
		}
	}
	x := func(t time.Time) float64 { return w * (t.Sub(from).Seconds() / span) }
	y := func(ms int64) float64 { return 2 + (h-4)*(1-float64(ms)/float64(maxMS)) }
	var b strings.Builder
	fmt.Fprintf(&b, `<svg viewBox="0 0 %.0f %.0f" width="%.0f" height="%.0f" aria-hidden="true">`, w, h, w, h)
	var pts []string
	flush := func() {
		if len(pts) == 1 {
			xy := strings.Split(pts[0], ",")
			fmt.Fprintf(&b, `<circle r="1.5" fill="#378add" cx="%s" cy="%s"/>`, xy[0], xy[1])
		} else if len(pts) > 1 {
			fmt.Fprintf(&b, `<polyline fill="none" stroke="#378add" stroke-width="1.3" points="%s"/>`, strings.Join(pts, " "))
		}
		pts = pts[:0]
	}
	for _, c := range downsample(checks, 60) {
		if !c.OK {
			flush()
			fmt.Fprintf(&b, `<line x1="%.1f" y1="2" x2="%.1f" y2="%.0f" stroke="#e24b4a" stroke-width="1.5"/>`, x(c.CheckedAt), x(c.CheckedAt), h-2)
			continue
		}
		pts = append(pts, fmt.Sprintf("%.1f,%.1f", x(c.CheckedAt), y(c.LatencyMS)))
	}
	flush()
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}
