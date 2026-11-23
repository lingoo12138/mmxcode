package webui

import (
	"fmt"
	"math"
	"time"

	"github.com/lingoo12138/mmxcode/internal/store"
)

// 每天消耗的那张图。
//
// 图是服务端画的：没有脚本的时候它也在（一条线，几个点）。脚本添的是那
// 层交互 —— 十字线、浮着的数、点一天看那天的明细。
//
// 坐标是算出来的，不是量出来的：图按固定的尺寸画好，再由 CSS 拉宽，线
// 跟着走。

// The size of the drawing.
//
// 图是画在一张绷开的皮上的：那张皮按容器的大小拉（preserveAspectRatio
// 是不拉伸的 none），所以线永远填满那么宽。字不在皮上 —— 皮上的字跟着皮
// 一起变形，标签就变成了被拉长的字。字是另外一层，一像素一像素摆上去的。
//
// 所以这里的坐标只有一份用处：算出百分比。百分比是那层字的位置，也是那
// 些点和那条线的位置。
const (
	chartW    = 720
	chartH    = 240
	chartPadL = 46
	chartPadR = 14
	// chartTop is the top of the plot area.
	chartTop = 26
	// chartBase is the bottom of the plot area: the x axis sits on it.
	chartBase = 214
	// chartXLabel is where the labels under the x axis sit.
	chartXLabel = 226
)

// ChartDay is one day on the drawing.
type ChartDay struct {
	// Day is the site's day: 2026-11-23.
	Day string
	// Used is how much was used that day.
	Used float64
	// Accounts is how many accounts had a row that day.
	Accounts int
	// X is where the day's point is, in the drawing's own units.
	X float64
	// Y is how high the day's point is.
	Y float64
	// X0 is where the day's band starts: the band is the day's hit
	// target, and it is wider than the point.
	X0 float64
	// W is how wide the day's band is.
	W float64
	// Left and Top are where the day's point is, in percent of the
	// drawing. The floating number hangs off these.
	Left float64
	Top  float64
	// Short is the day's short name: 11-23.
	Short string
}

// ChartTick is a tick on the y axis.
type ChartTick struct {
	// Label is the number on the tick.
	Label string
	// Y is where the tick is.
	Y float64
	// Left and Top are where the number sits, in percent.
	Left float64
	Top  float64
}

// ChartLabel is a piece of text on the drawing.
type ChartLabel struct {
	// Text is what it says.
	Text string
	// Anchor says where the text hangs off: start, middle or end.
	Anchor string
	// Left and Top are where it sits, in percent.
	Left float64
	Top  float64
}

// ChartMark is a point on the line that is drawn and labelled.
type ChartMark struct {
	// Label is the number beside the point.
	Label string
	// Peak says this point is the one that used the most.
	Peak bool
	// Left and Top are where the point is, in percent.
	Left float64
	Top  float64
	// Anchor says where the number hangs off the point: middle or end.
	Anchor string
}

// Chart is the drawing of the daily consumption.
type Chart struct {
	// Days are the days, in order.
	Days []ChartDay
	// Line is the path of the line.
	Line string
	// Area is the path of the wash under the line.
	Area string
	// Ticks are the ticks on the y axis.
	Ticks []ChartTick
	// XLabels are the labels under the x axis.
	XLabels []ChartLabel
	// Marks are the points that are drawn and labelled.
	Marks []ChartMark
	// Empty says there is nothing to draw: no day has anything on it.
	Empty bool
	// W and H are the size of the drawing.
	W float64
	H float64
	// Left, Right, Top and Bottom are the edges of the plot area.
	Left   float64
	Right  float64
	Top    float64
	Bottom float64
}

// buildChart draws the days.
//
// 一天一个点。有一天什么都没有，它就是零 —— 不是没有那一天。
func buildChart(totals []store.DayTotal, from, to string) Chart {
	var c Chart
	byDay := map[string]store.DayTotal{}
	for _, t := range totals {
		byDay[t.Day] = t
	}
	for _, day := range daysBetween(from, to) {
		t := byDay[day]
		c.Days = append(c.Days, ChartDay{
			Day:      day,
			Used:     t.Used,
			Accounts: t.Accounts,
			Short:    day[5:],
		})
	}
	if len(c.Days) == 0 {
		c.Empty = true
		return c
	}

	top := 0.0
	for _, d := range c.Days {
		if d.Used > top {
			top = d.Used
		}
	}
	if top <= 0 {
		// 一天都没有用掉：图上是一条贴着底的直线。那条线什么也没说，
		// 不如说清楚还没有账。
		c.Empty = true
		return c
	}
	top = niceMax(top)

	plotW := float64(chartW - chartPadL - chartPadR)
	plotH := float64(chartBase - chartTop)
	c.W, c.H = chartW, chartH
	c.Left, c.Right = chartPadL, chartW-chartPadR
	c.Top, c.Bottom = chartTop, chartBase
	step := plotW
	if len(c.Days) > 1 {
		step = plotW / float64(len(c.Days)-1)
	}
	peak := 0
	for i := range c.Days {
		d := &c.Days[i]
		d.X = float64(chartPadL) + step*float64(i)
		d.Y = chartBase - plotH*(d.Used/top)
		if d.Used > c.Days[peak].Used {
			peak = i
		}
		// 那一段是那一天的「地方」：指针落在它哪一块，就是哪一天。
		// 它比那个点宽，所以不必瞄准那个两像素的点。
		d.X0 = d.X - step/2
		d.W = step
		if d.X0 < float64(chartPadL) {
			d.X0 = float64(chartPadL)
		}
		if d.X0+d.W > float64(chartW-chartPadR) {
			d.W = float64(chartW-chartPadR) - d.X0
		}
		d.Left = pctX(d.X)
		d.Top = pctY(d.Y)
	}

	c.Line = linePath(c.Days)
	c.Area = areaPath(c.Days, chartBase)

	// 纵轴：三根线（底、中、顶）。线是实的、细的、退到后面去。
	for _, frac := range []float64{0, 0.5, 1} {
		v := top * frac
		y := chartBase - plotH*frac
		c.Ticks = append(c.Ticks, ChartTick{
			Label: tickLabel(v),
			Y:     y,
			Left:  pctX(chartPadL),
			Top:   pctY(y),
		})
	}

	// 横轴：头、中、尾三个日子。不是每一天都写 —— 三十个标签挤在一
	// 起，谁也看不清。
	for _, i := range []int{0, len(c.Days) / 2, len(c.Days) - 1} {
		if i < 0 || i >= len(c.Days) {
			continue
		}
		d := c.Days[i]
		anchor := "middle"
		if i == 0 {
			anchor = "start"
		}
		if i == len(c.Days)-1 {
			anchor = "end"
		}
		c.XLabels = append(c.XLabels, ChartLabel{
			Text:   d.Short,
			Anchor: anchor,
			Left:   pctX(d.X),
			Top:    pctY(chartXLabel),
		})
	}

	// 数字只标两处：最后一天（今天）和用得最多的那天。它们挨得近就
	// 只标一个 —— 两个数字挤在一起，谁也读不出来。
	last := len(c.Days) - 1
	c.Marks = append(c.Marks, ChartMark{
		Label:  tickLabel(c.Days[last].Used),
		Left:   pctX(c.Days[last].X),
		Top:    pctY(c.Days[last].Y),
		Anchor: "end",
	})
	if peak != last {
		c.Marks = append(c.Marks, ChartMark{
			Label:  tickLabel(c.Days[peak].Used),
			Peak:   true,
			Left:   pctX(c.Days[peak].X),
			Top:    pctY(c.Days[peak].Y),
			Anchor: anchorFor(c.Days[peak].X),
		})
	}
	return c
}

// pctX says where a point on the x axis is, in percent of the drawing.
//
// 那层字是按百分比摆的：容器多宽，字都在同一个地方等着，不会被皮拉着
// 变形。
func pctX(x float64) float64 {
	return x / float64(chartW) * 100
}

// pctY says where a point on the y axis is, in percent of the drawing.
func pctY(y float64) float64 {
	return y / float64(chartH) * 100
}

// anchorFor says how a number hangs off its point.
//
// 贴着边上的那个点，数字要往里让：不然它就掉出图外去了。
func anchorFor(x float64) string {
	switch {
	case x < float64(chartPadL)+60:
		return "start"
	case x > float64(chartW-chartPadR)-60:
		return "end"
	default:
		return "middle"
	}
}

// daysBetween gives the days from one day to another, both included.
//
// 日子是站点的日子，所以是同一时区里的日子，一天一天地走。
func daysBetween(from, to string) []string {
	f, err := time.ParseInLocation("2006-01-02", from, store.CheckinZone())
	if err != nil {
		return nil
	}
	t, err := time.ParseInLocation("2006-01-02", to, store.CheckinZone())
	if err != nil {
		return nil
	}
	var out []string
	for d := f; !d.After(t); d = d.AddDate(0, 0, 1) {
		out = append(out, store.DayKey(d))
	}
	return out
}

// linePath gives the path of the line.
func linePath(days []ChartDay) string {
	if len(days) == 0 {
		return ""
	}
	s := fmt.Sprintf("M%.1f %.1f", days[0].X, days[0].Y)
	for _, d := range days[1:] {
		s += fmt.Sprintf(" L%.1f %.1f", d.X, d.Y)
	}
	return s
}

// areaPath gives the path of the wash under the line.
//
// 那是一层淡淡的水，不是实心的一块：十个点的透明度。
func areaPath(days []ChartDay, base float64) string {
	if len(days) == 0 {
		return ""
	}
	s := linePath(days)
	s += fmt.Sprintf(" L%.1f %.1f", days[len(days)-1].X, base)
	s += fmt.Sprintf(" L%.1f %.1f", days[0].X, base)
	return s + " Z"
}

// niceMax gives a round top for the scale.
//
// 顶上那个数要是人读得顺的数：1.5 倍、2 倍、2.5 倍…… 不是 1.37 倍。
func niceMax(v float64) float64 {
	mag := math.Pow(10, math.Floor(math.Log10(v)))
	switch n := v / mag; {
	case n <= 1:
		return mag
	case n <= 1.5:
		return 1.5 * mag
	case n <= 2:
		return 2 * mag
	case n <= 2.5:
		return 2.5 * mag
	case n <= 3:
		return 3 * mag
	case n <= 4:
		return 4 * mag
	case n <= 5:
		return 5 * mag
	case n <= 6:
		return 6 * mag
	case n <= 8:
		return 8 * mag
	default:
		return 10 * mag
	}
}

// tickLabel says a number on the axis.
//
// 一根轴上只有一种写法：一万以下写全，十万以上写成「万」。
func tickLabel(v float64) string {
	if v == 0 {
		return "0"
	}
	if v >= 100000 {
		return fmt.Sprintf("%.1f万", v/10000)
	}
	return fmt.Sprintf("%.0f", v)
}
