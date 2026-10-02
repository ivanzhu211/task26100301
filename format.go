package chronobase

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"io"
	"math"
)

// 文件布局：
//
//	[文件头][指标名][来源标识][压缩子块 0]...[压缩子块 N-1][子块索引][文件尾]
//
// 文件头固定长度；索引记录每个压缩子块的时间范围与文件偏移，
// 区间读取时据此只加载并解压覆盖该区间的子块；文件尾记录索引位置。

const (
	magic          = 0x43424b31 // "CBK1"
	version        = 1
	pointsPerChunk = 1024 // 每个压缩子块包含的点数

	headerSize     = 48
	indexEntrySize = 40
	trailerSize    = 16
	trailerMagic   = 0x43424b54 // "CBKT"
)

// header 是文件的固定长度头部。
type header struct {
	numPoints uint64
	hourStart int64 // 块所属 UTC 小时的起始纳秒时间戳；空块为 0
	metricLen uint32
	sourceLen uint32
}

func encodeHeader(h header) []byte {
	buf := make([]byte, headerSize)
	binary.LittleEndian.PutUint32(buf[0:4], magic)
	binary.LittleEndian.PutUint32(buf[4:8], version)
	binary.LittleEndian.PutUint32(buf[8:12], pointsPerChunk)
	binary.LittleEndian.PutUint64(buf[16:24], h.numPoints)
	binary.LittleEndian.PutUint64(buf[24:32], uint64(h.hourStart))
	binary.LittleEndian.PutUint32(buf[32:36], h.metricLen)
	binary.LittleEndian.PutUint32(buf[36:40], h.sourceLen)
	return buf
}

func decodeHeader(buf []byte) (header, error) {
	var h header
	if len(buf) < headerSize {
		return h, ErrInvalidFile
	}
	if binary.LittleEndian.Uint32(buf[0:4]) != magic {
		return h, ErrInvalidFile
	}
	if binary.LittleEndian.Uint32(buf[4:8]) != version {
		return h, ErrUnsupportedVer
	}
	if binary.LittleEndian.Uint32(buf[8:12]) != pointsPerChunk {
		return h, ErrUnsupportedVer
	}
	h.numPoints = binary.LittleEndian.Uint64(buf[16:24])
	h.hourStart = int64(binary.LittleEndian.Uint64(buf[24:32]))
	h.metricLen = binary.LittleEndian.Uint32(buf[32:36])
	h.sourceLen = binary.LittleEndian.Uint32(buf[36:40])
	return h, nil
}

// trailer 位于文件末尾，记录索引位置与点数校验。
type trailer struct {
	indexOffset int64
	numChunks   uint32
}

func encodeTrailer(t trailer) []byte {
	buf := make([]byte, trailerSize)
	binary.LittleEndian.PutUint64(buf[0:8], uint64(t.indexOffset))
	binary.LittleEndian.PutUint32(buf[8:12], t.numChunks)
	binary.LittleEndian.PutUint32(buf[12:16], trailerMagic)
	return buf
}

func decodeTrailer(buf []byte) (trailer, error) {
	var t trailer
	if len(buf) < trailerSize {
		return t, ErrInvalidFile
	}
	if binary.LittleEndian.Uint32(buf[12:16]) != trailerMagic {
		return t, ErrInvalidFile
	}
	t.indexOffset = int64(binary.LittleEndian.Uint64(buf[0:8]))
	t.numChunks = binary.LittleEndian.Uint32(buf[8:12])
	return t, nil
}

// chunkIndex 是单个压缩子块的索引项。
type chunkIndex struct {
	firstTS   int64 // 子块内最小时间戳
	lastTS    int64 // 子块内最大时间戳
	offset    int64 // 压缩数据在文件中的偏移
	compLen   int64 // 压缩数据长度
	numPoints int32 // 子块内点数
}

func (c chunkIndex) encode() []byte {
	buf := make([]byte, indexEntrySize)
	binary.LittleEndian.PutUint64(buf[0:8], uint64(c.firstTS))
	binary.LittleEndian.PutUint64(buf[8:16], uint64(c.lastTS))
	binary.LittleEndian.PutUint64(buf[16:24], uint64(c.offset))
	binary.LittleEndian.PutUint64(buf[24:32], uint64(c.compLen))
	binary.LittleEndian.PutUint32(buf[32:36], uint32(c.numPoints))
	return buf
}

func decodeChunkIndex(buf []byte) chunkIndex {
	return chunkIndex{
		firstTS:   int64(binary.LittleEndian.Uint64(buf[0:8])),
		lastTS:    int64(binary.LittleEndian.Uint64(buf[8:16])),
		offset:    int64(binary.LittleEndian.Uint64(buf[16:24])),
		compLen:   int64(binary.LittleEndian.Uint64(buf[24:32])),
		numPoints: int32(binary.LittleEndian.Uint32(buf[32:36])),
	}
}

// encodeChunkRaw 将一个子块的点编码为未压缩字节流：
// 首点为有符号 varint 绝对时间戳，其后为非负 varint 时间戳增量；
// 每个时间戳后紧跟 8 字节小端 float64 原始位型，保证逐位无损。
func encodeChunkRaw(points []Point) []byte {
	buf := make([]byte, 0, len(points)*12)
	var tmp [binary.MaxVarintLen64]byte
	var prev int64
	for i, p := range points {
		var n int
		if i == 0 {
			n = binary.PutVarint(tmp[:], p.Timestamp)
		} else {
			n = binary.PutUvarint(tmp[:], uint64(p.Timestamp-prev))
		}
		buf = append(buf, tmp[:n]...)
		var bits [8]byte
		binary.LittleEndian.PutUint64(bits[:], math.Float64bits(p.Value))
		buf = append(buf, bits[:]...)
		prev = p.Timestamp
	}
	return buf
}

// decodeChunkRaw 是 encodeChunkRaw 的逆过程，n 为子块内点数。
func decodeChunkRaw(raw []byte, n int) ([]Point, error) {
	points := make([]Point, 0, n)
	prev := int64(0)
	for i := 0; i < n; i++ {
		var ts int64
		if i == 0 {
			v, read := binary.Varint(raw)
			if read <= 0 {
				return nil, ErrCorrupt
			}
			ts = v
			raw = raw[read:]
		} else {
			d, read := binary.Uvarint(raw)
			if read <= 0 {
				return nil, ErrCorrupt
			}
			ts = prev + int64(d)
			raw = raw[read:]
		}
		if len(raw) < 8 {
			return nil, ErrCorrupt
		}
		bits := binary.LittleEndian.Uint64(raw[:8])
		raw = raw[8:]
		points = append(points, Point{Timestamp: ts, Value: math.Float64frombits(bits)})
		prev = ts
	}
	return points, nil
}

// compressChunk 使用 DEFLATE 压缩子块原始字节。
func compressChunk(raw []byte) ([]byte, error) {
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

// decompressChunk 解压子块数据。
func decompressChunk(comp []byte) ([]byte, error) {
	r := flate.NewReader(bytes.NewReader(comp))
	defer r.Close()
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, ErrCorrupt
	}
	return raw, nil
}
