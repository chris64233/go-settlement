# go-settlement

大额结算批次的银行指令拆分服务（Go 模块，无外部依赖）。

## 功能

- **批次与规则**：批次包含付款账户、收款方、币种、总金额（最小货币单位整数）与期望价值日；银行规则包含单笔最小/最大额、当日剩余额度、每批最多指令数。生成方案时冻结批次总额与规则版本快照。
- **确定性拆分**：指令数取满足单笔上限的最小值 `n = ceil(total/max)`，再尽量均分（前 `total%n` 条多一个最小单位），保证每条落在 `[min, max]`、各条之和精确等于批次总额、同一输入永远得到同一输出。受最小额、最大额、每日额度或条数限制无法成案时整次失败，不保存半套子指令。
- **状态与汇总**：确认方案后子指令可分别接收银行回执；批次汇总精确区分未发送（PENDING）、处理中（PROCESSING）、成功（SUCCESS）、失败（FAILED）金额，并校验总额守恒。
- **重试**：失败子指令沿用原业务身份（同一指令 ID）重试，不重新拆分已成功部分。
- **幂等**：批次号（`BatchNo`）与方案号（`PlanNo`）分别幂等；相同键重复提交返回原结果，规则或总金额变化返回 `ErrConflict`。
- **查询**：`GetPlan` 返回拆分依据（`Plan.Rationale`）与子指令状态；`Summary` 做批次金额核对；`Trace` 从批次追到每条银行指令及其回执。

## API 概览

```go
s := settlement.NewService()
b, _ := s.CreateBatch(settlement.Batch{BatchNo: "B1", PayerAccount: "A", Payee: "P", Currency: "CNY", TotalAmount: 2500, ValueDate: "2026-10-09"})
plan, instrs, err := s.GeneratePlan(b.ID, "P1", settlement.BankRule{
    Version: "v1", MinPerInstruction: 100, MaxPerInstruction: 1000,
    DailyRemaining: 100000, MaxInstructions: 10,
})
s.ConfirmPlan(plan.ID)
s.SendInstruction(instrs[0].ID)
s.RecordReceipt(instrs[0].ID, true, "OK", "")
sum, _ := s.Summary(b.ID)          // 金额核对
_, _, _, receipts, _ := s.Trace(b.ID) // 批次 -> 指令 -> 回执
```

## 测试

覆盖舍入边界、规则变化冲突、部分回执汇总、重复确认幂等、失败重试身份保持等场景：

    go test ./...
