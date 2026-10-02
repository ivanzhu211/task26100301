package chronobase

import (
	"io"
	"os"
	"sort"
)

// Block 是一个已打开的封存块。打开时只读取头部与索引元数据，
// 数据子块在迭代时按需局部加载解压。可并发进行多次读取。
type Block struct {
	file      *os.File
	meta      Meta
	index     []chunkIndex
	numPoints uint64
	hourStart int64
}

// Open 打开一个封存块文件，读取其头部、索引等全部元数据。
func Open(path string) (*Block, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	b := &Block{file: f}
	if err := b.load(); err != nil {
		f.Close()
		return nil, err
	}
	return b, nil
}

func (b *Block) load() error {
	st, err := b.file.Stat()
	if err != nil {
		return err
	}
	size := st.Size()
	if size < headerSize+trailerSize {
		return ErrInvalidFile
	}
	// 文件尾：定位索引。
	tbuf := make([]byte, trailerSize)
	if _, err := b.file.ReadAt(tbuf, size-trailerSize); err != nil {
		return err
	}
	t, err := decodeTrailer(tbuf)
	if err != nil {
		return err
	}
	indexLen := int64(t.numChunks) * indexEntrySize
	if t.indexOffset < headerSize || t.indexOffset+indexLen+trailerSize != size {
		return ErrCorrupt
	}
	// 索引。
	b.index = make([]chunkIndex, t.numChunks)
	if t.numChunks > 0 {
		ibuf := make([]byte, indexLen)
		if _, err := b.file.ReadAt(ibuf, t.indexOffset); err != nil {
			return err
		}
		for i := range b.index {
			b.index[i] = decodeChunkIndex(ibuf[i*indexEntrySize:])
		}
	}
	// 文件头与元信息。
	hbuf := make([]byte, headerSize)
	if _, err := b.file.ReadAt(hbuf, 0); err != nil {
		return err
	}
	h, err := decodeHeader(hbuf)
	if err != nil {
		return err
	}
	metaLen := int64(h.metricLen) + int64(h.sourceLen)
	if int64(headerSize)+metaLen > t.indexOffset {
		return ErrCorrupt
	}
	mbuf := make([]byte, metaLen)
	if _, err := b.file.ReadAt(mbuf, headerSize); err != nil {
		return err
	}
	b.meta = Meta{
		Metric: string(mbuf[:h.metricLen]),
		Source: string(mbuf[h.metricLen:]),
	}
	b.numPoints = h.numPoints
	b.hourStart = h.hourStart
	return nil
}

// Meta 返回块的元信息（指标名与来源序列标识）。
func (b *Block) Meta() Meta { return b.meta }

// NumPoints 返回块内点的总数。
func (b *Block) NumPoints() uint64 { return b.numPoints }

// HourStart 返回块所属 UTC 小时的起始纳秒时间戳；空块返回 0。
func (b *Block) HourStart() int64 { return b.hourStart }

// Close 关闭块，释放文件句柄。
func (b *Block) Close() error { return b.file.Close() }

// ReadAll 返回一个按写入顺序逐点产出全部数据的迭代器。
func (b *Block) ReadAll() *Iterator {
	return &Iterator{block: b, chunks: b.index}
}

// ReadRange 返回一个迭代器，逐点产出时间戳落在 [start, end) 内的所有点，
// 包括边界处的全部重复观测。只局部读取并解压覆盖该区间的压缩子块；
// 区间与块数据无交集时不读取任何数据子块。
func (b *Block) ReadRange(start, end int64) *Iterator {
	if start >= end {
		return &Iterator{err: ErrInvalidRange}
	}
	// 子块按时间非递减排列，二分找到第一个 lastTS >= start 的子块，
	// 之后顺序取到 firstTS >= end 为止。
	lo := sort.Search(len(b.index), func(i int) bool {
		return b.index[i].lastTS >= start
	})
	hi := lo
	for hi < len(b.index) && b.index[hi].firstTS < end {
		hi++
	}
	return &Iterator{
		block:     b,
		chunks:    b.index[lo:hi],
		hasRange:  true,
		rangeFrom: start,
		rangeTo:   end,
	}
}

// Iterator 逐点产出查询结果。数据子块按需逐个加载解压。
type Iterator struct {
	block  *Block
	chunks []chunkIndex // 待访问的子块

	hasRange  bool
	rangeFrom int64
	rangeTo   int64

	points []Point // 当前已解压子块
	pos    int     // 当前点在 points 中的下标，-1 表示尚未开始

	point Point
	err   error
	done  bool
}

// Next 前进到下一个点，没有更多点时返回 false。
func (it *Iterator) Next() bool {
	if it.done || it.err != nil {
		return false
	}
	for {
		if it.pos+1 < len(it.points) {
			it.pos++
			p := it.points[it.pos]
			if it.hasRange && (p.Timestamp < it.rangeFrom || p.Timestamp >= it.rangeTo) {
				continue
			}
			it.point = p
			return true
		}
		if !it.loadNextChunk() {
			it.done = true
			return false
		}
	}
}

// loadNextChunk 读取并解压下一个子块。没有更多子块时返回 false。
func (it *Iterator) loadNextChunk() bool {
	if len(it.chunks) == 0 {
		return false
	}
	ci := it.chunks[0]
	it.chunks = it.chunks[1:]
	comp := make([]byte, ci.compLen)
	if _, err := it.block.file.ReadAt(comp, ci.offset); err != nil && err != io.EOF {
		it.err = err
		return false
	}
	raw, err := decompressChunk(comp)
	if err != nil {
		it.err = err
		return false
	}
	points, err := decodeChunkRaw(raw, int(ci.numPoints))
	if err != nil {
		it.err = err
		return false
	}
	it.points = points
	it.pos = -1
	return true
}

// Point 返回当前点，仅在 Next 返回 true 后有效。
func (it *Iterator) Point() Point { return it.point }

// Err 返回迭代过程中发生的错误（如有）。
func (it *Iterator) Err() error { return it.err }
