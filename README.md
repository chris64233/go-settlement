# go-settlement

开发环境：Go 1.23.0。

运行测试：

    go test ./...

## 结算指令排队服务

`Queue`（见 `queue.go`）是一个受可用资金约束的结算指令排队服务，所有方法并发安全。

### 指令与登记

- 每条指令记录付款账户、金额、币种、价值日、优先级（数值越大越优先）和最晚处理时间。
- 受理后按 **优先级降序 → 受理时间升序 → 指令号升序** 形成稳定顺序。
- 以外部指令号为幂等键：相同内容重复提交返回原结果（`replayed=true`）；关键内容（账户、金额、币种、价值日、优先级、截止时间）变化时返回 `ErrInstructionConflict`。
- 所有金额使用 `Decimal`（基于 `big.Rat`）精确表示，不使用浮点数。

### 释放扫描规则

- 每次 `Scan` 固定扫描时刻的账户可用余额与队列版本，余额能够覆盖指令时才允许释放。
- 遇到无法覆盖的指令时按 `ScanPolicy` 处理：
  - `StopOnInsufficient`：立即停止，其后指令一律记为 `BLOCKED_BY_AHEAD`；
  - `AllowPrioritySkip`：仅放行优先级**严格高于**阻塞指令的候选，绝不任意挑选小额指令凑数；
  - 两者都未开启时，缺口之后的指令全部保持等待。
- 价值日未到的指令不参与释放，记为 `VALUE_DATE_NOT_REACHED`。

### 并发与一致性

- 登记、入账、冻结、扫描都在同一临界区内完成：同一笔余额不会被重复使用，每条指令最多从 `WAITING` 进入 `RELEASED` 一次。
- 任何影响队列或余额的变更都会递增队列版本；扫描在版本快照上规划、原子提交，扫描失败或并发冲突不会改变队列顺序和未释放指令。

### 查询接口

- `Pending(account, asOf)`：返回等待队列及每条指令未处理的原因（`INSUFFICIENT_FUNDS` / `BLOCKED_BY_AHEAD` / `VALUE_DATE_NOT_REACHED` / `RELEASABLE`）。
- `ReleaseBasis(externalID)`：返回释放依据，包括扫描号、队列版本、释放前后可用余额快照和当时策略。
- `BalanceOf(account, currency)`：查询可用与冻结余额；`Freeze`/`Unfreeze` 支持人工冻结。
