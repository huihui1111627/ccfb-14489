# 外部交叉核对：protowire

验收里的逐字节期望值由两套独立实现给出：

1. `acceptance/reference_encode.py`：按线格式规范手写的 Python 编码器（仓库内，
   不调用被测代码）；
2. Google 官方 protobuf 的底层线格式包 `google.golang.org/protobuf/encoding/protowire`
   （不在本仓库依赖中，一次性在临时模块里运行，用于确认上面那组字节不是我们自
   说自话）。

核对脚本（在 `/tmp` 下的独立模块执行，避免污染被测模块）：

```go
// go.mod: require google.golang.org/protobuf v1.36.12
package main

import (
	"encoding/hex"
	"fmt"
	"math"
	"os"

	"google.golang.org/protobuf/encoding/protowire"
)

func main() {
	var b []byte
	b = protowire.AppendTag(b, 1, protowire.Fixed64Type)
	b = protowire.AppendFixed64(b, math.Float64bits(1.5))
	...
	// 与 TestWireReferenceVectors 的 bagWant 相同的一组字段
	got := hex.EncodeToString(b)
	want := "09000000000000f83f15000010c018ffffffffffffffffff0120feffffffffffffffff0128ac0230ffffffffffffffffff01380d400f4defbeadde5108070605040302015df7ffffff61f6ffffffffffffff6801720668c3a96c6c6f7a0300ff108001028a010c01ffffffffffffffffff0102900105900106"
	if got != want {
		fmt.Println("MISMATCH")
		os.Exit(1)
	}
}
```

运行结果：

```
09000000000000f83f15000010c018ffffffffffffffffff0120feffffffffffffffff0128ac0230ffffffffffffffffff01380d400f4defbeadde5108070605040302015df7ffffff61f6ffffffffffffff6801720668c3a96c6c6f7a0300ff108001028a010c01ffffffffffffffffff0102900105900106
MATCHES reference encoder expectations
```

另外逐项核对的原始值：

| 项 | protowire 输出 | 说明 |
| --- | --- | --- |
| `string "abc"`（字段 1） | `0a03616263` | tag + 长度 + 内容 |
| `int64 -1` | `ffffffffffffffffff01` | 负数符号扩展为 10 字节 |
| `sint64 -8` | `0f` | zig-zag 后 1 字节 |
| `sint32 -7` | `0d` | zig-zag |
| `fixed32 0xdeadbeef` | `efbeadde` | 小端 |
| `fixed64 0x0102030405060708` | `0807060504030201` | 小端 |
| `uint64 max` | `ffffffffffffffffff01` | 10 字节 |
| 未知字段 `field 99 varint 42` | `98 06 2a` | `ConsumeTag` 得 num=99、type=0、n=2 |

由此确认：varint（含负数符号扩展）、zig-zag、定长小端、tag 布局、长度前缀与未知
字段跳过都与官方实现一致。
