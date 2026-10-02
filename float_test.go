package chronobase

import (
	"math"
	"path/filepath"
	"testing"
	"time"
)

// specialBits 覆盖正负零、正负无穷、非规格化数与携带不同载荷的 NaN。
var specialBits = []uint64{
	0x0000000000000000, // +0
	0x8000000000000000, // -0
	0x7ff0000000000000, // +Inf
	0xfff0000000000000, // -Inf
	0x0000000000000001, // 最小正非规格化数
	0x8000000000000001, // 最小负非规格化数
	0x000fffffffffffff, // 最大非规格化数
	0x0010000000000000, // 最小正规格化数
	0x7fefffffffffffff, // 最大有限值
	0xffefffffffffffff, // 最小（绝对值最大负）有限值
	0x7ff8000000000000, // 静默 NaN
	0xfff8000000000000, // 负静默 NaN
	0x7ff8000000000001, // 携带载荷的 NaN
	0x7ff80000000000ff, // 携带载荷的 NaN
	0xfff80000deadbeef, // 负 NaN，自定义载荷
	0x7ff0000000000001, // 发信 NaN
	0x7ff4000000000000, // 发信 NaN
	0x3ff0000000000000, // 1.0
	0xbff0000000000000, // -1.0
}

func collectAll(t *testing.T, r *Reader) []Point {
	t.Helper()
	var out []Point
	for p, err := range r.All() {
		if err != nil {
			t.Fatalf("All: %v", err)
		}
		out = append(out, p)
	}
	return out
}

func TestSpecialFloatBitExactRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "special.chb")
	hour := HourStart(time.Date(2026, 10, 2, 8, 0, 0, 0, time.UTC).UnixNano())

	w, err := Create(path, Meta{Metric: "temp", Source: "edge-1", Hour: hour})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// 每个特殊位型写多次（含相同时间戳重复观测）。
	var want []Point
	ts := hour
	for round := 0; round < 3; round++ {
		for _, b := range specialBits {
			p := Point{Timestamp: ts, Value: math.Float64frombits(b)}
			if err := w.Add(p.Timestamp, p.Value); err != nil {
				t.Fatalf("Add: %v", err)
			}
			want = append(want, p)
			ts += int64(time.Second)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// 关闭后重新打开，完整顺序读回。
	r, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer r.Close()
	if got := r.Meta(); got != (Meta{Metric: "temp", Source: "edge-1", Hour: hour}) {
		t.Fatalf("Meta = %+v", got)
	}
	if r.Len() != len(want) {
		t.Fatalf("Len = %d, want %d", r.Len(), len(want))
	}
	got := collectAll(t, r)
	if len(got) != len(want) {
		t.Fatalf("read %d points, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Timestamp != want[i].Timestamp {
			t.Fatalf("point %d: ts = %d, want %d", i, got[i].Timestamp, want[i].Timestamp)
		}
		gb, wb := math.Float64bits(got[i].Value), math.Float64bits(want[i].Value)
		if gb != wb {
			t.Fatalf("point %d: bits = %#016x, want %#016x", i, gb, wb)
		}
	}
}
