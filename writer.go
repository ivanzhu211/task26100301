package chronobase

import (
	"errors"
	"fmt"
	"os"
)

// Writer 用于创建封存块：调用方按时间戳非递减顺序逐点写入。
// 写入目标是临时文件，Close 成功后原子替换为最终路径。
type Writer struct {
	path      string
	tmp       *os.File
	meta      Meta
	frameSize int

	offset int64             // 当前文件写入偏移
	buf    []Point           // 未满一帧的缓冲点
	index  []frameIndexEntry // 已封存帧的索引
	count  int64             // 总点数
	lastTS int64
	hasAny bool
	closed bool
}

// Create 在 path 处创建一个新的封存块文件。
// frameSize 为每帧点数，<=0 时使用 DefaultFrameSize。
func Create(path string, meta Meta) (*Writer, error) {
	return CreateFrameSize(path, meta, 0)
}

// CreateFrameSize 与 Create 相同，但允许指定帧粒度（点/帧）。
func CreateFrameSize(path string, meta Meta, frameSize int) (*Writer, error) {
	if frameSize <= 0 {
		frameSize = DefaultFrameSize
	}
	tmp, err := os.CreateTemp("", "chronobase-*.tmp")
	if err != nil {
		return nil, err
	}
	w := &Writer{path: path, tmp: tmp, meta: meta, frameSize: frameSize}
	off, err := writeHeader(tmp, metaJSON{
		Metric:    meta.Metric,
		Source:    meta.Source,
		Hour:      meta.Hour,
		FrameSize: frameSize,
		// Count 在 Close 时无法回写头部（头部已封存），总点数改由尾部索引计数承载。
	})
	if err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return nil, err
	}
	w.offset = off
	return w, nil
}

// Add 追加一个点。时间戳必须非递减，且属于 Meta.Hour 所在的 UTC 小时。
func (w *Writer) Add(ts int64, value float64) error {
	if w.closed {
		return errors.New("chronobase: write on closed writer")
	}
	if w.hasAny && ts < w.lastTS {
		return fmt.Errorf("chronobase: timestamps not non-decreasing: %d < %d", ts, w.lastTS)
	}
	if HourStart(ts) != w.meta.Hour {
		return fmt.Errorf("chronobase: timestamp %d outside block hour %d", ts, w.meta.Hour)
	}
	w.buf = append(w.buf, Point{Timestamp: ts, Value: value})
	w.lastTS = ts
	w.hasAny = true
	w.count++
	if len(w.buf) >= w.frameSize {
		return w.flushFrame()
	}
	return nil
}

// flushFrame 将缓冲点压缩为一帧写入文件，并记录索引。
func (w *Writer) flushFrame() error {
	if len(w.buf) == 0 {
		return nil
	}
	comp, err := encodeFrame(w.buf)
	if err != nil {
		return err
	}
	entry := frameIndexEntry{
		FirstTS: w.buf[0].Timestamp,
		LastTS:  w.buf[len(w.buf)-1].Timestamp,
		Count:   uint32(len(w.buf)),
		Offset:  uint64(w.offset),
		CompLen: uint32(len(comp)),
	}
	if _, err := w.tmp.Write(comp); err != nil {
		return err
	}
	w.offset += int64(len(comp))
	w.index = append(w.index, entry)
	w.buf = w.buf[:0]
	return nil
}

// Close 冲刷并封存块：写入索引与尾部记录，fsync 后原子替换目标路径。
// 封存后文件不再修改。
func (w *Writer) Close() error {
	if w.closed {
		return errors.New("chronobase: writer already closed")
	}
	w.closed = true
	cleanup := func(err error) error {
		w.tmp.Close()
		os.Remove(w.tmp.Name())
		return err
	}
	if err := w.flushFrame(); err != nil {
		return cleanup(err)
	}
	indexOffset := uint64(w.offset)
	for _, e := range w.index {
		if _, err := w.tmp.Write(e.marshal()); err != nil {
			return cleanup(err)
		}
	}
	if err := writeFooter(w.tmp, footer{IndexOffset: indexOffset, IndexCount: uint32(len(w.index))}); err != nil {
		return cleanup(err)
	}
	if err := w.tmp.Sync(); err != nil {
		return cleanup(err)
	}
	if err := w.tmp.Close(); err != nil {
		os.Remove(w.tmp.Name())
		return err
	}
	if err := os.Rename(w.tmp.Name(), w.path); err != nil {
		os.Remove(w.tmp.Name())
		return err
	}
	return nil
}
