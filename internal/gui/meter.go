package gui

import (
	"fmt"
	"image/color"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/widget"

	"github.com/adelinn/srt-control/internal/bridge"
)

// levelMeter draws stereo peak meters from -60 to 0 dBFS with green, amber and
// red zones, a dB readout per channel and a scale underneath. It stays dark in
// both themes, like a hardware meter.
type levelMeter struct {
	widget.BaseWidget
	db [bridge.Channels]float64
}

func newLevelMeter() *levelMeter {
	m := &levelMeter{db: [bridge.Channels]float64{-90, -90}}
	m.ExtendBaseWidget(m)
	return m
}

func (m *levelMeter) setLevels(db [bridge.Channels]float64) {
	if db != m.db {
		m.db = db
		m.Refresh()
	}
}

type meterZone struct {
	lo, hi   float64
	dim, lit color.NRGBA
}

var meterZones = []meterZone{
	{-60, -18, hex(0x1d4a2c), hex(0x34c759)},
	{-18, -6, hex(0x54460f), hex(0xf5c518)},
	{-6, 0, hex(0x581c18), hex(0xff453a)},
}

var meterTicks = []float64{-48, -36, -24, -18, -12, -6, -3}

type meterRenderer struct {
	m      *levelMeter
	bg     *canvas.Rectangle
	names  [bridge.Channels]*canvas.Text
	values [bridge.Channels]*canvas.Text
	dim    [bridge.Channels][]*canvas.Rectangle
	lit    [bridge.Channels][]*canvas.Rectangle
	ticks  []*canvas.Rectangle
	scale  []*canvas.Text
	all    []fyne.CanvasObject
}

func (m *levelMeter) CreateRenderer() fyne.WidgetRenderer {
	r := &meterRenderer{m: m, bg: canvas.NewRectangle(hex(0x15171b))}
	r.bg.CornerRadius = 8
	r.all = append(r.all, r.bg)
	for ch := 0; ch < bridge.Channels; ch++ {
		r.names[ch] = canvas.NewText([]string{"L", "R"}[ch], hex(0xc9ced6))
		r.names[ch].TextSize = 12
		r.names[ch].TextStyle.Bold = true
		r.values[ch] = canvas.NewText("", hex(0xe4e7eb))
		r.values[ch].TextSize = 12
		r.values[ch].TextStyle.Monospace = true
		r.all = append(r.all, r.names[ch], r.values[ch])
		for _, z := range meterZones {
			d, l := canvas.NewRectangle(z.dim), canvas.NewRectangle(z.lit)
			r.dim[ch] = append(r.dim[ch], d)
			r.lit[ch] = append(r.lit[ch], l)
			r.all = append(r.all, d, l)
		}
	}
	for _, db := range meterTicks {
		t := canvas.NewRectangle(color.NRGBA{A: 0xcc})
		r.ticks = append(r.ticks, t)
		s := canvas.NewText(fmt.Sprintf("%.0f", db), hex(0x7d8590))
		s.TextSize = 9
		r.scale = append(r.scale, s)
		r.all = append(r.all, t, s)
	}
	return r
}

const (
	meterPadX   = 12
	meterPadY   = 9
	meterNameW  = 16
	meterValueW = 70
	meterGap    = 5
	meterScaleH = 13
)

func (r *meterRenderer) MinSize() fyne.Size {
	return fyne.NewSize(360, 2*meterPadY+2*13+meterGap+meterScaleH)
}

func (r *meterRenderer) Layout(size fyne.Size) {
	r.bg.Resize(size)
	left := float32(meterPadX + meterNameW)
	right := size.Width - meterPadX - meterValueW
	span := right - left
	x := func(db float64) float32 {
		return left + span*float32((min(0, max(-60, db))+60)/60)
	}
	barH := (size.Height - 2*meterPadY - meterGap - meterScaleH) / 2
	for ch := 0; ch < bridge.Channels; ch++ {
		y0 := meterPadY + float32(ch)*(barH+meterGap)
		level := r.m.db[ch]
		n := r.names[ch].MinSize()
		r.names[ch].Move(fyne.NewPos(meterPadX, y0+(barH-n.Height)/2))
		for i, z := range meterZones {
			r.dim[ch][i].Move(fyne.NewPos(x(z.lo), y0))
			r.dim[ch][i].Resize(fyne.NewSize(x(z.hi)-x(z.lo), barH))
			lit := r.lit[ch][i]
			if level > z.lo {
				lit.Move(fyne.NewPos(x(z.lo), y0))
				lit.Resize(fyne.NewSize(x(min(level, z.hi))-x(z.lo), barH))
				lit.Show()
			} else {
				lit.Hide()
			}
		}
		label := "  -inf"
		if level > -60 {
			label = fmt.Sprintf("%5.1f dB", level)
		}
		r.values[ch].Text = label
		v := r.values[ch].MinSize()
		r.values[ch].Move(fyne.NewPos(right+10, y0+(barH-v.Height)/2))
	}
	barsBottom := meterPadY + 2*barH + meterGap
	for i, db := range meterTicks {
		r.ticks[i].Move(fyne.NewPos(x(db), meterPadY))
		r.ticks[i].Resize(fyne.NewSize(1, barsBottom-meterPadY))
		s := r.scale[i].MinSize()
		r.scale[i].Move(fyne.NewPos(x(db)-s.Width/2, barsBottom+1))
	}
}

func (r *meterRenderer) Refresh() {
	r.Layout(r.m.Size())
	canvas.Refresh(r.m)
}

func (r *meterRenderer) Objects() []fyne.CanvasObject { return r.all }
func (r *meterRenderer) Destroy()                     {}
