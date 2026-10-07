# go-settlement

参与方之间的结算限额控制。系统在接受资金指令时计算双方当日累计风险，
保证并发提交不会绕过单边或双边限额。

## 功能

- **限额规则版本化**：规则按参与方、对手方、币种和生效时间维护
  （`RuleStore.Publish` / `Resolve`）。每次发布生成新的不可变版本，
  已受理的指令继续保留当时采用的版本（`Instruction.PayerVersion` /
  `PayeeVersion`）。
- **三层限额**：单笔上限（`MaxSingleAmount`）、双边净敞口上限
  （`MaxNetExposure`）和单方总敞口上限（`MaxGrossExposure`），
  可同时设置，值为 0 表示该层不约束。
- **原子受理**：`Service.Accept` 固定付款方、收款方、金额、价值日和
  限额版本，并在同一把互斥锁构成的持久化边界内完成校验与双方占用登记；
  任一层限额不足时整笔拒绝，双方占用均不增加。
- **生命周期**：`Complete` 将预留占用转为已用额度，`Cancel` / `Fail`
  按原占用记录释放。释放只依据指令自身的记录，迟到取消不会影响其他
  指令后来取得的额度；重复取消/完成幂等。
- **幂等受理**：相同 `RequestID` 的重复请求返回首次受理的指令，
  不产生新的占用。
- **占用查询**：`Usage` 按参与方、对手方、币种和价值日聚合占用，
  `UsageDetails` 返回逐笔明细。

## 示例

```go
rules := settlement.NewRuleStore()
day := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)

rules.Publish(
	settlement.RuleKey{Participant: "A", Counterparty: "B", Currency: "CNY"},
	settlement.LimitRule{MaxSingleAmount: 1_000, MaxNetExposure: 5_000, MaxGrossExposure: 8_000},
	day,
)
rules.Publish(
	settlement.RuleKey{Participant: "B", Counterparty: "A", Currency: "CNY"},
	settlement.LimitRule{MaxNetExposure: 5_000},
	day,
)

svc := settlement.NewService(rules)
ins, err := svc.Accept("req-1", "A", "B", "CNY", 900, day)
if err != nil {
	// 任一层限额不足，整笔拒绝
}
svc.Complete(ins.ID) // 或 svc.Cancel(ins.ID) / svc.Fail(ins.ID)

sum := svc.Usage("A", "B", "CNY", day)
fmt.Println(sum.ReservedOutgoing, sum.UsedOutgoing, sum.NetExposure())
```

## 测试

覆盖限额版本切换、多层限额冲突、并发占用争用、重复请求幂等、
生命周期转换与占用查询：

    go test -race ./...

开发环境：Go 1.23.0。

运行测试：

    go test ./...
