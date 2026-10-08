# go-settlement

开发环境：Go 1.23.0。

运行测试：

    go test ./...

## 大额结算批次拆分

把一个大额结算批次按银行规则拆分为多条银行指令，保证单笔限额、
每日额度与笔数上限，且拆分结果确定、总金额不丢失。

### 概念

- `Batch`：批次，含付款账户、收款方、币种、总金额、期望价值日。
- `BankRule`：银行规则版本，含单笔最小额、单笔最大额、当日剩余额度、
  每批最多指令数。生成方案时批次与规则快照被冻结进 `Plan`。
- `Plan`：拆分方案，含拆分依据（`Rationale`）与全部子指令。
- `Instruction`：子指令，`InstructionID` 为业务身份，重试不变化。
- `Receipt`：银行回执，按尝试次数（`Attempt`）留痕。

金额一律使用最小货币单位（分）的整数，避免浮点舍入误差。

### 拆分算法

确定性算法（相同输入必得相同输出），见 `split.go`：

1. `n = ceil(total / max)`，取满足单笔上限的最少笔数；
2. `base = total / n`，余数逐笔加一分配给前 `rem` 笔；
3. 所有子指令金额之和恒等于批次总额。

若总额超过当日剩余额度、所需笔数超过每批上限、或均分后低于单笔
最小额，则整次生成失败（返回 `*SplitError`），不保存任何子指令。

### 流程

```go
svc := settlement.NewService()
plan, err := svc.GeneratePlan("PLAN-1", batch, rule) // 冻结快照，幂等
plan, err = svc.ConfirmPlan("PLAN-1")                // 确认，幂等
svc.MarkSent(insID)                                  // 发送银行
svc.RecordReceipt(insID, "SUCCESS", "BANK-REF")      // 接收回执
svc.RetryFailed("PLAN-1")                            // 重试失败指令，沿用业务身份
sum, _ := svc.Summary("BATCH-1")                     // 批次金额核对
trace, _ := svc.Trace("BATCH-1")                     // 批次 -> 指令 -> 回执
```

### 幂等与冲突

- 批次号与方案号分别作为幂等键；参数一致时重复调用返回原方案。
- 同一幂等键下总金额或规则版本发生变化时返回 `ErrConflict`。

### 状态模型

子指令状态：`PENDING`（未发送）→ `PROCESSING`（处理中）→
`SUCCESS` / `FAILED`。`FAILED` 可经 `RetryFailed` 回到 `PROCESSING`，
尝试次数递增，已成功部分不重新拆分。`BatchSummary` 按状态汇总金额，
`Reconciled()` 校验各状态金额之和等于批次总额。
