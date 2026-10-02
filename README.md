# chronobase

ChronoBase 封存时序块存储库（纯 Go，仅标准库，无外部依赖、无服务进程）。

边缘设备把停止写入的小时级遥测块转为压缩归档文件；运维侧可完整顺序读回，
或对短区间做**局部读取、局部解压**。每块只含一个指标的一条来源序列，
所有点属于同一 UTC 小时。浮点值逐位（`math.Float64bits`）无损保存：
±0、±Inf、非规格化数、携带不同载荷的 NaN 均原样往返。

## 文件格式

```
[头部]  magic | metaLen | meta JSON（指标、来源、UTC 小时、帧粒度）
[数据帧] 每帧默认 1024 点，独立 flate 压缩（时间戳差值 varint + 值原始位型）
[索引]  每帧一条：首/末时间戳、点数、文件偏移、压缩长度
[尾部]  索引偏移 | 帧数 | magic
```

区间读取按索引跳过不重叠的帧，只读取并解压覆盖该区间的帧。

## 使用

```go
// 写入：按时间戳非递减顺序逐点提供，相同时间戳可重复（各自独立观测）。
w, _ := chronobase.Create("cpu-20261002T08Z.chb", chronobase.Meta{
    Metric: "cpu.usage", Source: "edge-7",
    Hour: chronobase.HourStart(ts),
})
for _, p := range points {
    _ = w.Add(p.Timestamp, p.Value) // ts 必须非递减且属于该 UTC 小时
}
_ = w.Close() // 封存：写索引与尾部，fsync 后原子落盘，之后不再修改

// 读取：关闭后重新打开仍可独立读取。
r, _ := chronobase.Open("cpu-20261002T08Z.chb")
defer r.Close()

for p, err := range r.All() { /* 完整顺序读回，逐点消费 */ }

// 区间 [start, end)，左闭右开，含边界处全部重复观测；只局部解压重叠帧。
for p, err := range r.Range(start, end) { /* ... */ }
```

- `Create(path, meta)` / `CreateFrameSize(path, meta, n)`：创建封存块（默认 1024 点/帧）。
- `Open(path)`：打开封存块；`Meta()`、`Len()` 读取元数据。
- `All()` / `Range(start, end)`：返回 `iter.Seq2[Point, error]`，逐点消费，可提前终止。
- `FramesRead()`：已解压帧数，用于观察区间读取的局部性。

## 测试

```sh
go test ./...
```

覆盖：特殊浮点位型（±0、±Inf、非规格化数、多载荷 NaN）关闭重开后的逐位往返；
长串重复时间戳两侧及内部的区间读取（含空结果与块尾部区间）；
重新打开后的局部读取行为（区间结果与直接过滤原始序列逐点一致）。
