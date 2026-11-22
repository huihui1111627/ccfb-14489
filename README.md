# IDL Bridge — HTTP/JSON ⇄ 二进制 RPC 通用转码网关

一个用 Go 写的 API 网关转码模块：解析接口描述文件（protobuf 方言），把普通
HTTP/JSON 请求按 `google.api.http` 路由规则映射成二进制消息交给后端，再把后端
响应（含服务端流式响应与错误码）转回 HTTP。**描述解析、二进制编解码、JSON 映射、
路由匹配全部由本仓库实现，没有使用任何现成的序列化库或转码库**（只用了标准库的
`encoding/json` 处理 JSON 语法、`encoding/binary` 处理字节序，protobuf 的
varint、zig-zag、tag、打包等语义均自行实现）。

后端服务用 `internal/backend` 里的仿真实现代替：它按方法描述自动生成响应，并支持
通过 `X-Fail-Status` 请求头注入错误码，用于演示状态码映射。

```
             ┌──────────────── gateway ────────────────┐
HTTP/JSON ──▶│ 路由匹配 → 请求绑定 → 二进制编码 → 后端 │──▶ 仿真服务
             │   ↑ 逐字节解释 / 检查器页面            │
HTTP/JSON ◀──│ 状态码映射 ← JSON 渲染 ← 二进制解码     │◀──
             └────────────────────────────────────────┘
```

## 快速开始

```bash
go test ./...                       # 单元测试 + 验收测试
go run ./cmd/gateway -addr :8080    # 打开 http://localhost:8080/ 使用检查器页面
```

页面顶部选择一个方法，构造路径/查询/请求体，点击「匹配并转码」，右侧会依次显示：

* 命中的路由、路径变量、按优先级排序的候选规则；
* 绑定出的请求消息（JSON 视图）；
* 每个字段来自 path / query / body 的哪一个参数；
* 二进制编码的逐字节解释（偏移、字节、tag、长度、值）、十六进制与 Base64。
* 「跑一次吞吐基准」按钮会对匹配+绑定+编码做一轮基准并显示 µs/次与次/秒。

## 目录结构

| 路径 | 内容 |
| --- | --- |
| `internal/idl` | 描述模型、`.proto` 词法/语法解析、类型链接、JSON 命名规则、内建 well-known 类型 |
| `internal/dyn` | 无类型动态消息（字段号 → 规范值），深拷贝、相等性、字段路径赋值 |
| `internal/wire` | 二进制编解码：tag、uvarint、zig-zag、定长、长度前缀、打包、map、未知字段跳过 |
| `internal/jsoncodec` | 严格 JSON 映射：双命名、64 位字符串、枚举名、时间戳/时长等特殊类型、默认值省略 |
| `internal/routing` | 路径模板解析与匹配、多段变量与 `**`、动作后缀、优先级排序、查询参数 key 语法 |
| `internal/transcode` | HTTP 规则编译、路径/请求体/查询绑定、错误码映射、流式输出 |
| `internal/explain` | 二进制逐字节解释 |
| `internal/backend` | 仿真后端（按描述生成响应 + 错误注入） |
| `internal/gateway` | HTTP 处理器、`/_inspect` `/_schema` `/_bench` 端点 |
| `web/index.html` | 检查器页面（纯静态，无外部依赖） |
| `testdata/*.proto` | 验收用接口描述，覆盖全部特性 |
| `acceptance/` | 手工期望值驱动的验收测试、随机往返测试、基准与独立参考编码器 |

## 1. 接口描述解析

支持 `proto3`（也接受 `proto2` 语法骨架）：

* `package`、`import`（本地文件与 `google/protobuf/*` 内建类型）、`option`；
* `message`、嵌套 `message`/`enum`、`oneof`、`map<K,V>`、`repeated`、
  `optional`、`[packed]`、`[json_name]`、`[deprecated]`、`allow_alias`；
* 全部标量类型：`double float int32 int64 uint32 uint64 sint32 sint64
  fixed32 fixed64 sfixed32 sfixed64 bool string bytes`；
* `service`/`rpc`，含 `stream` 两侧、`google.api.http` 及其
  `body`/`response_body`/`additional_bindings`；
* 类型解析遵循 protobuf 的作用域规则（由内向外、包前缀、前导点绝对名）。

内建 well-known 类型（`Timestamp`、`Duration`、`FieldMask`、`Any`、`Empty`、
`Struct`、`Value`、`ListValue`、9 种包装类型）随注册表提供。

## 2. 路由匹配

路径模板按 `/` 分段，段可以是字面量、`{变量}`、`{变量=模式}`，模式里可含
`*`、`**` 与字面量；变量可展开成多段；最后一段可有 `:动作` 后缀。

```
/v1/{name=shelves/*/books/*}           → /v1/shelves/sci/books/dune
                                          name = "sci/dune"
/v1/{name=shelves/*/books/*}/chapters/{c} → 多段变量在中间
/v1/files/{path=**}:download           → /v1/files/a/b/c.pdf:download
```

* 路径先按原始 `/` 切分再逐段百分号解码；`%2F` 默认留在段内，若整条路径按此
  规则匹配不上，会再把 `%2F` 当分隔符重试（客户端把整个多段变量编码进一段时
  也能命中）。空段（`//`）不合法。
* 带动作后缀的路由只匹配带相同后缀的请求；无后缀路由不匹配带 `:` 的请求。
* **优先级**（`routing.Less`，全序，结果稳定）：
  1. 首个通配符之前的字面量段数（越多越具体）；
  2. 含 `**` 的模板排在后面；
  3. 通配段数越少越优先；
  4. 路径变量越多越具体（`{name=shelves/*}` 胜过 `{id=*}`）；
  5. 带动作后缀者优先；
  6. 段数多者优先；
  7. 声明顺序（含 `additional_bindings` 的展开顺序）；
  8. 模板文本。
  页面的「优先级排序」列表就是这条规则的实际顺序。

## 3. 请求映射

`transcode.BindRequest` 的规则（越靠前优先级越高，与 HTTP 转码规范一致）：

1. **路径变量**写入对应字段；变量名是点分字段路径，可指向嵌套字段
   （`{bag.text}`），多段变量必须绑定 `string`/`bytes` 字段，值为各段用 `/` 连接。
2. **请求体**：`body: "*"` 解析为整个请求消息；`body: "book"` 解析后写入该消息
   字段；规则未声明 body 时携带非空体会被拒绝。
3. **查询参数**覆盖其余未绑定的叶子字段。支持：
   * `a.b.c` 与 `a[b][c]` 两种嵌套写法，可混用（`a[b].c[d]`）；
   * 重复字段：重复同名键、`items[]=1&items[]=2`、`items=1&items=2`；
   * 重复消息：`items.name=x`（追加元素）、`items[2].name=x`（按下标）、
     `items[].name=x`；
   * map：`labels[env]=prod`，消息值可继续下钻 `shelves[top].topic=sci`；
   * 名称接受 `page_size` 与 `pageSize` 两种拼写；
   * 已由路径或请求体赋值的字段会被忽略；单值字段重复给出、类型不合法、
     未知字段（默认）都会返回 `INVALID_ARGUMENT`。

绑定过程会记录每一处赋值的来源，检查器页面「字段来源」即来自这里。

## 4. JSON ⇄ 消息

严格按 proto3 JSON 映射实现（`internal/jsoncodec`）：

| 项 | 规则 |
| --- | --- |
| 字段名 | 输出 lowerCamelCase（或 `UseProtoNames` 时 snake_case）；输入两种都接受 |
| 64 位整数 | 输出字符串（`"9223372036854775807"`）；输入接受字符串或数字 |
| 32 位整数 | JSON 数字 |
| 枚举 | 输出名字，未知数值输出数字；输入接受名字或数字 |
| bytes | 标准 Base64（输入同时接受 URL-safe 与无填充形式） |
| float | 特殊值用 `"NaN"`/`"Infinity"`/`"-Infinity"`，float32 用最短往返表示 |
| Timestamp | RFC 3339，小数位 0/3/6/9，年份限制 0001–9999 |
| Duration | `"1.500s"`，小数位 0/3/6/9，符号一致性与范围校验 |
| FieldMask | 逗号分隔的 camelCase 路径 |
| 包装类型 | 直接输出内层值 |
| Struct/Value/ListValue | 任意 JSON 结构 |
| 缺省值 | 默认省略（`EmitDefaults` 可保留）；`optional`/`oneof`/消息字段有 presence，零值也输出 |
| 未知字段 | `UnknownReject`（默认）/`UnknownIgnore`/`UnknownPreserve`（保留并回写） |
| 重复/冲突 | 同一对象的重复键、两种拼写指向同一字段、同一 oneof 的多个成员都拒绝 |

## 5. 二进制编码

`internal/wire`：

* tag = `field_number << 3 | wire_type`，wire type 0/1/2/5；
* `int32/int64` 用 64 位 varint，负数按符号扩展为 10 字节；`sint32/sint64`
  用 zig-zag；`fixed*`/`sfixed*` 小端定长；`string/bytes/message` 长度前缀；
* 打包：proto3 下可打包的重复标量/枚举默认打包，`[packed=false]` 或
  `IsPacked()==false` 时逐个编码；解码端两种形式都接受；
* map 编码为重复的 entry 消息（key=1，value=2），缺省的 key/value 按各自默认值
  处理，编码时按 key 排序保证确定性；
* 嵌套深度上限 100；非法 UTF-8、截断、长度越界、varint 溢出、字段号 0 都会报错；
* 解码遇到未知字段时按 wire type 跳过，并保留原始字节，重新编码时原样附回
  （`DiscardUnknown` 可丢弃）。

## 6. 错误码与流式

后端状态码（gRPC 风格）到 HTTP 的映射：

| 状态 | HTTP | 状态 | HTTP |
| --- | --- | --- | --- |
| OK | 200 | OUT_OF_RANGE | 400 |
| CANCELLED | 499 | UNIMPLEMENTED | 501 |
| UNKNOWN | 500 | INTERNAL | 500 |
| INVALID_ARGUMENT | 400 | UNAVAILABLE | 503 |
| DEADLINE_EXCEEDED | 504 | DATA_LOSS | 500 |
| NOT_FOUND | 404 | UNAUTHENTICATED | 401 |
| ALREADY_EXISTS | 409 | RESOURCE_EXHAUSTED | 429 |
| PERMISSION_DENIED | 403 | ABORTED | 409 |
| FAILED_PRECONDITION | 400 | | |

错误体为 `{"code": <数字>, "message": ..., "status": "<名字>"}`，并附
`X-Status-Code` 响应头。路由未命中返回 404 + `UNIMPLEMENTED`；绑定失败返回 400 +
`INVALID_ARGUMENT` 并指出具体参数。

`rpc X(Req) returns (stream Resp)` 会按块输出：默认 NDJSON（每行一个响应消息，
`Content-Type: application/x-ndjson`，逐条 `Flush`），加
`stream_format=array` 得到 JSON 数组；首个消息之前的错误仍以正常 HTTP 错误码
返回，流开始后的错误作为最后一行错误文档输出。`Grpc-Timeout`/`X-Request-Timeout`
控制单请求超时（默认 30s，上限 10min）。

## 7. 验收与性能

`acceptance/acceptance_test.go` 逐条比对**手工准备**的期望：

* `expectations.json`：路由命中/不命中与路径变量、路径/查询/请求体绑定的结果与
  错误、以及一个完整请求的期望十六进制；
* `TestWireReferenceVectors`：全标量消息、Book、Timestamp、Duration 的逐字节
  期望，与独立实现 `acceptance/reference_encode.py`（Python 按线格式规范手写，
  不调用被测代码）交叉核对；该组字节另外与官方
  `google.golang.org/protobuf/encoding/protowire` 逐字节比对过，见
  `acceptance/protowire_crosscheck.md`；
* `TestJSONReferenceDocuments`：精确 JSON 文档（64 位字符串、枚举名、时间戳、
  时长、包装类型、map）；
* `TestBinaryToJSONCrossConversion`、`TestRandomJSONBinaryRoundTrip`、
  `wire` 包内的 `TestRandomRoundTrip`：随机/交叉往返；
* `internal/gateway/handler_test.go`：真实 HTTP 往返、状态码映射表、流式分块、
  `Accept: application/protobuf` 二进制响应、检查器端点。

基准（Apple M4，`go test ./acceptance -bench . -benchtime 500ms`，数值为量级参考）：

| 操作 | 耗时 | 分配 |
| --- | --- | --- |
| 路由匹配 | ≈1.7 µs/op | 15 allocs |
| 查询+路径绑定 | ≈1.7 µs/op | 24 allocs |
| JSON 请求体绑定 | ≈3.1 µs/op | 57 allocs |
| 二进制编码（≈200B 消息） | ≈1.4 µs/op | — |
| 二进制解码 | ≈3.0 µs/op | — |
| JSON 编码 / 解码 | ≈10 µs / ≈11 µs | — |
| 完整转码 + 仿真后端调用 | ≈6.8 µs/op | 106 allocs |

`/_bench?n=2000` 提供同样的在线基准。

手工期望值除了由 `go test ./acceptance` 校验，也可以直接打印成对照表：

```bash
go run ./cmd/acceptancerun -schema ./testdata -expect ./acceptance/expectations.json
```

每条用例会显示期望、实际匹配到的 RPC 与路径变量、绑定出的 JSON、二进制十六进制，
以及被拒绝时的具体错误信息。

## 已知取舍

* 不支持 protobuf `group` 语法（wire type 3/4 的解码跳过仍可用）；
* `Any` 的 JSON 形式只实现 `{"@type": ..., "value": <base64>}`，不解析被包装类型；
* 客户端流式方法可以解析描述，但网关只实现服务端流式（`stream` 请求侧返回
  `UNIMPLEMENTED`）；
* 查询参数里的重复消息按「追加/按下标」处理，不支持多层嵌套的索引语法；
* 中文/Unicode 标识符被接受，便于用中文书写描述，但不符合 protobuf 官方的
  ASCII 限制。
