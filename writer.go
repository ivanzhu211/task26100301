package chronobase

import (
	"os"
)

// hourNs 是一个 UTC 小时的纳秒数。
const hourNs = int64(3600) * 1e9

// hourStartOf 返回 ts 所属 UTC 小时的起始纳秒时间戳（向下取整，兼容负时间戳）。
func hourStartOf(ts int64) int64 {
	m := ts % hourNs
	if m < 0 {
		m += hourNs
	}
	return ts - m
}

// Writer 用于创建封存块。调用方按时间戳非递减顺序逐点写入，
// 全部写完后调用 Close 完成封存。封存后的块不再修改。
type Writer struct {
	meta Meta

	file      *os.File
	tmpPath   string
	finalPath string

	chunk     []Point      // 当前未满的压缩子块缓冲
	index     []chunkIndex // 已落盘子块的索引
	offset    int64        // 当前写入偏移
	numPoints uint64
	hourStart int64
	hasPoints bool
	prevTS    int64

	sealed bool
	closed bool
}

// Create 在 path 处创建一个新的封存块写入器。
// 数据先写入临时文件，Close 封存成功后原子替换为正式文件。
func Create(path string, meta Meta) (*Writer, error) {
	if meta.Metric == "" {
		return nil, ErrEmptyMetric
	}
	if meta.Source == "" {
		return nil, ErrEmptySource
	}
	tmpPath := path + ".tmp"
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	w := &Writer{
		meta:      meta,
		file:      f,
		tmpPath:   tmpPath,
		finalPath: path,
	}
	h := encodeHeader(header{
		metricLen: uint32(len(meta.Metric)),
		sourceLen: uint32(len(meta.Source)),
	})
	if _, err := f.Write(h); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return nil, err
	}
	if _, err := f.Write([]byte(meta.Metric)); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return nil, err
	}
	if _, err := f.Write([]byte(meta.Source)); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return nil, err
	}
	w.offset = int64(headerSize + len(meta.Metric) + len(meta.Source))
	return w, nil
}

// Add 追加一个点。时间戳必须非递减，且与块内已有点属于同一 UTC 小时。
// 相同时间戳可以出现任意多次，每次都代表独立观测。
func (w *Writer) Add(ts int64, value float64) error {
	if w.closed {
		return ErrAlreadySealed
	}
	if w.hasPoints {
		if ts < w.prevTS {
			return ErrTimestampOrder
		}
		if hourStartOf(ts) != w.hourStart {
			return ErrHourMismatch
		}
	} else {
		w.hourStart = hourStartOf(ts)
		w.hasPoints = true
	}
	w.prevTS = ts
	w.chunk = append(w.chunk, Point{Timestamp: ts, Value: value})
	w.numPoints++
	if len(w.chunk) == pointsPerChunk {
		return w.flushChunk()
	}
	return nil
}

// flushChunk 将当前缓冲的子块压缩落盘并登记索引。
func (w *Writer) flushChunk() error {
	if len(w.chunk) == 0 {
		return nil
	}
	raw := encodeChunkRaw(w.chunk)
	comp, err := compressChunk(raw)
	if err != nil {
		return err
	}
	if _, err := w.file.Write(comp); err != nil {
		return err
	}
	w.index = append(w.index, chunkIndex{
		firstTS:   w.chunk[0].Timestamp,
		lastTS:    w.chunk[len(w.chunk)-1].Timestamp,
		offset:    w.offset,
		compLen:   int64(len(comp)),
		numPoints: int32(len(w.chunk)),
	})
	w.offset += int64(len(comp))
	w.chunk = w.chunk[:0]
	return nil
}

// Close 封存块并落盘全部元数据。之后文件可被 Open 独立读取。
// Close 之后不能再 Add。
func (w *Writer) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	if err := w.flushChunk(); err != nil {
		w.cleanup()
		return err
	}
	indexOffset := w.offset
	for _, ci := range w.index {
		if _, err := w.file.Write(ci.encode()); err != nil {
			w.cleanup()
			return err
		}
		w.offset += indexEntrySize
	}
	t := encodeTrailer(trailer{indexOffset: indexOffset, numChunks: uint32(len(w.index))})
	if _, err := w.file.Write(t); err != nil {
		w.cleanup()
		return err
	}
	// 回填头部中的点数与小时起始。
	h := encodeHeader(header{
		numPoints: w.numPoints,
		hourStart: w.hourStart,
		metricLen: uint32(len(w.meta.Metric)),
		sourceLen: uint32(len(w.meta.Source)),
	})
	if _, err := w.file.WriteAt(h, 0); err != nil {
		w.cleanup()
		return err
	}
	if err := w.file.Sync(); err != nil {
		w.cleanup()
		return err
	}
	if err := w.file.Close(); err != nil {
		w.cleanup()
		return err
	}
	if err := os.Rename(w.tmpPath, w.finalPath); err != nil {
		os.Remove(w.tmpPath)
		return err
	}
	w.sealed = true
	return nil
}

// cleanup 在出错时关闭并删除临时文件。
func (w *Writer) cleanup() {
	w.file.Close()
	os.Remove(w.tmpPath)
}
