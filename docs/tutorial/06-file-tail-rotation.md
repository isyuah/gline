# 06. 文件 Tail、Checkpoint 与轮转恢复

> 本章描述目标实现。当前 `internal/agent/source/file.go` 只支持“打开时跳到文件末尾并轮询新增完整行”，尚未实现本章的 checkpoint、文件身份和轮转状态机。不要把文中的目标保证写成当前能力。

## 1. 本章目标

完成本章后，你应该能够：

1. 从字节 offset 而不是字符串长度计算恢复位置；
2. 用平台 adapter 在 Windows 和 Unix/Linux 上取得文件身份；
3. 正确恢复同一文件、运行中 rename + recreate、停机期间轮转和 truncate；
4. 解释为什么路径不是文件身份，为什么一次 EOF 不代表文件结束；
5. 让每条 `RawRecord` 携带“本条之后的位置”，并只在 batch + checkpoint 事务成功后建立 durable 进度；
6. 对半行、超长行、CRLF、非法 UTF-8 和缺失文件制定显式策略；
7. 诚实描述 `copytruncate` 无法提供绝对零丢失的原因；
8. 在 Windows 与 Linux 上用真实文件操作验证轮转，而不是只使用 mock。

## 2. 当前代码定位

当前实现位于 `internal/agent/source/file.go`：

- `NewFileSource` 立即 `os.Open` 并 `Seek(0, io.SeekEnd)`；
- `bufio.Reader.ReadString('\n')` 读取行；
- EOF 后每秒 `time.After` 再试；
- 半行暂存在 `strings.Builder`；
- `Close` 关闭当前文件。

它有一个重要的正确点：只有读到换行才返回完整记录，EOF 时保留 pending 半行。它仍有这些目标缺口：

- 每次启动无条件跳末尾，无法从 durable checkpoint 恢复；
- `RawRecord` 没有文件 identity 和 byte offset；
- 当前 path 被 rename 后不会发现 replacement；
- `copytruncate` 后 reader/offset 不会重置；
- `ReadString` 对单行大小没有上限；
- `time.After` 在轮询循环中反复分配 timer；
- 构造在 `Seek` 失败时没有关闭已经打开的 file；
- 只保存一个当前 handle，未定义旧文件稳定 EOF 与新文件切换顺序。

本章建议保留 `Source.NextRecord(ctx)` 这种拉取接口，但重写 FileSource 内部状态机。

## 3. 前置知识

需要理解：

- 文件路径是目录项，打开的 handle 指向文件对象；
- Unix 的 device + inode 与 Windows 的 volume + file ID；
- `bufio.Reader` 可能从底层文件预读，因此 `file.Seek(0, io.SeekCurrent)` 不等于逻辑消费位置；
- UTF-8 字符长度与磁盘字节长度不同；
- rename rotation 与 copytruncate 是两种完全不同的轮转机制；
- [第五章](./05-spool-checkpoint.md)中的 volatile cursor 和 durable checkpoint；
- [可靠性文档的 FileSource 边界](../05-reliability-security-observability.md#4-filesource-与-checkpoint)。

## 4. 首先定义保证范围

建议把用户可见保证写成：

> 对仍可访问的普通追加文件，Gline 使用文件身份和 durable checkpoint 恢复；每个完整记录只有在对应 batch 与 checkpoint 同一 spool 事务提交后才被视为本地安全。运行中的 rename + recreate 通过保留旧 handle 读到稳定 EOF 后切换。copytruncate 存在轮转工具自身的不可消除竞态，Gline 检测后恢复跟随，但不承诺该场景绝对零丢失。

这个保证不覆盖：

- 业务日志库尚未 flush 的用户态缓冲；
- 文件系统或磁盘永久损坏；
- Agent 停机期间旧轮转文件已被删除且无法按 identity 找回；
- copytruncate 的复制与截断窗口；
- 用户显式选择 `start_position: end` 跳过的历史字节；
- 用户显式 reset checkpoint 或选择 drop policy 的数据。

明确边界比声称“任何情况下零丢失”更专业，也更容易设计可验证实验。

## 5. 核心定义与不变量

### 5.1 文件身份

路径 `C:\logs\app.log` 或 `/var/log/app.log` 只是“当前这个名字指向谁”。轮转后，相同路径可能指向一个新文件；旧 handle 仍可能指向被改名的旧文件。

建议使用可序列化的中立值：

```go
type FileIdentity struct {
	Kind string // "windows-file-id" 或 "unix-device-inode"
	A    uint64
	B    uint64
}

func (id FileIdentity) Equal(other FileIdentity) bool {
	return id.Kind == other.Kind && id.A == other.A && id.B == other.B
}
```

字段名 `A/B` 便于跨平台持久化，但业务日志中应格式化成可读的 hash/短字符串，不必暴露底层细节。

### 5.2 字节位置

```go
type Position struct {
	Identity FileIdentity
	Offset   int64
}
```

`Offset` 是从文件开头计算的字节数，表示下一次读取起点。它不是：

- rune 数；
- UTF-8 字符数；
- `len(strings.TrimSpace(line))`；
- `os.File` 因 bufio 预读后报告的内核 cursor。

读取 `"你好\r\n"` 时，内容可以去掉 `\r\n`，但 offset 必须包含原始编码字节和两个换行字节。

### 5.3 不变量

| 编号 | 不变量 |
| --- | --- |
| F1 | 每个 RawRecord 的 `After` 是消费原始字节后的下一位置 |
| F2 | durable checkpoint 只来自已成功提交到 spool 的 batch 最后一条 `After` |
| F3 | pending 半行不推进 durable checkpoint |
| F4 | 同 identity 的正常追加 offset 单调递增 |
| F5 | identity 改变必须经过显式 rotate transition，不能沿用旧 offset |
| F6 | identity 相同但 size 小于 volatile/durable offset 是 truncate 信号，不是普通 EOF |
| F7 | 第一版 batch 不跨文件 identity；切换前先 flush 旧 identity 的完整记录 |
| F8 | 一次 EOF 只表示当前没有更多可读字节，不表示文件永远结束 |
| F9 | 轮转候选扫描有目录、glob、数量和时间上限 |
| F10 | FileSource 只关闭自己拥有的 handle，且所有退出路径都关闭 |

## 6. 平台文件身份 Adapter

不要在跨平台主状态机里到处写 runtime 条件。定义小接口：

```go
type IdentityProvider interface {
	FromFile(file *os.File) (FileIdentity, error)
	FromPath(path string) (FileIdentity, error)
}
```

`FromPath` 的实现最好是打开文件、从 handle 取 identity、再关闭；只依赖 path stat 信息在 Windows 上可能不够一致。为避免检查与打开之间的 TOCTOU，真正开始读取时必须以已打开 handle 的 `FromFile` 结果为准。

### 6.1 Linux/Unix

常见 identity 是：

```text
(device number, inode number)
```

建议第一批明确支持 Linux，而不是写一个过宽的 `//go:build !windows` 然后假设所有系统的 `Stat_t` 都相同：

```text
identity_linux.go      // Linux 的 dev + inode
identity_windows.go    // Windows volume + file ID
identity_unsupported.go
```

未来扩展 macOS/BSD 时再加入对应 build tag 和测试。若使用 `golang.org/x/sys/unix`，通过包管理器加入兼容版本，不直接编辑 lock 数据。

inode 可能在文件删除后被复用，因此 identity 不是跨无限时间的内容 ID。恢复时还应结合配置 fingerprint、路径范围、文件大小和合理的 checkpoint 校验；找不到可信候选时默认失败并要求人工选择，不随便跳到新文件。

### 6.2 Windows

Windows 可从已打开 handle 获取：

```text
(volume serial number, file index / file ID)
```

可通过 `golang.org/x/sys/windows` 的 handle API 实现。不要把创建时间 + 文件大小拼成身份：创建时间分辨率、复制行为和快速重建都可能碰撞。

Windows 轮转还受共享模式影响。目标行为必须用当前 Go 版本实际 `os.Open` 的 rename/delete 语义验证；若需要定制 share flags，应只放在 Windows adapter 中，主状态机不感知系统调用细节。

### 6.3 Adapter 错误

身份读取错误必须带 operation 和脱敏 path：

```go
return FileIdentity{}, fmt.Errorf("identify open file %q: %w", safePath, err)
```

不要记录 handle 数值或把错误统一改成 “file unavailable”。调用方需要通过 `errors.Is(err, fs.ErrNotExist)` 等进行临时/致命分类。

## 7. 推荐类型骨架

### 7.1 配置

```go
type StartPosition string

const (
	StartBeginning StartPosition = "beginning"
	StartEnd       StartPosition = "end"
)

type PartialLinePolicy string

const (
	PartialKeep        PartialLinePolicy = "keep"
	PartialEmitOnRotate PartialLinePolicy = "emit_on_rotate"
	PartialDiscard     PartialLinePolicy = "discard"
)

type FileSourceOptions struct {
	Path                string
	StartPosition       StartPosition
	PollInterval        time.Duration
	StableEOFWindow     time.Duration
	MaxLineBytes        int
	PartialLinePolicy   PartialLinePolicy
	RotationGlob        string
	MaxRotationCandidates int
}
```

配置校验必须拒绝零/负 interval、过小 max line、glob 越出配置目录等危险组合。`discard` 必须是显式选择并有丢弃指标。

### 7.2 打开状态

```go
type openFile struct {
	file       *os.File
	reader     *bufio.Reader
	identity   FileIdentity
	readOffset int64
	pending    []byte
	lastSize   int64
	eofSince   time.Time
}

type FileSource struct {
	opts       FileSourceOptions
	identity   IdentityProvider
	clock      Clock
	current    *openFile
	state      State
	checkpoint checkpoint.Value
}
```

`readOffset` 由“实际从 reader 交付给行组装器的字节数”维护，不调用 `file.Seek(Current)` 猜测。每次重新 Seek 后都用 `reader.Reset(file)` 清空旧 buffer。

### 7.3 状态

```go
type State string

const (
	StateOpening            State = "opening"
	StateFollowing          State = "following"
	StateDrainingRotated    State = "draining_rotated"
	StateWaitingReplacement State = "waiting_replacement"
	StateStopped            State = "stopped"
	StateFailed             State = "failed"
)
```

状态变化应集中在少数方法，不要在每个错误分支随意改多个 bool。

## 8. 启动与恢复算法

### 8.1 没有 checkpoint

打开配置 path，取得 handle identity 和 size：

- `start_position: beginning`：seek 0；
- `start_position: end`：seek 当前 size，明确跳过此前内容。

`end` 的初始 anchor 可以写入一个显式 `initialized` checkpoint 事务。它不是“某个 batch 已持久化”的证明，而是用户策略明确选择跳过历史数据。这个初始化例外必须使用不同的 reason 标识；此后任何因消费记录产生的 checkpoint 推进仍必须与 batch 同事务。

若创建 anchor 前崩溃，再启动时会重新以当时文件末尾为起点，可能继续跳过停机期间数据。若你希望 `end` 首次选择后立即固定边界，就必须持久化这个 anchor。

### 8.2 checkpoint identity 与当前 path 相同

1. 打开 path；
2. 从 handle 获取 identity；
3. identity 相同且 `size >= checkpoint.offset`：seek 到 offset；
4. identity 相同但 `size < checkpoint.offset`：进入 validated truncate；
5. Reset reader，令 `readOffset` 等于 seek 位置；
6. 开始 following。

不要先对 path stat，再另行 open 并信任第一次结果；两步之间可能发生轮转。以最终 handle identity 为准。

### 8.3 checkpoint identity 与当前 path 不同

这意味着 Agent 停机期间可能发生 rename + recreate。若直接对当前新文件使用旧 offset，会跳过或错读。

推荐流程：

1. 在配置限定的目录和 `rotation_glob` 中扫描最多 N 个候选；
2. 逐个打开并读取 identity；
3. 找到 checkpoint identity 时，从 checkpoint offset 继续旧文件；
4. 旧文件稳定 EOF 后，再切换当前配置 path；
5. 找不到时默认将 Pipeline 标记 failed，报告 `checkpoint_file_missing`；
6. 用户可显式执行 reset/accept-gap，再按新文件初始策略继续。

不要在找不到旧 identity 时静默从新文件 0 开始并声称无丢失。旧文件可能还有未进入 spool 的尾部。

候选排序不能只靠文件名字符串。先按 identity 精确匹配旧文件；多个新候选的追赶顺序需要结合 rotation 配置和时间信息。第一版可以只保证“旧 checkpoint 文件 + 当前 path”这一级，并把多次停机轮转列为已知限制。

## 9. 逐行读取算法

### 9.1 为什么不用无界 `ReadString`

日志行可能因为应用 bug 达到数百 MB。`ReadString('\n')` 会不断扩容，违背有界资源原则。

推荐使用固定 reader buffer + `ReadSlice('\n')`：

```go
func (s *openFile) readLine(max int) (content []byte, after int64, complete bool, err error) {
	for {
		fragment, readErr := s.reader.ReadSlice('\n')
		s.readOffset += int64(len(fragment))
		if len(s.pending)+len(fragment) > max {
			return nil, 0, false, ErrLineTooLong
		}
		s.pending = append(s.pending, fragment...)

		switch {
		case readErr == nil:
			line := trimLineEnding(s.pending)
			s.pending = s.pending[:0]
			return line, s.readOffset, true, nil
		case errors.Is(readErr, bufio.ErrBufferFull):
			continue
		case errors.Is(readErr, io.EOF):
			return nil, s.readOffset, false, nil
		default:
			return nil, 0, false, readErr
		}
	}
}
```

这是算法骨架，不是可直接复制的最终代码。需要特别处理：

- EOF 时 reader 可能已经返回非空 fragment，必须保留；
- `pending` 的容量在清空后可能长期持有大数组，可在阈值后释放；
- `ErrLineTooLong` 后必须定义如何消费直到下一换行，不能留在未知中间状态；
- 返回给 Parser 前执行 UTF-8 策略；
- `trimLineEnding` 只去掉一个尾部 `\n` 及其前一个 `\r`，不使用 `TrimSpace`。

### 9.2 RawRecord

```go
return RawRecord{
	ObservedAt: s.clock.Now(),
	Content:    string(validatedBytes),
	After: Position{
		Identity: s.current.identity,
		Offset:   after,
	},
	Partial: false,
}, nil
```

当 Parser 失败并降级为 Unknown entry 时，`After` 不能丢失。否则 batch 中有日志但 checkpoint 不知道消费到哪里。

### 9.3 空行与 CRLF

- `"\n"` 是一条内容为空的完整记录，是否过滤应由显式 parser/filter 策略决定；
- `"\r\n"` 的内容为空，但 offset 增加 2；
- 文件最后没有换行时属于半行，不因为 EOF 自动当完整行。

## 10. 运行中 rename + recreate

### 10.1 检测

在当前 handle 到达 EOF 后，同时检查：

- 当前 handle 的 identity、size；
- 配置 path 现在指向的 identity、size；
- 当前 identity 与 path identity 是否相同。

若 path identity 不同：

```text
following(old handle)
  -> draining_rotated(old handle)
  -> old handle stable EOF
  -> flush old-identity batch to spool
  -> open current path and verify new identity
  -> persist rotate anchor(new identity, offset 0)
  -> close old handle and switch to verified new handle
  -> following(new handle)
```

第一版顺序 drain 旧文件再读新文件，保持 Pipeline 内顺序并避免一个 checkpoint 同时描述两个 active identity。新文件可能在等待期间增长，但只要仍存在，就可以随后追赶。

### 10.2 稳定 EOF

一次 EOF 不够，因为应用可能暂时没有写入。稳定 EOF 可以定义为：

- 已确认配置 path 指向不同 identity；
- 旧 handle 连续处于 EOF；
- 旧 handle size 在 `stable_eof_window` 内没有增长；
- context 未取消；
- pending 半行已按显式 rotation policy 处理。

避免只用 `Sleep(window)` 阻塞。使用可复用 timer，并在新数据到来或状态变化时 reset。测试通过 fake clock 或抽出的 `scanOnce` 驱动。

### 10.3 切换点与 batch

在关闭旧 handle 前，先将属于旧 identity 的完整记录 batch 提交到 spool。不要让一个 batch 同时包含旧 identity 与新 identity 的 entry。

旧文件稳定 EOF 且 pending 半行已经按策略处理后，即使新文件还没有完整行，也应持久化一个显式 `rotate` anchor：compare-and-set 旧 checkpoint，并把新 identity 的 offset 0 写入同一 spool 数据库。这个控制过渡没有消费新文件字节，所以不需要伪造空 batch；它必须走第五章受限的 `Transition` API，不能使用任意 checkpoint setter。

这样，若进程在切换 handle 后、第一条新日志到来前崩溃，重启会从新 identity 的 offset 0 恢复，而不再依赖可能已被删除的旧轮转文件。若在 rotate anchor 提交前崩溃，则旧 checkpoint 仍有效，恢复逻辑继续查找旧 identity；系统不能把未提交的内存状态当作已完成切换。

## 11. 路径暂时缺失与快速多次轮转

### 11.1 路径缺失

常见轮转过程可能短暂出现：旧文件已 rename，新文件尚未 create。此时：

- 继续 drain 已打开的旧 handle；
- 配置 path `not exist` 视为 temporary；
- 在有界、可取消的 poll/backoff 中等待 replacement；
- 不关闭旧 handle 后立刻把 Pipeline 判 fatal；
- 超过可配置告警时间后 readiness 可降级，但仍可继续等待。

### 11.2 快速多次轮转

若应用在 Agent drain 旧文件期间又轮转多次，当前 path 只显示最新文件，中间文件可能丢失于发现范围之外。要完整支持，需要目录候选发现与有序待跟随文件队列。

推荐迭代：

1. MVP：保证单次运行中 rename + recreate；
2. 下一步：扫描限定 glob，按 identity 去重并维护有界 successor queue；
3. 明确 `max_rotation_candidates` 和异常轮转告警；
4. 用高速轮转集成测试再宣称支持多次追赶。

不要在只处理一个 replacement 的代码上写“任意轮转不丢”。

## 12. Copytruncate

### 12.1 检测与恢复

若配置 path 与当前 handle identity 相同，但 `size < readOffset`：

1. 记录 `copytruncate_detected` 指标/事件；
2. 处理 pending 半行策略；
3. compare-and-set 当前 checkpoint，持久化同 identity、offset 0 的 validated truncate transition；
4. `file.Seek(0, io.SeekStart)`；
5. `reader.Reset(file)`，清除旧 buffer；
6. `readOffset = 0`；
7. 继续 following。

若 transition 提交后、内存 reader reset 前崩溃，重启会从持久化的 offset 0 恢复；若提交前崩溃，旧 checkpoint 仍会再次触发检测。若 durable checkpoint offset 大于新 size，启动恢复也走同样的 validated truncate 分支，而不是直接 Seek 到越界位置等待。

### 12.2 为什么不能承诺绝对零丢失

典型 copytruncate：

```text
复制 app.log -> app.log.1
                 [竞态窗口：应用仍可能写 app.log]
截断 app.log 到 0
```

竞态窗口中的新字节可能没有进入副本，又在截断时消失。Agent 即使频繁 poll 也不一定来得及读取。

还有更隐蔽的情况：截断后应用快速写入，使文件 size 在下一次检查时已经重新超过旧 offset。仅用 `size < offset` 甚至可能检测不到发生过 truncate。可以用文件内容窗口指纹、事件 API 或更频繁检查提高检测率，但不能消除轮转方式的根本竞态。

因此 Gline 应：

- 推荐 rename + recreate；
- 记录检测次数和最近时间；
- 文档明确 copytruncate 的弱保证；
- 使用连续序号发生器量化实际 gap；
- 不把 copytruncate 测试偶尔通过写成绝对保证。

## 13. 半行策略

### 13.1 正常 EOF

正常 following 状态下，EOF 的 pending bytes 保留在内存，等待后续追加换行。durable checkpoint 仍停在最近完整行之后，因此崩溃会从那里重读整个半行。

### 13.2 轮转或 shutdown

必须明确配置策略：

| 策略 | 行为 | 代价 |
| --- | --- | --- |
| `keep` | 等待旧文件继续出现换行 | 轮转后可能永久无法完成，需 timeout/人工处理 |
| `emit_on_rotate` | 将 pending 作为 `partial=true` 记录 | 下游看到非完整行，但字节不会静默消失 |
| `discard` | 丢弃并记录 byte count | 明确有损，只能用户显式选择 |

推荐演示配置使用 `emit_on_rotate`，Parser 失败时走 Unknown entry，并在 attributes 中标记 partial。是否将它作为永久产品默认值属于策略，不必用过细测试锁死；真正稳定的合同是“不静默处理”。

### 13.3 shutdown deadline

取消发生时：

- 已形成的完整行 batch 应尝试在本地 deadline 内 commit；
- 未完成半行按 shutdown policy 处理；
- 若保留不提交，durable checkpoint 不前进，重启会重读；
- 不等待文件未来出现换行。

## 14. 超长行与编码

### 14.1 超长行

默认建议：达到 `max_line_bytes` 后停止该 Pipeline，报告 `line_too_long`，不推进 checkpoint。这样不会静默丢失，但需要用户提高上限或显式选择有损策略。

可选策略：

- `truncate_and_emit`：消费到换行，发送截断内容并标记原始 byte count；
- `chunk`：拆成多个带 chunk metadata 的 entry，协议和查询会更复杂；
- `discard`：消费到换行，记录丢失指标。

第一版不要同时实现三种。选一个默认安全行为，再根据真实需求扩展。

关键是：检测超限后仍要知道如何重新同步到下一换行。若直接返回错误但下次继续从同一 reader buffer 中间位置，状态可能不可解释。

### 14.2 UTF-8

Go string 可持有任意 bytes，但 JSON 编码会处理非法 UTF-8。必须在 Source/Parser 边界定义策略：

- 严格 UTF-8：非法输入停 Pipeline；
- replacement：使用 `strings.ToValidUTF8`，标记 `encoding_replaced=true`；
- 配置指定编码：通过成熟 decoder 转换，并限制错误/扩容。

无论内容如何转换，checkpoint offset 始终按原始文件 bytes 计算。

不要记录整行非法原始字节到 Agent 自身日志；只记录 pipeline、offset、byte count 和安全摘要。

## 15. FileSource 状态机

```text
opening
  | checkpoint/current identity match
  v
following -- EOF + path new identity --> draining_rotated
  | same identity, size < offset                |
  +--------------> reset_after_truncate         | stable EOF
                         |                       v
                         +-------------> waiting_replacement
                                                   |
                                                   | open new path
                                                   v
                                               following

任何状态 -- fatal identity/read/seek error --> failed
任何状态 -- context canceled -------------> stopped
```

建议把每次轮询拆成可测试动作：

```go
type ActionKind int

const (
	ActionRead ActionKind = iota
	ActionWait
	ActionBeginDrain
	ActionSwitch
	ActionResetTruncated
	ActionStop
)
```

状态决策函数可尽量保持纯逻辑，真正的 open/read/stat/seek 在小 adapter 中执行。不要为了“纯函数”抽象整套文件系统；只隔离平台身份、clock 和必要 I/O 即可。

## 16. 与 Batch/Spool 的连接

### 16.1 单个记录

```text
FileSource 读取 bytes [oldOffset, afterOffset)
  -> RawRecord{After: identity + afterOffset}
  -> Parser/Enricher
  -> PendingEntry{Entry, After}
```

### 16.2 单个 batch

一个 batch 中所有 entry 必须：

- 来自同一个 Pipeline；
- 来自同一个 file identity；
- `After.Offset` 严格递增；
- batch checkpoint 等于最后一条 `After`。

形成新 identity 记录前，flush 旧 batch。旧文件稳定 EOF 后，用受限控制事务保存新 identity offset 0 的 rotate anchor；随后新文件每个含记录的 batch 仍使用普通 `Commit(batch + checkpoint)` 推进位置。

### 16.3 commit 失败

若容量满，Pipeline 阻塞在 commit，不继续调用 `NextRecord`。此时最多有一个有界内存 batch和 FileSource 可能已经读入的有界 `bufio`/pending 数据。

若进程退出，durable checkpoint 未推进；重启会重读这部分。不要尝试把 FileSource 的 volatile `readOffset` 单独保存来“减少重复”，那会重新制造 batch/checkpoint 分离窗口。

## 17. 分小步实现顺序

### 步骤 1：修正当前资源与 timer 问题

在不加轮转前先做到：

- `NewFileSource` 在 Seek/identity 失败时关闭 file；
- 使用一个可复用 timer 或 clock abstraction；
- `Close` 唤醒/配合取消；
- 错误包含 operation 与 path。

### 步骤 2：引入 byte position

维护显式 `readOffset`，让 RawRecord 带 `After`。覆盖 ASCII、中文、CRLF、空行和 bufio 跨 buffer 行。

### 步骤 3：实现 max line 与半行

改用有界 fragment 读取，定义超限和 shutdown/rotation partial policy。此时仍只跟随一个 identity。

### 步骤 4：实现 Linux 与 Windows identity adapter

先支持项目实际 release 目标。每个平台都要：

- 同一 handle 多次 identity 相同；
- rename 后旧 handle identity 不变；
- recreate 同 path 后新 identity 不同；
- 打开/关闭无句柄泄漏。

### 步骤 5：checkpoint 启动恢复

接入第五章的 Store：无 checkpoint 执行 initial policy；有 checkpoint 严格验证 identity/size/fingerprint。

### 步骤 6：运行中 rename + recreate

先实现单次轮转：旧 handle stable EOF、flush 旧 batch、打开并验证新 identity、持久化 rotate anchor，再切换到新 handle。使用真实文件集成测试，并覆盖“anchor 提交后、第一条新记录前崩溃”的恢复场景。

### 步骤 7：停机期间轮转候选扫描

加入受限 glob 与 identity 匹配。找不到旧 identity 默认失败，不静默接受 gap。

### 步骤 8：copytruncate 检测

实现 size 回退路径与 metric，写清弱保证。不要先上复杂内容指纹，除非测试证明值得。

### 步骤 9：多次快速轮转

只有单轮转稳定后再增加 successor queue。每增加一种状态，都先扩展故障矩阵和 handle 上限。

## 18. 错误、取消与资源关闭

### 18.1 错误分类建议

| 情况 | 分类 | 行为 |
| --- | --- | --- |
| 配置 path 尚不存在 | temporary | 等待创建并告警 |
| replacement 短暂缺失 | temporary | 保留旧状态，等待 |
| Windows sharing violation | 通常 temporary | 有界退避重试 |
| permission denied | fatal/paused | 停 Pipeline，等待配置/权限修复 |
| identity 不支持 | fatal | 拒绝该 Source 启动 |
| checkpoint identity 找不到 | state conflict | 默认停 Pipeline，要求 reset/accept-gap |
| line too long | policy error | 默认停 Pipeline，不推进 checkpoint |
| 普通 read I/O error | 依据 `errors.Is` | 不要全部当 EOF |

错误分类必须基于可识别 cause，而不是匹配错误字符串。

### 18.2 关闭

FileSource 应只有一个 goroutine 调用 `NextRecord`。若要支持并发 Close，必须明确同步；更简单的合同是顶层先取消 context、等待 `NextRecord` 返回，再调用 `Close`。

关闭时：

1. 停止/reset timer；
2. 不再 stat/open 新 path；
3. 按 partial policy 处理 pending；
4. 让 Pipeline flush 完整 batch 到 spool；
5. 关闭当前及任何 retiring handle；
6. 聚合 Close 错误，但不覆盖更重要的 spool commit 错误。

## 19. 哪些代码值得完整给出

实现时应完整展示并逐行解释：

- Linux 与 Windows identity adapter；
- 从 checkpoint 打开/验证/seek 的函数；
- 有界逐行读取算法；
- EOF 时 stat、rotate、truncate 的状态决策；
- stable EOF 与 old/new handle 切换；
- pending 半行在 rotate/shutdown 中的处理；
- batch 不跨 identity 的 flush 边界。

配置字段、简单 equality 和纯映射代码不需要写成冗长样板。

## 20. 测试设计

### 20.1 纯行为/单元测试

| 输入 | 断言 |
| --- | --- |
| `a\n` | content=`a`，offset 增加 2 |
| `你好\r\n` | 去换行后内容正确，offset 按原始 UTF-8 bytes 增加 |
| `\n` | 产生完整空行或按显式 filter 处理 |
| 半行 + 后续追加 | 第一次不返回，追加换行后只产生一条完整记录 |
| 超过 max line | 命中明确策略，内存不继续增长 |
| context cancel at EOF | timer 停止并及时返回 |
| checkpoint offset | seek 后第一条正好是未提交记录 |

### 20.2 真实轮转集成测试

使用 `t.TempDir()`，不要 mock identity：

1. 创建 `app.log`，启动 beginning；
2. 追加带连续序号的完整行；
3. 等待 batch commit；
4. rename 为 `app.log.1`；
5. 在旧 handle 再追加一条（平台允许时）；
6. create 新 `app.log` 并追加；
7. 断言旧文件尾部先被 spool，新文件随后被 spool；
8. 重启 Source，确认不重读已 checkpoint 内容。

另建控制过渡崩溃测试：新 path 已验证但还没有完整行时提交 rotate anchor，随后立即退出并删除旧轮转文件；重启后必须从新 identity 的 offset 0 继续。再让 transition 在 compare-and-set 前失败，确认旧 checkpoint 保持不变。

Windows 和 Linux 必须分别在真实 runner 执行。只在 Linux mock 一个“Windows identity”不能证明 Windows rename/share 行为。

### 20.3 停机轮转测试

1. commit 到 offset N 后关闭 Agent；
2. rename 当前文件并创建新 path；
3. 给旧文件追加未处理内容；
4. 重启；
5. 候选扫描按 identity 找到旧文件；
6. 读完旧文件再切换新文件；
7. 连续序号集合无缺失。

再测旧文件已删除：默认应给出明确 state conflict，而不是静默继续。

### 20.4 Copytruncate 测试

测试可以证明“检测到 size 回退后提交 validated transition 并从 0 继续”，不能证明竞态窗口绝不丢失。分别在 transition 前和后模拟退出，确认旧 checkpoint 触发重新检测、新 checkpoint 从 0 恢复。用序号发生器记录某次实验的缺失结果，并在文档注明限制。

### 20.5 避免脆弱等待

优先暴露测试用 `ScanOnce`/fake clock 驱动状态。真实文件集成测试可用有 deadline 的 eventually 循环，但失败时打印当前 identity、offset、size、state，不使用无上限 sleep。

## 21. 验收命令与证据

```powershell
gofmt -w .\internal\agent\source .\internal\agent\checkpoint
go test ./internal/agent/source -count=1
go test -race ./internal/agent/source -count=1
go test ./internal/agent/... -count=1
go vet ./internal/agent/...
```

交叉编译只能证明平台文件能编译：

```powershell
$env:GOOS = 'linux'
$env:GOARCH = 'amd64'
go test -c ./internal/agent/source
Remove-Item Env:GOOS
Remove-Item Env:GOARCH
```

真实语义仍需 Windows runner 和 Linux runner 各自执行测试。验收证据应包括：

- 当前平台真实 identity 测试；
- rename + recreate 运行中测试；
- 停机期间轮转恢复测试；
- truncate 检测测试与弱保证说明；
- 中文/CRLF/半行/max-line 测试；
- 候选找不到时的明确失败；
- 反复轮转后所有 handle 关闭、临时目录可删除；
- 带 `run_id + sequence` 的端到端集合比较。

## 22. 常见错误

1. **把 path 当 identity。** 同一路径轮转后可能是新文件。
2. **只保存 offset。** 新文件复用旧 offset 会跳过头部。
3. **用文件名排序猜 identity。** 命名规则可配置，时间戳和压缩会改变。
4. **用 `file.Seek(Current)` 当 bufio 消费位置。** reader 可能预读更多 bytes。
5. **用字符串长度计算 offset。** Unicode、CRLF 和编码转换会错位。
6. **一次 EOF 就切新文件。** 旧文件可能稍后继续写入。
7. **同一 batch 混合两个 identity。** checkpoint 状态转换和恢复难以证明。
8. **truncate 后只把 offset 设 0，不 Reset reader。** buffer 里可能仍有旧文件数据。
9. **半行 EOF 时自动提交为完整行。** 应由显式策略决定。
10. **用 `ReadString` 接受无限大行。** 单条异常日志即可耗尽内存。
11. **copytruncate 测试通过就宣称零丢失。** 工具本身存在无法消除的竞态。
12. **只在一个 OS 测平台 adapter。** 编译成功不证明 rename 和 sharing 行为。
13. **找不到旧 identity 时静默从新文件开始。** 这隐藏了可能的数据 gap。
14. **轮询循环每次 `time.After`。** 长期运行产生不必要 timer 分配，也不利于确定性测试。

## 23. 复盘题

1. 为什么 path 相同不代表文件相同？
2. 为什么 `After.Offset` 必须按原始 bytes 而不是处理后的 string 计算？
3. bufio 预读会怎样让 `Seek(Current)` 误导 checkpoint？
4. 运行中 rename + recreate 时，为什么要保留旧 handle 到稳定 EOF？
5. Agent 停机期间轮转后，怎样找回旧 identity？找不到时默认为什么应停止？
6. 为什么第一版 batch 不跨 identity？
7. copytruncate 的不可消除窗口发生在哪里？
8. 如果 truncate 后文件快速长回旧 offset 以上，只比较 size 会有什么漏检？
9. 半行在普通 EOF、轮转和 shutdown 三种情况下应如何处理？
10. `start_position: end` 的初始 anchor 与消费数据后的 checkpoint 有何区别？
11. 如何证明 Windows adapter 的行为，而不只证明它能交叉编译？
12. spool 满时，FileSource 最多还能领先 durable checkpoint 多少数据？由哪些上限约束？

## 24. 进入下一章前的完成条件

- [ ] RawRecord 携带 identity + byte offset 的 `After`；
- [ ] offset 对 UTF-8、CRLF 和 bufio 跨 buffer 都正确；
- [ ] Windows 与 Linux 使用各自平台 adapter；
- [ ] 有 checkpoint 时严格校验 identity、size 和 source fingerprint；
- [ ] 无 checkpoint 时 beginning/end 策略明确，end anchor 可恢复；
- [ ] 单次 rename + recreate 能 drain 旧 handle、持久化 rotate anchor 后切换；
- [ ] 停机轮转能通过受限候选扫描找旧 identity；
- [ ] 找不到旧 identity 时不会静默跳过；
- [ ] truncate 控制过渡可恢复，并会 Seek + Reset reader、记录事件；
- [ ] 文档和用户输出不承诺 copytruncate 绝对零丢失；
- [ ] 半行、超长行和编码都有显式有界策略；
- [ ] 每个 batch 只包含一个 Pipeline、一个文件 identity；
- [ ] 所有真实 handle、timer 和等待在取消后可关闭。

下一章将从 spool 的另一侧消费 batch：[07. Dispatch、重试与背压](./07-dispatch-retry-backpressure.md)。
