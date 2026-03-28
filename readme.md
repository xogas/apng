# APNG Go Library

`github.com/xogas/apng` 是一个零依赖的 Go APNG 编解码库。

## APNG 文件格式

### 整体结构

```txt
+---------------+------+------+------+------+------+------+------+------+------+
| PNG Signature | IHDR | acTL | fcTL | IDAT | fcTL | fdAT | fcTL | fdAT | IEND |
+---------------+------+------+------+------+------+------+------+------+------+
```

规范约束：

- acTL **必须** 出现在第一个 IDAT 之前
- fcTL/fdAT 序列号（sequence number）在整个文件中单调递增，从 0 开始
- 第 0 帧的像素数据使用 IDAT 块（与普通 PNG 兼容），后续帧使用 fdAT 块
- IDAT 出现在第一个 fcTL 之前时，代表非 APNG 查看器显示的静态底图（default image）

### Chunk 格式

#### PNG Signature — 8 字节固定魔数

```txt
89 50 4E 47 0D 0A 1A 0A
```

#### 通用 Chunk 结构

```txt
+--------+------------+------+-----+
| Length | Chunk Type | Data | CRC |
+--------+------------+------+-----+
Length:     uint32, Data 字段的字节数（不含 Type 和 CRC）
Chunk Type: 4 字节 ASCII
Data:       Length 字节
CRC:        uint32, 覆盖 Type + Data 的 CRC-32/ISO-HDLC
```

#### IHDR — 13 字节

```txt
+-------+--------+-----------+------------+--------------------+---------------+------------------+
| Width | Height | Bit depth | Color Type | Compression method | Filter method | Interlace method |
+-------+--------+-----------+------------+--------------------+---------------+------------------+
Width:               uint32
Height:              uint32
Bit depth:           uint8, 每通道位深
Color Type:          uint8, 0=灰度 2=RGB 3=调色板 4=灰度+Alpha 6=RGBA
Compression method:  uint8, 必须为 0（Deflate）
Filter method:       uint8, 必须为 0（自适应滤波）
Interlace method:    uint8, 0=无隔行 1=Adam7 隔行扫描
```

#### acTL — 8 字节

```txt
+------------+-----------+
| num_frames | num_plays |
+------------+-----------+
num_frames: uint32, 动画帧总数（不含 default image）
num_plays:  uint32, 循环次数，0 表示无限循环
```

#### fcTL — 26 字节

```txt
+---------+-------+--------+----------+----------+-----------+-----------+------------+----------+
| seq_num | Width | Height | x_offset | y_offset | delay_num | delay_den | dispose_op | blend_op |
+---------+-------+--------+----------+----------+-----------+-----------+------------+----------+
seq_num:    uint32, 单调递增序列号
width:      uint32, 本帧像素宽度（必须 > 0）
height:     uint32, 本帧像素高度（必须 > 0）
x_offset:   uint32, 帧左上角在画布上的 X 偏移
y_offset:   uint32, 帧左上角在画布上的 Y 偏移
delay_num:  uint16, 帧延迟分子
delay_den:  uint16, 帧延迟分母，0 时等价于 100
dispose_op: uint8, 帧显示后画布还原方式（0=保留 1=清透明 2=还原前帧）
blend_op:   uint8, 帧合成方式（0=替换 1=alpha 混合）
```

#### IDAT / fdAT

```txt
IDAT: 标准 PNG 图像数据（Deflate 压缩）
fdAT: uint32 seq_num + IDAT 数据（去掉 seq_num 后等同于 IDAT）
```

**IEND** — 0 字节 Data，仅作为流结束标记。

---

## 公开 API 设计（animation.go）

### 类型设计原则

- **`APNG.Width`/`Height`** 使用 `uint32`：画布尺寸直接映射 IHDR 的 `uint32` 字段，类型本身排除负值，序列化时零转换
- **`Frame.XOffset`/`YOffset`**（偏移量）使用 `int`：参与 `image.Rectangle` 坐标运算（标准库坐标系），减少类型转换
- `LoopCount` 使用 `uint32`：`0` = 无限循环，语义明确；直接映射 acTL 二进制字段
- 移除 `NewAPNG`/`NewFrame` 构造函数，Go 惯用结构体字面量初始化
- 移除 `CanvasSize()` 方法，改为 `Width`/`Height` 直接字段（编码时为 0 则自动推算）
- `Frame.Bounds()` 替代 `Region()`，命名与 `image.Image` 接口一致
- `Frame.Image` 存储**原始帧片段**（非合成图），保留 DisposeOp/BlendOp 的完整语义，实现编解码 round-trip 保真

### 哨兵错误

```go
var (
    // ErrInvalidSignature 表示流开头不是合法的 PNG 签名。
    ErrInvalidSignature = errors.New("apng: invalid PNG signature")

    // ErrNotAPNG 表示文件缺少 acTL chunk，是普通 PNG 而非 APNG。
    ErrNotAPNG = errors.New("apng: not an APNG file (missing acTL chunk)")

    // ErrCRCMismatch 表示某个 chunk 的 CRC-32 校验失败。
    ErrCRCMismatch = errors.New("apng: CRC mismatch")

    // ErrInvalidChunk 表示 chunk 格式不符合规范（如长度错误）。
    ErrInvalidChunk = errors.New("apng: invalid chunk")
)
```

调用方可通过 `errors.Is(err, apng.ErrNotAPNG)` 判断具体错误类型。

### DisposeOp / BlendOp

```go
// DisposeOp 指定帧显示完毕后（进入下一帧之前）画布的还原方式。
type DisposeOp uint8

const (
    DisposeOpNone       DisposeOp = iota // 保留当前画布内容（默认）
    DisposeOpBackground                  // 将帧区域清为全透明
    DisposeOpPrevious                    // 将帧区域还原为前一帧显示之前的状态
)

// BlendOp 指定帧与画布的合成方式。
type BlendOp uint8

const (
    BlendOpSource BlendOp = iota // 直接替换（draw.Src）
    BlendOpOver                  // Alpha 混合（draw.Over）
)
```

### APNG 结构体

```go
// APNG 表示一个 Animated PNG 图像。
type APNG struct {
    // Width/Height 指定画布尺寸（像素）。
    // Encode 时若为 0，自动从所有帧的 (X + frame.width, Y + frame.height) 最大值推算。
    // Decode 时由 IHDR chunk 填充。
    // 使用 uint32 直接映射 IHDR 格式字段，类型本身保证非负。
    Width, Height uint32

    // Frames 是有序的动画帧序列，至少需要 1 帧才能编码。
    Frames []Frame

    // LoopCount 指定动画循环次数，0 表示无限循环。
    LoopCount uint32

    // Background 是可选的静态底图（default image）。
    // 非 APNG 查看器会显示此图像；APNG 查看器忽略它。
    // Encode 时：非 nil 则将其编码为 IDAT，写在第一个 fcTL 之前。
    // Decode 时：IDAT 出现在第一个 fcTL 之前时，解码结果存入此字段。
    Background image.Image
}
```

### Frame 结构体

```go
// Frame 表示动画中的一帧。
// Image 存储帧的原始像素片段（fragment），而非合成后的完整画布。
// 解码时，Image 是从 IDAT/fdAT 直接解码出的片段；
// 编码时，编码器负责将 Image 合成到画布上并做差分优化。
type Frame struct {
    // Image 是本帧的像素内容，坐标系以自身左上角为原点 (0,0)。
    Image image.Image

    // XOffset/YOffset 是帧左上角在画布坐标系中的偏移，必须 >= 0。
    XOffset, YOffset int

    // DelayNum/DelayDen 是帧延迟的分子/分母（单位：秒）。
    // DelayDen 为 0 时等价于 100；即 DelayNum/100 秒。
    DelayNum, DelayDen uint16

    // DisposeOp 指定帧结束后画布的还原方式，默认 DisposeOpNone。
    DisposeOp DisposeOp

    // BlendOp 指定帧的合成方式，默认 BlendOpSource。
    BlendOp BlendOp
}

// Bounds 返回本帧在画布坐标系中占据的矩形区域。
// 等价于 image.Rect(f.XOffset, f.YOffset, f.XOffset+f.Image.Bounds().Dx(), f.YOffset+f.Image.Bounds().Dy())。
// 若 f.Image 为 nil，返回空矩形。
func (f *Frame) Bounds() image.Rectangle

// Delay 返回本帧的显示时长。
// DelayDen 为 0 时按 100 计算，符合 APNG 规范。
func (f *Frame) Delay() time.Duration
```

### 顶层函数

```go
// Decode 从 r 解码一个 APNG。
// 若文件是普通 PNG（无 acTL chunk），返回 ErrNotAPNG。
// 若签名无效，返回 ErrInvalidSignature。
// 若 CRC 校验失败，返回包含 ErrCRCMismatch 的包装错误。
func Decode(r io.Reader) (*APNG, error)

// Encode 将 a 编码为 APNG 格式并写入 w。
// 要求 a.Frames 至少有 1 帧，且所有帧的 X/Y >= 0、Image 非 nil。
// a.Width/Height 为 0 时自动从帧边界推算。
func Encode(w io.Writer, a *APNG) error
```

**注意**：参数顺序 `(w io.Writer, a *APNG)` 与标准库 `png.Encode(w, m)`、`json.NewEncoder(w)` 保持一致。

---

## 内部实现设计

### chunk.go — 底层 I/O 与共用类型

**职责**：chunk 读写、CRC 计算、chunk 名称常量、rawFrame 定义、pngHeader。

```go
// PNG 文件签名
var pngHeader = []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}

// Chunk 名称常量，避免魔术字符串
const (
    chunkIHDR = "IHDR"
    chunkIDAT = "IDAT"
    chunkIEND = "IEND"
    chunkACTL = "acTL"
    chunkFCTL = "fcTL"
    chunkFDAT = "fdAT"
)

// rawFrame 是 fcTL 二进制字段的直接映射，字段类型与 APNG 规范一致。
// 仅在解码器内部使用，不对外暴露。
type rawFrame struct {
    seqNum             uint32
    width, height      uint32
    xOffset, yOffset   uint32
    delayNum, delayDen uint16 // 注意：规范中是 uint16，不是 uint32
    disposeOp, blendOp uint8
}

// readChunk 从 r 读取一个 PNG chunk。
// 返回 chunk 类型名（4 字节 ASCII）和 data 载荷。
// CRC 校验失败时返回包含 ErrCRCMismatch 的包装错误。
// 流结束时返回 io.EOF。
func readChunk(r io.Reader) (typ string, data []byte, err error)

// writeChunk 向 w 写入一个完整的 PNG chunk（length + type + data + CRC）。
func writeChunk(w io.Writer, typ string, data []byte) error
```

---

### reader.go — 解码器

**职责**：实现 `Decode` 的全部逻辑，chunk 状态机 → 帧提交 → 填充 `APNG`。

#### 解码器状态机

```txt
初始状态
   |
   v
读取签名 --失败--> ErrInvalidSignature
   |
  成功
   |
   v
stateHaveIHDR ---- IDAT ----> stateDefaultImage（收集 default image 的 IDAT）
   |                                |
   | acTL                           | fcTL -> commitFrame(default image -> Background)
   |                                |
   |                                v
   +------------- fcTL ---------> stateInFrame（收集当前帧的 IDAT/fdAT）
                                    |
                                    +- fcTL -> commitFrame(前帧) -> 切换到新帧
                                    +- IEND -> commitFrame(最后帧) -> stateDone
```

#### decoder 结构体

```go
type decoder struct {
    r    io.Reader
    apng *APNG    // 由 parseACTL 初始化
    ihdr *ihdrData // 由 parseIHDR 填充，用于重建每帧的合法 PNG 字节流

    state       decodeState
    idatBuffs   [][]byte  // 累积当前帧（或 default image）的原始 IDAT 载荷
    curRawFrame *rawFrame // 当前帧的 fcTL 元数据；nil 表示正在收集 default image
}

type ihdrData struct {
    width, height                          uint32
    bitDepth, colorType, compress, filter, interlace uint8
}
```

#### 方法集

```go
// newDecoder 读取并验证 PNG 签名，返回初始化好的 decoder。
func newDecoder(r io.Reader) (*decoder, error)

// decode 驱动主循环，逐 chunk 处理，返回完整的 APNG。
func (d *decoder) decode() (*APNG, error)

// parseIHDR 解析 IHDR chunk（期望 13 字节），填充 d.ihdr 和 d.apng.Width/Height。
// 出现多个 IHDR 时返回错误。
func (d *decoder) parseIHDR(data []byte) error

// parseACTL 解析 acTL chunk（期望 8 字节），初始化 d.apng。
// 出现多个 acTL 时返回错误。
func (d *decoder) parseACTL(data []byte) error

// parseFCTL 解析 fcTL chunk（期望 26 字节）。
// 调用 commitFrame() 提交之前积累的数据，然后设置 d.curRawFrame。
func (d *decoder) parseFCTL(data []byte) error

// parseIDAT 处理 IDAT chunk，将载荷追加到 d.idatBuffs。
// 若在 stateHaveIHDR 时收到第一个 IDAT，状态切换为 stateDefaultImage。
func (d *decoder) parseIDAT(data []byte) error

// parseFDAT 处理 fdAT chunk，剥去 4 字节序列号后追加到 d.idatBuffs。
// 仅在 stateInFrame 时有效，其他状态下忽略。
func (d *decoder) parseFDAT(data []byte) error

// parseIEND 处理 IEND chunk，调用 commitFrame() 提交最后一帧。
func (d *decoder) parseIEND() error

// commitFrame 将 d.idatBuffs 中的数据重建为完整 PNG 并解码。
// 若 d.curRawFrame == nil（default image），解码结果写入 d.apng.Background。
// 否则解码结果构造 Frame 追加到 d.apng.Frames。
// idatBuffs 为空时为 no-op。
func (d *decoder) commitFrame() error

// decodeImageFromIDAT 重建一个最小合法 PNG（signature + IHDR + merged IDAT + IEND），
// 然后调用 png.Decode 解码，返回 image.Image。
// IHDR 中的宽高优先使用 curRawFrame 的 width/height（帧片段尺寸），
// 其余字段（bitDepth、colorType 等）来自全局 d.ihdr。
func (d *decoder) decodeImageFromIDAT() (image.Image, error)
```

#### 错误处理规范

所有错误统一使用 `fmt.Errorf("apng: <context>: %w", err)` 包装，确保 `errors.Is` 可穿透。

---

### writer.go — 编码器

**职责**：实现 `Encode` 的全部逻辑，帧规划（差分/裁剪）→ 写 chunk。

#### 编码流程

```txt
Encode(w, a)
   |
   +--校验：Frames 非空 / 所有帧 Image 非 nil / 所有帧 X,Y >= 0
   |
   +--计算画布尺寸：Width/Height > 0 直接用；否则遍历帧推算最大边界
   |
   +-- 写 PNG Signature
   +-- 写 IHDR（画布尺寸，色彩参数来自第 0 帧编码结果）
   +-- 写 acTL（帧数 + LoopCount）
   |
   +--（可选）写 Background → IDAT（若 a.Background 非 nil）
   |
   +-- 逐帧循环（i=0,1,2,...）
         |
         +-- planFrame：composite -> diffRegion -> crop -> encodeImageToIDAT
         +-- 写 fcTL（seq++）
         +-- i==0：写 IDAT（*）；i>0：写 fdAT（seq++）
         +-- 根据 DisposeOp 更新 prevCanvas
   |
   +-- 写 IEND

(*) 若 a.Background 非 nil，则第 0 帧的像素数据已通过 Background 写入，
    此处仍按规范写 fcTL + IDAT（第 0 帧 IDAT 是动画帧像素，与 Background IDAT 并列）。
```

#### encoder 结构体

```go
type encoder struct {
    w    io.Writer
    apng *APNG
}

// framePlan 存储单帧编码所需的全部派生数据。
type framePlan struct {
    frame          Frame
    idatPayloads   [][]byte // 差分裁剪后的图像编码为 IDAT 载荷
    width, height  uint32   // 差分区域的像素尺寸（写入 fcTL）
    xOffset, yOffset uint32 // 差分区域在画布上的绝对偏移（写入 fcTL）
    nextPrevCanvas *image.RGBA // 本帧结束后 prevCanvas 的状态（DisposeOp 应用后）
}

// seqCursor 维护 fcTL/fdAT 的单调递增序列号。
type seqCursor struct{ next uint32 }
func (c *seqCursor) nextSeq() uint32
```

#### 方法集

```go
// encode 是编码主流程，按上述流程写入所有 chunk。
func (e *encoder) encode() error

// canvasSize 返回画布宽高。
// 若 e.apng.Width/Height > 0 直接返回；否则遍历帧推算。
func (e *encoder) canvasSize() (w, h int)

// writeHeaderChunks 写入 Signature + IHDR + acTL。
// refIHDR 是从第 0 帧编码结果中提取的 IHDR 载荷，用于获取色彩参数。
func (e *encoder) writeHeaderChunks(refIHDR []byte, canvasW, canvasH int, frameCount uint32) error

// planFrame 完成单帧的差分规划：
//   1. compositeOnto：将 frame.Image 按 BlendOp 合成到 prevCanvas，得到 currCanvas
//   2. diffRegion：找出 prevCanvas 和 currCanvas 之间的变化包围盒（画布坐标）
//   3. 将包围盒转换为帧本地坐标，裁剪 frame.Image
//   4. encodeImageToIDAT：将裁剪后的子图编码为 IDAT 载荷
//   5. deriveNextPrevCanvas：按 DisposeOp 计算下一帧的 prevCanvas
func (e *encoder) planFrame(frame Frame, prevCanvas *image.RGBA, canvasW, canvasH int) (*framePlan, error)

// writeFCTL 写入一个 fcTL chunk。
func (e *encoder) writeFCTL(plan *framePlan, seq uint32) error

// writeFramePayload 写入帧像素数据：
// - firstFrame=true：写 IDAT（不消耗 seq）
// - firstFrame=false：写 fdAT（每个 payload 消耗一个 seq）
func (e *encoder) writeFramePayload(payloads [][]byte, seq *seqCursor, firstFrame bool) error

// encodeImageToIDAT 用标准库 png.Encode 将 img 编码为 PNG，
// 然后调用 extractIHDRAndIDAT 提取 IHDR 载荷和所有 IDAT 载荷。
func (e *encoder) encodeImageToIDAT(img image.Image) (ihdr []byte, idatPayloads [][]byte, err error)

// extractIHDRAndIDAT 从 chunk 流（已跳过签名）中提取 IHDR 和 IDAT 载荷。
func extractIHDRAndIDAT(r io.Reader) (ihdr []byte, idatPayloads [][]byte, err error)

// compositeOnto 将 frame.Image 按 frame.BlendOp 合成到 prevCanvas（克隆），
// prevCanvas 为 nil 时创建空白画布。
func compositeOnto(prev *image.RGBA, canvasW, canvasH int, frame Frame) *image.RGBA

// diffRegion 返回 prev 和 curr 之间有变化的像素包围盒（画布坐标）。
// prev 为 nil 时返回 curr 的全部 Bounds。
// 实现：按行比较 curr.Pix 和 prev.Pix 的对应行切片（bytes.Equal），
//       先确定 minY/maxY，再对有变化的行扫描 minX/maxX。
//       利用底层字节比较，比逐像素 RGBAAt 快 1~2 个数量级。
// 若无差异，返回 1x1 区域（APNG 不允许零尺寸帧）。
func diffRegion(prev, curr *image.RGBA) image.Rectangle

// deriveNextPrevCanvas 按 DisposeOp 确定本帧结束后 prevCanvas 的状态：
//   DisposeOpNone:       currCanvas（保留当前合成结果）
//   DisposeOpBackground: 克隆 currCanvas 并将帧区域清为透明
//   DisposeOpPrevious:   prevCanvas（还原为本帧之前的状态）
func deriveNextPrevCanvas(frame Frame, prevCanvas, currCanvas *image.RGBA) *image.RGBA

// rebuildIHDR 复制 ref IHDR 载荷并替换其中的 width/height 字段。
func rebuildIHDR(ref []byte, width, height uint32) []byte

// 以下为图像工具函数（包级，不导出）
func cropImage(src *image.RGBA, r image.Rectangle) *image.RGBA  // 裁剪并归零坐标
func toRGBA(src image.Image) *image.RGBA                         // 转换为 Min=(0,0) 的 *image.RGBA
func cloneRGBA(src *image.RGBA) *image.RGBA                     // 深拷贝
func clearRegionRGBA(img *image.RGBA, r image.Rectangle)        // 将区域清为全透明
```

---

## 项目文件结构

```txt
animation.go  — 公开 API：APNG、Frame、DisposeOp、BlendOp、Decode()、Encode()、哨兵错误
chunk.go      — 底层基础：pngHeader、chunk 名称常量、readChunk()、writeChunk()、rawFrame
reader.go     — 解码实现：decoder 结构体及全部方法
writer.go     — 编码实现：encoder 结构体及全部方法、图像工具函数
apng_test.go  — 测试：round-trip、DisposeOp 正确性、错误路径、边界值
```

---

## 关键设计决策

| 决策 | 选择 | 理由 |
| :--- | :--- | :--- |
| `APNG.Width`/`Height` 类型 | `uint32` | 直接映射 IHDR 二进制字段；类型本身排除负值；用户无法写出 `Width: -1` 这类无意义值 |
| `Frame.X`/`Y` 类型 | `int` | 参与 `image.Rectangle` 坐标运算，与标准库坐标系一致；偏移量在算术运算中可为中间负值 |
| rawFrame 内部坐标类型 | `uint32` | 直接映射 fcTL 二进制字段，序列化边界保持清晰 |
| `LoopCount` 类型 | `uint32` | 0 = 无限循环，语义明确；允许负数的 `int` 会引入歧义 |
| `Frame.Image` 内容 | 原始片段（fragment） | 保留 DisposeOp/BlendOp 语义，round-trip 保真；合成逻辑收敛在编码器内部 |
| default image | `APNG.Background` 字段 | 消除 `Frame.IsDefault bool` 的类型污染；Background 与动画帧是不同语义 |
| `Encode` 参数顺序 | `(w io.Writer, a *APNG)` | 与 `png.Encode(w, m)`、`json.NewEncoder(w)` 一致 |
| 构造函数 | 删除 `NewAPNG`/`NewFrame` | Go 惯用结构体字面量；构造函数无附加逻辑时是噪音 |
| `CanvasSize()` 方法 | 删除，改为 `Width`/`Height` 字段 | 字段直接可读；编码时 0 值触发自动推算 |
| `Region()` 方法 | 改名为 `Bounds()` | 与 `image.Image` 接口命名一致 |
| `diffRegion` 实现 | `bytes.Equal` 行级比较 | 相比逐像素 `RGBAAt`，减少函数调用并触发 CPU SIMD 向量化 |
| 错误包装 | `fmt.Errorf("...: %w", err)` | 统一风格，支持 `errors.Is`/`errors.As` 穿透 |
