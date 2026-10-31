package gui

import "fyne.io/fyne/v2"

// vstack stacks children vertically with a gap; the last child takes the
// remaining height (used for the log).
type vstack struct{ gap float32 }

func (v vstack) MinSize(objs []fyne.CanvasObject) fyne.Size {
	var s fyne.Size
	for i, o := range objs {
		m := o.MinSize()
		s.Width = max(s.Width, m.Width)
		s.Height += m.Height
		if i > 0 {
			s.Height += v.gap
		}
	}
	return s
}

func (v vstack) Layout(objs []fyne.CanvasObject, size fyne.Size) {
	y := float32(0)
	for i, o := range objs {
		h := o.MinSize().Height
		if i == len(objs)-1 {
			h = max(h, size.Height-y)
		}
		o.Move(fyne.NewPos(0, y))
		o.Resize(fyne.NewSize(size.Width, h))
		y += h + v.gap
	}
}

// columns lays children out side by side in equal widths and equal heights.
type columns struct{ gap float32 }

func (c columns) MinSize(objs []fyne.CanvasObject) fyne.Size {
	var w, h float32
	for _, o := range objs {
		m := o.MinSize()
		w = max(w, m.Width)
		h = max(h, m.Height)
	}
	n := float32(len(objs))
	return fyne.NewSize(w*n+c.gap*(n-1), h)
}

func (c columns) Layout(objs []fyne.CanvasObject, size fyne.Size) {
	n := float32(len(objs))
	w := (size.Width - c.gap*(n-1)) / n
	for i, o := range objs {
		o.Move(fyne.NewPos(float32(i)*(w+c.gap), 0))
		o.Resize(fyne.NewSize(w, size.Height))
	}
}
