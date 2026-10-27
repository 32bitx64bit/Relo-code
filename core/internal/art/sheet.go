package art

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"os"
	"path/filepath"

	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"

	"github.com/relocode/relo/internal/sprite"
)

// CandidateSheet lays a job's pictures out in a grid, each with its number in a corner, so an agent can look at all of
// them in one image and name one by number.
func CandidateSheet(job Job) ([]byte, error) {
	if len(job.Candidates) == 0 {
		return nil, fmt.Errorf("job %s has no pictures", job.ID)
	}
	cell := 512
	cols := 2
	switch n := len(job.Candidates); {
	case n == 1:
		cell, cols = 1024, 1
	case n > 4:
		cols = 3
	}
	const gutter = 8
	rows := (len(job.Candidates) + cols - 1) / cols
	sheet := image.NewNRGBA(image.Rect(0, 0, cols*cell+(cols+1)*gutter, rows*cell+(rows+1)*gutter))
	draw.Draw(sheet, sheet.Rect, image.NewUniform(color.NRGBA{40, 40, 44, 255}), image.Point{}, draw.Src)
	for i, c := range job.Candidates {
		data, err := os.ReadFile(filepath.Join(job.Dir, c.File))
		if err != nil {
			return nil, err
		}
		img, _, err := sprite.Decode(data)
		if err != nil {
			return nil, err
		}
		if img.Rect.Dx() > cell || img.Rect.Dy() > cell {
			frames, _, err := sprite.Run(img, []sprite.Op{{Op: "resize", MaxEdge: cell, Filter: "area"}}, sprite.Env{})
			if err != nil {
				return nil, err
			}
			img = frames[0]
		}
		x := gutter + (i%cols)*(cell+gutter) + (cell-img.Rect.Dx())/2
		y := gutter + (i/cols)*(cell+gutter) + (cell-img.Rect.Dy())/2
		draw.Draw(sheet, image.Rect(x, y, x+img.Rect.Dx(), y+img.Rect.Dy()), img, img.Rect.Min, draw.Over)
		drawLabel(sheet, x+6, y+6, fmt.Sprint(c.Index))
	}
	return sprite.EncodePNG(sheet)
}

// drawLabel draws a number large enough to read after the sheet is scaled down for a model: the built-in font
// enlarged four times, black on white.
func drawLabel(dst *image.NRGBA, x, y int, text string) {
	const scale = 4
	face := basicfont.Face7x13
	w := font.MeasureString(face, text).Ceil() + 4
	h := face.Metrics().Height.Ceil() + 4
	small := image.NewNRGBA(image.Rect(0, 0, w, h))
	draw.Draw(small, small.Rect, image.NewUniform(color.White), image.Point{}, draw.Src)
	d := font.Drawer{Dst: small, Src: image.NewUniform(color.Black), Face: face, Dot: fixed.P(2, h-4)}
	d.DrawString(text)
	for yy := 0; yy < h*scale; yy++ {
		for xx := 0; xx < w*scale; xx++ {
			if image.Pt(x+xx, y+yy).In(dst.Rect) {
				dst.SetNRGBA(x+xx, y+yy, small.NRGBAAt(xx/scale, yy/scale))
			}
		}
	}
}
