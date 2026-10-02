package chronobase

import (
	"math"
	"path/filepath"
	"testing"
	"time"
)

// collect 逐点消费迭代器，返回全部点。
func collect(t *testing.T, it *Iterator) []Point {
	t.Helper()
	var out []Point
	for it.Next() {
		out = append(out, it.Point())
	}
	if err := it.Err(); err != nil {
		t.Fatalf("迭代出错: %v", err)
	}
	return out
}

// writeBlock 创建块、逐点写入并封存，返回文件路径。
func writeBlock(t *testing.T, path string, meta Meta, points []Point) {
	t.Helper()
	w, err := Create(path, meta)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, p := range points {
		if err := w.Add(p.Timestamp, p.Value); err != nil {
			t.Fatalf("Add(%d): %v", p.Timestamp, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// filterPoints 直接过滤原始序列，作为区间查询的参照结果。
func filterPoints(points []Point, start, end int64) []Point {
	var out []Point
	for _, p := range points {
		if p.Timestamp >= start && p.Timestamp < end {
			out = append(out, p)
		}
	}
	return out
}

func samePoints(t *testing.T, got, want []Point) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("点数不一致: got %d, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i].Timestamp != want[i].Timestamp {
			t.Fatalf("点 %d 时间戳不一致: got %d, want %d", i, got[i].Timestamp, want[i].Timestamp)
		}
		if math.Float64bits(got[i].Value) != math.Float64bits(want[i].Value) {
			t.Fatalf("点 %d 值位型不一致: got %#016x, want %#016x",
				i, math.Float64bits(got[i].Value), math.Float64bits(want[i].Value))
		}
	}
}

var testHour = time.Date(2026, 9, 30, 14, 0, 0, 0, time.UTC).UnixNano()

// TestSpecialFloatBitwiseRoundTrip 覆盖特殊浮点位型的逐位往返，
// 包括关闭后重新打开并顺序读回。
func TestSpecialFloatBitwiseRoundTrip(t *testing.T) {
	bits := []uint64{
		0x0000000000000000, // +0
		0x8000000000000000, // -0
		0x7FF0000000000000, // +Inf
		0xFFF0000000000000, // -Inf
		0x0000000000000001, // 最小非规格化数
		0x8000000000000001, // 负最小非规格化数
		0x000FFFFFFFFFFFFF, // 最大非规格化数
		0x0010000000000000, // 最小规格化数
		0x7FEFFFFFFFFFFFFF, // 最大有限值
		0x7FF8000000000000, // 默认 NaN
		0x7FF8000000000001, // 携带载荷的 NaN
		0x7FF0000000000001, // 信号 NaN 位型
		0xFFF8000000000000, // 负 NaN
		0xFFF8000000000042, // 负 NaN 自定义载荷
		0x7FFFFFFFFFFFFFFF, // 全一尾数 NaN
	}
	var points []Point
	for i, b := range bits {
		points = append(points, Point{
			Timestamp: testHour + int64(i)*int64(time.Millisecond),
			Value:     math.Float64frombits(b),
		})
	}
	path := filepath.Join(t.TempDir(), "special.cbk")
	meta := Meta{Metric: "sensor.temp", Source: "edge-01"}
	writeBlock(t, path, meta, points)

	// 关闭后重新打开，顺序读回。
	b, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer b.Close()
	if got := b.Meta(); got != meta {
		t.Fatalf("元信息不一致: got %+v, want %+v", got, meta)
	}
	if b.HourStart() != testHour {
		t.Fatalf("小时起始不一致: got %d, want %d", b.HourStart(), testHour)
	}
	samePoints(t, collect(t, b.ReadAll()), points)
}

// dupSeries 构造含长串重复时间戳（跨越多个压缩子块）的序列。
func dupSeries() (points []Point, dupTS int64) {
	// 前导：10 个间隔 1s 的点。
	for i := 0; i < 10; i++ {
		points = append(points, Point{testHour + int64(i)*int64(time.Second), float64(i)})
	}
	// 长串重复：5000 个相同时间戳，跨越多个压缩子块。
	dupTS = testHour + 100*int64(time.Second)
	for i := 0; i < 5000; i++ {
		points = append(points, Point{dupTS, float64(1000 + i)})
	}
	// 后续：2000 个间隔 1s 的点，使末尾子块不含重复串。
	for i := 0; i < 2000; i++ {
		points = append(points, Point{dupTS + int64(i+1)*int64(time.Second), float64(2000 + i)})
	}
	return points, dupTS
}

// TestDuplicateTimestampRanges 覆盖长串重复时间戳两侧及内部的区间读取，
// 包括空结果与块尾部查询，并验证重新打开后的局部读取行为。
func TestDuplicateTimestampRanges(t *testing.T) {
	points, dupTS := dupSeries()
	last := points[len(points)-1].Timestamp
	path := filepath.Join(t.TempDir(), "dups.cbk")
	writeBlock(t, path, Meta{Metric: "m", Source: "s"}, points)

	ranges := []struct {
		name       string
		start, end int64
	}{
		{"重复串内部", dupTS, dupTS + 1},
		{"重复串左侧(不含串)", testHour, dupTS},
		{"重复串右侧(含串)", dupTS, dupTS + 50*int64(time.Second)},
		{"跨重复串", testHour + 5*int64(time.Second), dupTS + 5*int64(time.Second)},
		{"空结果(间隙中)", testHour + int64(time.Second), testHour + 2*int64(time.Second)},
		{"空结果(块之前)", testHour - 10*int64(time.Second), testHour - 5*int64(time.Second)},
		{"空结果(块之后)", last + 1, last + 2},
		{"块尾部(含末点)", last, last + 1},
		{"块尾部(跨末点)", last - int64(time.Second), last + 1000},
		{"整小时", testHour, testHour + hourNs},
	}

	// 第一轮：封存后直接打开读取。
	b, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for _, r := range ranges {
		got := collect(t, b.ReadRange(r.start, r.end))
		samePoints(t, got, filterPoints(points, r.start, r.end))
	}
	samePoints(t, collect(t, b.ReadAll()), points)
	b.Close()

	// 第二轮：关闭后重新打开，结果必须与直接过滤原始序列逐点一致。
	b2, err := Open(path)
	if err != nil {
		t.Fatalf("重新 Open: %v", err)
	}
	defer b2.Close()
	for _, r := range ranges {
		got := collect(t, b2.ReadRange(r.start, r.end))
		samePoints(t, got, filterPoints(points, r.start, r.end))
	}
	samePoints(t, collect(t, b2.ReadAll()), points)
}

// TestRangeReadIsLocal 白盒验证区间读取只加载覆盖该区间的压缩子块。
func TestRangeReadIsLocal(t *testing.T) {
	points, dupTS := dupSeries()
	last := points[len(points)-1].Timestamp
	path := filepath.Join(t.TempDir(), "local.cbk")
	writeBlock(t, path, Meta{Metric: "m", Source: "s"}, points)

	b, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer b.Close()

	totalChunks := len(b.index)
	if totalChunks < 4 {
		t.Fatalf("子块数过少，无法验证局部性: %d", totalChunks)
	}

	// 无交集的区间：不加载任何数据子块。
	it := b.ReadRange(last+1, last+2)
	if len(it.chunks) != 0 {
		t.Fatalf("空结果区间应加载 0 个子块, got %d", len(it.chunks))
	}
	if n := len(collect(t, it)); n != 0 {
		t.Fatalf("空结果区间应返回 0 点, got %d", n)
	}

	// 块尾部区间：只加载最后一个子块。
	it = b.ReadRange(last, last+1)
	if len(it.chunks) != 1 {
		t.Fatalf("块尾部区间应加载 1 个子块, got %d", len(it.chunks))
	}

	// 重复串内部区间：只加载覆盖重复串的子块，远少于全块。
	it = b.ReadRange(dupTS, dupTS+1)
	if n := len(it.chunks); n >= totalChunks {
		t.Fatalf("重复串区间应局部加载子块: got %d, 全块 %d", n, totalChunks)
	}
	if got, want := len(collect(t, it)), 5000; got != want {
		t.Fatalf("重复串区间点数: got %d, want %d", got, want)
	}

	// 重新打开后同样局部。
	b.Close()
	b2, err := Open(path)
	if err != nil {
		t.Fatalf("重新 Open: %v", err)
	}
	defer b2.Close()
	it = b2.ReadRange(dupTS, dupTS+1)
	if n := len(it.chunks); n >= totalChunks {
		t.Fatalf("重新打开后重复串区间应局部加载子块: got %d, 全块 %d", n, totalChunks)
	}
	if got, want := len(collect(t, it)), 5000; got != want {
		t.Fatalf("重新打开后重复串区间点数: got %d, want %d", got, want)
	}
}

// TestWriterValidation 覆盖写入侧的约束校验。
func TestWriterValidation(t *testing.T) {
	dir := t.TempDir()

	if _, err := Create(filepath.Join(dir, "a.cbk"), Meta{Source: "s"}); err != ErrEmptyMetric {
		t.Fatalf("空指标应报错: %v", err)
	}
	if _, err := Create(filepath.Join(dir, "a.cbk"), Meta{Metric: "m"}); err != ErrEmptySource {
		t.Fatalf("空来源应报错: %v", err)
	}

	w, err := Create(filepath.Join(dir, "b.cbk"), Meta{Metric: "m", Source: "s"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := w.Add(testHour+10, 1); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := w.Add(testHour+5, 2); err != ErrTimestampOrder {
		t.Fatalf("时间戳回退应报错: %v", err)
	}
	if err := w.Add(testHour+hourNs, 3); err != ErrHourMismatch {
		t.Fatalf("跨小时应报错: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := w.Add(testHour+20, 4); err != ErrAlreadySealed {
		t.Fatalf("封存后写入应报错: %v", err)
	}
}

// TestEmptyBlock 覆盖空块的创建与读取。
func TestEmptyBlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.cbk")
	writeBlock(t, path, Meta{Metric: "m", Source: "s"}, nil)

	b, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer b.Close()
	if n := len(collect(t, b.ReadAll())); n != 0 {
		t.Fatalf("空块全量读取应返回 0 点, got %d", n)
	}
	if n := len(collect(t, b.ReadRange(testHour, testHour+hourNs))); n != 0 {
		t.Fatalf("空块区间读取应返回 0 点, got %d", n)
	}
}

// TestInvalidRange 覆盖非法区间。
func TestInvalidRange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.cbk")
	writeBlock(t, path, Meta{Metric: "m", Source: "s"},
		[]Point{{testHour, 1}})
	b, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer b.Close()
	it := b.ReadRange(testHour+1, testHour)
	if it.Next() {
		t.Fatal("非法区间不应产出点")
	}
	if it.Err() != ErrInvalidRange {
		t.Fatalf("非法区间应报 ErrInvalidRange: %v", it.Err())
	}
}
