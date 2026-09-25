// Command chart renders loadgen overtime's CSV (elapsed_s,ops_in_interval,
// ops_per_sec) as a self-contained SVG line chart.
//
// It exists so the throughput-over-time curve -- T8.1 calls it "the most
// interesting graph in the entire project" -- is generated from data rather
// than drawn by hand, and so reproducing it does not require a Python
// plotting stack the rest of the project has no other use for.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
)

type point struct {
	elapsed   float64
	opsPerSec float64
}

func main() {
	in := flag.String("in", "", "CSV file from loadgen overtime; defaults to stdin")
	out := flag.String("out", "", "SVG output path; defaults to stdout")
	title := flag.String("title", "Throughput over time", "chart title")
	flag.Parse()

	points, err := readPoints(*in)
	if err != nil {
		fmt.Fprintf(os.Stderr, "chart: %v\n", err)
		os.Exit(1)
	}
	if len(points) == 0 {
		fmt.Fprintln(os.Stderr, "chart: no data points")
		os.Exit(1)
	}

	svg := render(points, *title)

	w := os.Stdout
	if *out != "" {
		f, err := os.Create(*out)
		if err != nil {
			fmt.Fprintf(os.Stderr, "chart: %v\n", err)
			os.Exit(1)
		}
		defer func() { _ = f.Close() }()
		w = f
	}
	if _, err := fmt.Fprint(w, svg); err != nil {
		fmt.Fprintf(os.Stderr, "chart: write output: %v\n", err)
		os.Exit(1)
	}
}

func readPoints(path string) ([]point, error) {
	var r *bufio.Scanner
	if path == "" {
		r = bufio.NewScanner(os.Stdin)
	} else {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer func() { _ = f.Close() }()
		r = bufio.NewScanner(f)
	}

	var points []point
	first := true
	for r.Scan() {
		line := strings.TrimSpace(r.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if first {
			first = false
			if strings.HasPrefix(line, "elapsed_s") {
				continue // header row
			}
		}
		fields := strings.Split(line, ",")
		if len(fields) < 3 {
			continue
		}
		elapsed, err1 := strconv.ParseFloat(fields[0], 64)
		opsPerSec, err2 := strconv.ParseFloat(fields[2], 64)
		if err1 != nil || err2 != nil {
			continue
		}
		points = append(points, point{elapsed: elapsed, opsPerSec: opsPerSec})
	}
	return points, r.Err()
}

// render draws a simple line chart. No external library: an SVG is text, and
// a line chart is a handful of coordinates, which the standard library is
// entirely sufficient for.
func render(points []point, title string) string {
	const width, height = 900, 400
	const padLeft, padRight, padTop, padBottom = 70, 20, 40, 40
	plotW := float64(width - padLeft - padRight)
	plotH := float64(height - padTop - padBottom)

	maxOps := 0.0
	maxElapsed := 0.0
	for _, p := range points {
		if p.opsPerSec > maxOps {
			maxOps = p.opsPerSec
		}
		if p.elapsed > maxElapsed {
			maxElapsed = p.elapsed
		}
	}
	if maxOps == 0 {
		maxOps = 1
	}
	if maxElapsed == 0 {
		maxElapsed = 1
	}

	x := func(elapsed float64) float64 { return padLeft + (elapsed/maxElapsed)*plotW }
	y := func(ops float64) float64 { return padTop + plotH - (ops/maxOps)*plotH }

	var path strings.Builder
	for i, p := range points {
		cmd := "L"
		if i == 0 {
			cmd = "M"
		}
		fmt.Fprintf(&path, "%s%.1f,%.1f ", cmd, x(p.elapsed), y(p.opsPerSec))
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d" font-family="monospace" font-size="12">`+"\n",
		width, height, width, height)
	fmt.Fprintf(&sb, `<rect width="%d" height="%d" fill="white"/>`+"\n", width, height)
	fmt.Fprintf(&sb, `<text x="%d" y="20" font-size="16" fill="black">%s</text>`+"\n", padLeft, title)

	// Axes.
	fmt.Fprintf(&sb, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="black" stroke-width="1"/>`+"\n",
		float64(padLeft), float64(padTop), float64(padLeft), float64(height-padBottom))
	fmt.Fprintf(&sb, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="black" stroke-width="1"/>`+"\n",
		float64(padLeft), float64(height-padBottom), float64(width-padRight), float64(height-padBottom))

	// Y-axis labels: 0, max/2, max.
	for i := 0; i <= 2; i++ {
		v := maxOps * float64(i) / 2
		yy := y(v)
		fmt.Fprintf(&sb, `<text x="5" y="%.1f" fill="black">%.0f</text>`+"\n", yy+4, v)
		fmt.Fprintf(&sb, `<line x1="%d" y1="%.1f" x2="%d" y2="%.1f" stroke="#ddd" stroke-width="1"/>`+"\n",
			padLeft, yy, width-padRight, yy)
	}

	// X-axis labels: 0, max/2, max seconds.
	for i := 0; i <= 4; i++ {
		v := maxElapsed * float64(i) / 4
		fmt.Fprintf(&sb, `<text x="%.1f" y="%d" fill="black" text-anchor="middle">%.0fs</text>`+"\n",
			x(v), height-padBottom+20, v)
	}

	fmt.Fprintf(&sb, `<polyline points="%s" fill="none" stroke="#2563eb" stroke-width="2"/>`+"\n", strings.TrimSpace(path.String()))
	fmt.Fprintf(&sb, `<text x="%d" y="%d" fill="black">ops/sec</text>`+"\n", 5, padTop-10)
	sb.WriteString("</svg>\n")
	return sb.String()
}
