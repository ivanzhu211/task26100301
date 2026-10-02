package chronobase

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
)

// 文件布局：
//   [头部]   magic(4) | metaLen u32 | meta JSON
//   [数据帧] 连续存放，每帧独立 flate 压缩，可局部读取解压
//   [索引]   每帧一条 frameIndexEntry（二进制）
//   [尾部]   indexOffset u64 | indexCount u32 | magic(4)
//
// 帧内未压缩布局：
//   count uvarint | firstTS svarint | tsDelta uvarint*(n-1) | valueBits u64le*n
// 时间戳非递减，故差值用 uvarint；值逐位保存 math.Float64bits，保证无损。

var magic = [4]byte{'C', 'H', 'B', '1'}

const (
	headerLenLen = 4
	footerLen    = 8 + 4 + 4
	// DefaultFrameSize 是每帧默认点数，区间读取的局部解压粒度。
	DefaultFrameSize = 1024
)

// metaJSON 是头部元数据的序列化形式。
type metaJSON struct {
	Metric    string `json:"metric"`
	Source    string `json:"source"`
	Hour      int64  `json:"hour"`
	Count     int64  `json:"count"`
	FrameSize int    `json:"frameSize"`
}

// frameIndexEntry 描述一个数据帧在文件中的位置与时间范围。
type frameIndexEntry struct {
	FirstTS int64
	LastTS  int64
	Count   uint32
	Offset  uint64
	CompLen uint32
}

const frameIndexEntryLen = 8 + 8 + 4 + 8 + 4

func (e *frameIndexEntry) marshal() []byte {
	buf := make([]byte, frameIndexEntryLen)
	binary.LittleEndian.PutUint64(buf[0:8], uint64(e.FirstTS))
	binary.LittleEndian.PutUint64(buf[8:16], uint64(e.LastTS))
	binary.LittleEndian.PutUint32(buf[16:20], e.Count)
	binary.LittleEndian.PutUint64(buf[20:28], e.Offset)
	binary.LittleEndian.PutUint32(buf[28:32], e.CompLen)
	return buf
}

func (e *frameIndexEntry) unmarshal(buf []byte) error {
	if len(buf) < frameIndexEntryLen {
		return errors.New("chronobase: short index entry")
	}
	e.FirstTS = int64(binary.LittleEndian.Uint64(buf[0:8]))
	e.LastTS = int64(binary.LittleEndian.Uint64(buf[8:16]))
	e.Count = binary.LittleEndian.Uint32(buf[16:20])
	e.Offset = binary.LittleEndian.Uint64(buf[20:28])
	e.CompLen = binary.LittleEndian.Uint32(buf[28:32])
	return nil
}

// encodeFrame 将一帧点编码并压缩。值逐位保存，无损。
func encodeFrame(points []Point) ([]byte, error) {
	if len(points) == 0 {
		return nil, errors.New("chronobase: empty frame")
	}
	var raw []byte
	raw = binary.AppendUvarint(raw, uint64(len(points)))
	raw = binary.AppendVarint(raw, points[0].Timestamp)
	prev := points[0].Timestamp
	for _, p := range points[1:] {
		d := p.Timestamp - prev
		if d < 0 {
			return nil, fmt.Errorf("chronobase: timestamps not non-decreasing: %d < %d", p.Timestamp, prev)
		}
		raw = binary.AppendUvarint(raw, uint64(d))
		prev = p.Timestamp
	}
	var bits [8]byte
	for _, p := range points {
		binary.LittleEndian.PutUint64(bits[:], math.Float64bits(p.Value))
		raw = append(raw, bits[:]...)
	}

	var buf bytes.Buffer
	w, err := flate.NewWriter(&buf, flate.BestCompression)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(raw); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// decodeFrame 解压并解码一帧，返回 count 个点。
func decodeFrame(comp []byte) ([]Point, error) {
	r := flate.NewReader(bytes.NewReader(comp))
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("chronobase: frame inflate: %w", err)
	}
	if err := r.Close(); err != nil {
		return nil, err
	}
	count, n := binary.Uvarint(raw)
	if n <= 0 {
		return nil, errors.New("chronobase: bad frame count")
	}
	raw = raw[n:]
	if count == 0 {
		return nil, errors.New("chronobase: zero-count frame")
	}
	ts, n := binary.Varint(raw)
	if n <= 0 {
		return nil, errors.New("chronobase: bad frame first timestamp")
	}
	raw = raw[n:]
	points := make([]Point, count)
	points[0].Timestamp = ts
	for i := 1; i < int(count); i++ {
		d, n := binary.Uvarint(raw)
		if n <= 0 {
			return nil, errors.New("chronobase: bad frame timestamp delta")
		}
		raw = raw[n:]
		ts += int64(d)
		points[i].Timestamp = ts
	}
	if uint64(len(raw)) < count*8 {
		return nil, errors.New("chronobase: short frame values")
	}
	for i := range points {
		points[i].Value = math.Float64frombits(binary.LittleEndian.Uint64(raw[i*8:]))
	}
	return points, nil
}

// writeHeader 写入文件头，返回数据区起始偏移。
func writeHeader(w io.Writer, m metaJSON) (int64, error) {
	body, err := json.Marshal(m)
	if err != nil {
		return 0, err
	}
	if _, err := w.Write(magic[:]); err != nil {
		return 0, err
	}
	var lb [headerLenLen]byte
	binary.LittleEndian.PutUint32(lb[:], uint32(len(body)))
	if _, err := w.Write(lb[:]); err != nil {
		return 0, err
	}
	if _, err := w.Write(body); err != nil {
		return 0, err
	}
	return int64(4 + headerLenLen + len(body)), nil
}

// readHeader 读取并校验文件头与元数据。
func readHeader(r io.ReaderAt) (metaJSON, int64, error) {
	var m metaJSON
	var mg [4]byte
	if _, err := r.ReadAt(mg[:], 0); err != nil {
		return m, 0, err
	}
	if mg != magic {
		return m, 0, errors.New("chronobase: bad magic")
	}
	var lb [headerLenLen]byte
	if _, err := r.ReadAt(lb[:], 4); err != nil {
		return m, 0, err
	}
	n := binary.LittleEndian.Uint32(lb[:])
	if n > 1<<20 {
		return m, 0, errors.New("chronobase: meta too large")
	}
	body := make([]byte, n)
	if _, err := r.ReadAt(body, 4+headerLenLen); err != nil {
		return m, 0, err
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return m, 0, err
	}
	return m, int64(4 + headerLenLen + n), nil
}

// footer 记录索引区位置。
type footer struct {
	IndexOffset uint64
	IndexCount  uint32
}

func writeFooter(w io.Writer, f footer) error {
	buf := make([]byte, footerLen)
	binary.LittleEndian.PutUint64(buf[0:8], f.IndexOffset)
	binary.LittleEndian.PutUint32(buf[8:12], f.IndexCount)
	copy(buf[12:16], magic[:])
	_, err := w.Write(buf)
	return err
}

// readFooter 从文件末尾读取尾部记录，size 为文件总长度。
func readFooter(r io.ReaderAt, size int64) (footer, error) {
	var f footer
	if size < footerLen {
		return f, errors.New("chronobase: file too small")
	}
	buf := make([]byte, footerLen)
	if _, err := r.ReadAt(buf, size-footerLen); err != nil {
		return f, err
	}
	if !bytes.Equal(buf[12:16], magic[:]) {
		return f, errors.New("chronobase: bad footer magic")
	}
	f.IndexOffset = binary.LittleEndian.Uint64(buf[0:8])
	f.IndexCount = binary.LittleEndian.Uint32(buf[8:12])
	return f, nil
}
