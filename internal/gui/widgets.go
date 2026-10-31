package gui

import (
	"image/color"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

func variant() fyne.ThemeVariant { return fyne.CurrentApp().Settings().ThemeVariant() }

func themeColor(n fyne.ThemeColorName) color.Color {
	return fyne.CurrentApp().Settings().Theme().Color(n, variant())
}

// panel is a raised, rounded section with a small uppercase title and an
// optional widget on the right of the title row.
type panel struct {
	widget.BaseWidget
	content fyne.CanvasObject
}

// mutedText is small secondary text (hints, section titles) in the theme's
// placeholder color, which stays readable where LowImportance labels don't.
type mutedText struct {
	*widget.RichText
	seg *widget.TextSegment
}

func newMutedText(text string, bold bool) *mutedText {
	seg := &widget.TextSegment{Text: text, Style: widget.RichTextStyle{
		ColorName: theme.ColorNamePlaceHolder,
		SizeName:  theme.SizeNameCaptionText,
		TextStyle: fyne.TextStyle{Bold: bold},
		Inline:    true,
	}}
	return &mutedText{RichText: widget.NewRichText(seg), seg: seg}
}

func (m *mutedText) SetText(text string) {
	if m.seg.Text != text {
		m.seg.Text = text
		m.Refresh()
	}
}

// newPanel makes a section. The title row holds the title, an optional status
// light and an optional widget on the right.
func newPanel(title string, status *statusLight, trailing, content fyne.CanvasObject) *panel {
	if title != "" {
		gap := canvas.NewRectangle(color.Transparent)
		gap.SetMinSize(fyne.NewSize(6, 0))
		head := container.NewHBox(newMutedText(strings.ToUpper(title), true), gap)
		var middle fyne.CanvasObject
		if status != nil {
			middle = status.box
		}
		row := container.NewBorder(nil, nil, head, trailing, middle)
		content = container.NewBorder(row, nil, nil, nil, content)
	}
	p := &panel{content: content}
	p.ExtendBaseWidget(p)
	return p
}

func (p *panel) CreateRenderer() fyne.WidgetRenderer {
	bg := canvas.NewRectangle(themeColor(colorPanel))
	bg.CornerRadius = 10
	bg.StrokeWidth = 1
	bg.StrokeColor = themeColor(colorPanelBorder)
	return &panelRenderer{p: p, bg: bg}
}

type panelRenderer struct {
	p  *panel
	bg *canvas.Rectangle
}

const panelPad = 10

func (r *panelRenderer) Layout(size fyne.Size) {
	r.bg.Resize(size)
	r.p.content.Move(fyne.NewPos(panelPad, panelPad-4))
	r.p.content.Resize(size.Subtract(fyne.NewSize(2*panelPad, 2*panelPad-6)))
}

func (r *panelRenderer) MinSize() fyne.Size {
	return r.p.content.MinSize().Add(fyne.NewSize(2*panelPad, 2*panelPad-6))
}

func (r *panelRenderer) Refresh() {
	r.bg.FillColor = themeColor(colorPanel)
	r.bg.StrokeColor = themeColor(colorPanelBorder)
	r.bg.Refresh()
	r.p.content.Refresh()
}

func (r *panelRenderer) Objects() []fyne.CanvasObject { return []fyne.CanvasObject{r.bg, r.p.content} }
func (r *panelRenderer) Destroy()                     {}

// minWidth gives its only child at least w points of width.
type minWidth struct{ w float32 }

func (m minWidth) MinSize(objs []fyne.CanvasObject) fyne.Size {
	s := objs[0].MinSize()
	return fyne.NewSize(max(s.Width, m.w), s.Height)
}

func (m minWidth) Layout(objs []fyne.CanvasObject, size fyne.Size) {
	objs[0].Move(fyne.NewPos(0, 0))
	objs[0].Resize(size)
}

func withWidth(o fyne.CanvasObject, w float32) *fyne.Container { return container.New(minWidth{w}, o) }

// Component states grouped by how they are shown.
var (
	goodStates = map[string]bool{"receiving": true, "capturing": true, "playing": true, "sending": true}
	waitStates = map[string]bool{"connecting": true, "reconnecting": true, "buffering": true, "no audio": true}
)

func stateColor(state string) color.Color {
	switch {
	case goodStates[state]:
		return themeColor(theme.ColorNameSuccess)
	case waitStates[state]:
		return themeColor(theme.ColorNameWarning)
	case state == "error":
		return themeColor(theme.ColorNameError)
	}
	return themeColor(theme.ColorNameDisabled)
}

// statusLight is a colored dot with a "state — detail" line, shown in a
// panel's title row.
type statusLight struct {
	dot  *canvas.Circle
	text *widget.Label
	box  fyne.CanvasObject
}

func newStatusLight() *statusLight {
	l := &statusLight{dot: canvas.NewCircle(themeColor(theme.ColorNameDisabled)), text: widget.NewLabel("off")}
	l.text.SizeName = theme.SizeNameCaptionText
	l.text.Truncation = fyne.TextTruncateEllipsis
	dot := container.NewCenter(container.NewGridWrap(fyne.NewSize(9, 9), l.dot))
	l.box = container.NewBorder(nil, nil, dot, nil, l.text)
	return l
}

func (l *statusLight) set(state, detail string) {
	l.dot.FillColor = stateColor(state)
	l.dot.Refresh()
	text := state
	if detail != "" {
		text += " — " + detail
	}
	if l.text.Text != text {
		l.text.SetText(text)
	}
}

// stateBadge is the pill next to Start/Stop showing the overall state.
type stateBadge struct {
	widget.BaseWidget
	text  string
	color fyne.ThemeColorName
}

func newStateBadge() *stateBadge {
	b := &stateBadge{text: "STOPPED", color: theme.ColorNameDisabled}
	b.ExtendBaseWidget(b)
	return b
}

func (b *stateBadge) set(text string, c fyne.ThemeColorName) {
	if b.text != text || b.color != c {
		b.text, b.color = text, c
		b.Refresh()
	}
}

func (b *stateBadge) CreateRenderer() fyne.WidgetRenderer {
	bg := canvas.NewRectangle(color.Transparent)
	dot := canvas.NewCircle(color.White)
	txt := canvas.NewText("", color.White)
	txt.TextStyle = fyne.TextStyle{Bold: true}
	txt.TextSize = 11
	r := &badgeRenderer{b: b, bg: bg, dot: dot, txt: txt}
	r.Refresh()
	return r
}

type badgeRenderer struct {
	b   *stateBadge
	bg  *canvas.Rectangle
	dot *canvas.Circle
	txt *canvas.Text
}

func (r *badgeRenderer) MinSize() fyne.Size {
	t := r.txt.MinSize()
	return fyne.NewSize(t.Width+34, 26)
}

func (r *badgeRenderer) Layout(size fyne.Size) {
	h := float32(26)
	y := (size.Height - h) / 2
	r.bg.Move(fyne.NewPos(0, y))
	r.bg.Resize(fyne.NewSize(size.Width, h))
	r.bg.CornerRadius = h / 2
	r.dot.Move(fyne.NewPos(11, y+h/2-4))
	r.dot.Resize(fyne.NewSize(8, 8))
	t := r.txt.MinSize()
	r.txt.Move(fyne.NewPos(24, y+(h-t.Height)/2))
	r.txt.Resize(t)
}

func (r *badgeRenderer) Refresh() {
	c := themeColor(r.b.color)
	nc := color.NRGBAModel.Convert(c).(color.NRGBA)
	bg := nc
	bg.A = 0x2e // tinted background, solid dot and text
	r.bg.FillColor = bg
	r.dot.FillColor = c
	r.txt.Color = c
	if r.b.color == theme.ColorNameDisabled {
		r.txt.Color = themeColor(theme.ColorNameForeground)
	}
	r.txt.Text = r.b.text
	r.Layout(r.b.Size())
	canvas.Refresh(r.b)
}

func (r *badgeRenderer) Objects() []fyne.CanvasObject {
	return []fyne.CanvasObject{r.bg, r.dot, r.txt}
}
func (r *badgeRenderer) Destroy() {}
