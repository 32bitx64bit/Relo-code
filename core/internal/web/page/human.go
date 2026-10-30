package page

import (
	"context"
	"math"
	"math/rand/v2"
	"time"
)

// Point is a viewport coordinate in CSS pixels.
type Point struct{ X, Y float64 }

// pacer holds the human-pacing scale. Scale 1 is human speed; 0 removes all waits
// (used by tests). Randomness is injected so tests can be deterministic.
type pacer struct {
	scale float64
	rnd   *rand.Rand
}

func newPacer() *pacer {
	return &pacer{scale: 1, rnd: rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64()))}
}

// pause sleeps base plus a random share of jitter, scaled by the pacing scale.
// It returns early with ctx.Err() if ctx ends.
func (p *pacer) pause(ctx context.Context, base, jitter time.Duration) error {
	d := base + time.Duration(p.rnd.Float64()*float64(jitter))
	d = time.Duration(float64(d) * p.scale)
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// keyDelay is the gap between two keystrokes. Cadence varies per key; a space or
// punctuation mark adds a longer hesitation, as a typist would.
func (p *pacer) keyDelay(r rune) time.Duration {
	d := 70*time.Millisecond + time.Duration(p.rnd.Float64()*float64(110*time.Millisecond))
	switch r {
	case ' ', '.', ',', '!', '?', '\n':
		d += time.Duration(p.rnd.Float64() * float64(180*time.Millisecond))
	}
	return d
}

// wheelSteps splits a scroll of total pixels into uneven wheel notches. Each step is
// between 60 and 140 pixels, except the last one, which takes the remainder. The
// sign of total is kept.
func (p *pacer) wheelSteps(total int) []int {
	if total == 0 {
		return nil
	}
	sign := 1
	n := total
	if n < 0 {
		sign, n = -1, -n
	}
	var steps []int
	for n > 0 {
		s := 60 + p.rnd.IntN(81)
		if s > n {
			s = n
		}
		steps = append(steps, sign*s)
		n -= s
	}
	return steps
}

// mousePath returns a curved path from a to b with n+1 points, starting at a and
// ending at b. Two random control points bow the path off the straight line, and
// the spacing eases in and out, so the motion is not a straight, constant-speed
// jump.
func (p *pacer) mousePath(a, b Point, n int) []Point {
	if n < 1 {
		n = 1
	}
	dx, dy := b.X-a.X, b.Y-a.Y
	length := math.Hypot(dx, dy)
	// Perpendicular unit vector; bow magnitude scales with distance but is capped.
	nx, ny := 0.0, 0.0
	if length > 0 {
		nx, ny = -dy/length, dx/length
	}
	bow := math.Min(length*0.25, 120)
	off1 := (p.rnd.Float64()*2 - 1) * bow
	off2 := (p.rnd.Float64()*2 - 1) * bow
	c1 := Point{a.X + dx/3 + nx*off1, a.Y + dy/3 + ny*off1}
	c2 := Point{a.X + 2*dx/3 + nx*off2, a.Y + 2*dy/3 + ny*off2}

	pts := make([]Point, 0, n+1)
	for i := 0; i <= n; i++ {
		t := float64(i) / float64(n)
		t = t * t * (3 - 2*t) // smoothstep: slow start and finish
		pts = append(pts, cubic(a, c1, c2, b, t))
	}
	pts[0] = a
	pts[len(pts)-1] = b
	return pts
}

// cubic evaluates a cubic Bezier curve at t in [0,1].
func cubic(p0, p1, p2, p3 Point, t float64) Point {
	u := 1 - t
	w0, w1, w2, w3 := u*u*u, 3*u*u*t, 3*u*t*t, t*t*t
	return Point{
		X: w0*p0.X + w1*p1.X + w2*p2.X + w3*p3.X,
		Y: w0*p0.Y + w1*p1.Y + w2*p2.Y + w3*p3.Y,
	}
}

// stepCount picks how many mouse-move events a path of this length gets.
func stepCount(a, b Point) int {
	n := int(math.Hypot(b.X-a.X, b.Y-a.Y) / 20)
	return min(max(n, 8), 40)
}
