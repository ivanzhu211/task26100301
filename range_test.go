package chronobase

import (
	"fmt"
	"math"
	"path/filepath"
	"testing"
	"time"
)

// buildDupBlock 构造含长串重复时间戳的块：
// 若干独特时间戳锚点，每个锚点挂一长串重复观测，锚点之间有小间隔。
// 返回原始序列（与写入顺序一致）。
func buildDupBlock(t *testing.T, path string, frameSize int) (Meta, []Point) {
	t.Helper()
	hour := HourStart(time.Date(2026, 10, 2, 8, 0, 0, 0, time.UTC).UnixNano())
	meta := Meta{Metric: "cpu", Source: "edge-7", Hour: hour}

	var pts []Point
	anchor := hour + int64(time.Minute)
	for a := 0; a < 12; a++ {
		reps := 1 + a*3 // 各锚点的重复串长度递增，最长 34
		for k := 0; k < reps; k++ {
			pts = append(pts, Point{
				Timestamp: anchor,
				Value:     math.Float64frombits(uint64(0x3ff0000000000000 + a*1000 + k)),
			})
		}
		anchor += int64(5 * time.Second)
	}
	// 块尾部再放一串重复观测，覆盖“位于块尾部的区间”场景。
	tail := hour + int64(time.Hour) - int64(time.Second)
	for k := 0; k < 20; k++ {
		pts = append(pts, Point{Timestamp: tail, Value: float64(k)})
	}

	w, err := CreateFrameSize(path, meta, frameSize)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, p := range pts {
		if err := w.Add(p.Timestamp, p.Value); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return meta, pts
}

func filterPoints(pts []Point, start, end int64) []Point {
	var out []Point
	for _, p := range pts {
		if p.Timestamp >= start && p.Timestamp < end {
			out = append(out, p)
		}
	}
	return out
}

func collectRange(t *testing.T, r *Reader, start, end int64) []Point {
	t.Helper()
	var out []Point
	for p, err := range r.Range(start, end) {
		if err != nil {
			t.Fatalf("Range(%d,%d): %v", start, end, err)
		}
		out = append(out, p)
	}
	return out
}

func assertPointsEqual(t *testing.T, got, want []Point, label string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %d points, want %d", label, len(got), len(want))
	}
	for i := range want {
		if got[i].Timestamp != want[i].Timestamp ||
			math.Float64bits(got[i].Value) != math.Float64bits(want[i].Value) {
			t.Fatalf("%s: point %d = (%d,%#x), want (%d,%#x)",
				label, i, got[i].Timestamp, math.Float64bits(got[i].Value),
				want[i].Timestamp, math.Float64bits(want[i].Value))
		}
	}
}

func TestRangeWithDuplicateTimestamps(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dup.chb")
	const frameSize = 7 // 小帧粒度，让重复串跨越多帧
	meta, pts := buildDupBlock(t, path, frameSize)
	hour := meta.Hour

	// 收集所有出现过的独特时间戳作为区间边界候选。
	var anchors []int64
	seen := map[int64]bool{}
	for _, p := range pts {
		if !seen[p.Timestamp] {
			seen[p.Timestamp] = true
			anchors = append(anchors, p.Timestamp)
		}
	}
	tailTS := anchors[len(anchors)-1]

	type tc struct {
		name       string
		start, end int64
	}
	var cases []tc
	// 空结果：块开始之前、两个锚点之间的空隙、块结束之后、start==end、start>end。
	cases = append(cases,
		tc{"before-block", hour - int64(time.Hour), hour},
		tc{"gap-between-anchors", anchors[0] + 1, anchors[1]},
		tc{"after-block", hour + int64(time.Hour), hour + 2*int64(time.Hour)},
		tc{"empty-start-eq-end", anchors[2], anchors[2]},
		tc{"empty-start-gt-end", anchors[3], anchors[2]},
	)
	// 长串重复时间戳的两侧与内部。
	for _, a := range anchors {
		cases = append(cases,
			tc{fmt.Sprintf("exact-anchor-%d", a), a, a + 1},
			tc{fmt.Sprintf("anchor-left-edge-%d", a), a - 1, a + 1},
			tc{fmt.Sprintf("anchor-right-open-%d", a), a - int64(time.Second), a},
		)
	}
	// 块尾部区间：end 超出块末尾、start 落在尾部重复串上。
	cases = append(cases,
		tc{"tail-exact", tailTS, tailTS + 1},
		tc{"tail-beyond-block-end", tailTS - 1, hour + 2*int64(time.Hour)},
		tc{"tail-open-at-end", tailTS + 1, hour + int64(time.Hour)},
	)
	// 跨多个锚点的大区间与全块区间。
	cases = append(cases,
		tc{"multi-anchor", anchors[1], anchors[5] + 1},
		tc{"whole-hour", hour, hour + int64(time.Hour)},
	)

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, err := Open(path)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer r.Close()
			got := collectRange(t, r, c.start, c.end)
			want := filterPoints(pts, c.start, c.end)
			assertPointsEqual(t, got, want, c.name)
		})
	}
}

func TestRangeLocalReadAfterReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "local.chb")
	const frameSize = 7
	meta, pts := buildDupBlock(t, path, frameSize)
	hour := meta.Hour

	totalFrames := (len(pts) + frameSize - 1) / frameSize

	r, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer r.Close()

	// 首次打开后的区间读取应只触及重叠帧。
	mid := pts[len(pts)/2].Timestamp
	got := collectRange(t, r, mid, mid+1)
	assertPointsEqual(t, got, filterPoints(pts, mid, mid+1), "mid-anchor")
	if r.FramesRead() == 0 || r.FramesRead() >= totalFrames {
		t.Fatalf("FramesRead = %d, want in (0, %d)", r.FramesRead(), totalFrames)
	}

	// 空结果区间不读取任何数据帧。
	before := r.FramesRead()
	if got := collectRange(t, r, hour-int64(time.Minute), hour); len(got) != 0 {
		t.Fatalf("empty range got %d points", len(got))
	}
	if r.FramesRead() != before {
		t.Fatalf("empty range read %d extra frames", r.FramesRead()-before)
	}

	// 关闭后重新打开，计数归零，局部读取行为一致。
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	r2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer r2.Close()
	if r2.FramesRead() != 0 {
		t.Fatalf("reopened reader FramesRead = %d, want 0", r2.FramesRead())
	}
	tail := pts[len(pts)-1].Timestamp
	got2 := collectRange(t, r2, tail, tail+1)
	assertPointsEqual(t, got2, filterPoints(pts, tail, tail+1), "tail-after-reopen")
	// 尾部 20 个重复观测按 7 点/帧散布在 3 帧中，应只读这 3 帧而非全部。
	const wantTailFrames = 3
	if r2.FramesRead() != wantTailFrames {
		t.Fatalf("tail range read %d frames, want %d", r2.FramesRead(), wantTailFrames)
	}
	if r2.FramesRead() >= totalFrames {
		t.Fatalf("tail range not local: read %d of %d frames", r2.FramesRead(), totalFrames)
	}
}
