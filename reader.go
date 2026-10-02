package chronobase

import (
	"errors"
	"fmt"
	"io"
	"iter"
	"os"
)

// Reader 打开一个封存块用于读取。块文件封存后不再修改，
// 关闭后重新打开仍可独立读取。
type Reader struct {
	f          *os.File
	meta       Meta
	index      []frameIndexEntry
	count      int64
	framesRead int
}

// Open 打开 path 处的封存块，读取头部元数据与帧索引（不读取数据帧）。
func Open(path string) (*Reader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Reader, error) {
		f.Close()
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		return fail(err)
	}
	mj, _, err := readHeader(f)
	if err != nil {
		return fail(err)
	}
	ft, err := readFooter(f, st.Size())
	if err != nil {
		return fail(err)
	}
	index := make([]frameIndexEntry, ft.IndexCount)
	var count int64
	for i := range index {
		buf := make([]byte, frameIndexEntryLen)
		off := int64(ft.IndexOffset) + int64(i)*frameIndexEntryLen
		if _, err := f.ReadAt(buf, off); err != nil {
			return fail(fmt.Errorf("chronobase: read index: %w", err))
		}
		if err := index[i].unmarshal(buf); err != nil {
			return fail(err)
		}
		count += int64(index[i].Count)
	}
	return &Reader{
		f:     f,
		meta:  Meta{Metric: mj.Metric, Source: mj.Source, Hour: mj.Hour},
		index: index,
		count: count,
	}, nil
}

// Meta 返回块的元数据。
func (r *Reader) Meta() Meta { return r.meta }

// Len 返回块内点数。
func (r *Reader) Len() int { return int(r.count) }

// FramesRead 返回自打开以来实际读取并解压的数据帧数，
// 用于观察区间读取的局部性。
func (r *Reader) FramesRead() int { return r.framesRead }

// readFrame 按索引局部读取并解压一个数据帧。
func (r *Reader) readFrame(i int) ([]Point, error) {
	e := r.index[i]
	buf := make([]byte, e.CompLen)
	if _, err := r.f.ReadAt(buf, int64(e.Offset)); err != nil {
		return nil, fmt.Errorf("chronobase: read frame %d: %w", i, err)
	}
	r.framesRead++
	return decodeFrame(buf)
}

// All 按写入顺序逐点返回全部数据，调用方可逐点消费。
func (r *Reader) All() iter.Seq2[Point, error] {
	return func(yield func(Point, error) bool) {
		for i := range r.index {
			points, err := r.readFrame(i)
			if err != nil {
				yield(Point{}, err)
				return
			}
			for _, p := range points {
				if !yield(p, nil) {
					return
				}
			}
		}
	}
}

// Range 返回 [start, end) 内的全部点（含边界处的重复观测），按写入顺序。
// 只局部读取并解压时间范围与该区间重叠的数据帧。
func (r *Reader) Range(start, end int64) iter.Seq2[Point, error] {
	return func(yield func(Point, error) bool) {
		if start >= end {
			return
		}
		for i := range r.index {
			e := r.index[i]
			// 帧时间范围为 [FirstTS, LastTS]，与 [start, end) 无重叠则跳过，
			// 不读取、不解压该帧。
			if e.LastTS < start || e.FirstTS >= end {
				continue
			}
			points, err := r.readFrame(i)
			if err != nil {
				yield(Point{}, err)
				return
			}
			for _, p := range points {
				if p.Timestamp < start || p.Timestamp >= end {
					continue
				}
				if !yield(p, nil) {
					return
				}
			}
		}
	}
}

// Close 关闭读取器。
func (r *Reader) Close() error {
	if r.f == nil {
		return errors.New("chronobase: reader already closed")
	}
	err := r.f.Close()
	r.f = nil
	return err
}

// 确保 Reader 满足 io.Closer。
var _ io.Closer = (*Reader)(nil)
