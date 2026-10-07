# go-settlement

开发环境：Go 1.23.0。

运行测试：

    go test ./...

## 商户滚动保证金

`reserve.go` 实现商户滚动保证金管理：结算款按规则暂扣一部分，持有期结束后释放；
保证金释放与风险扣款共享同一份商户可用余额，互不复用。

### 核心概念

- **规则（ReserveRule）**：按商户版本化管理（暂扣比例 `RateBP`、持有天数 `HoldDays`、生效时间）。
  登记结算款时取当时生效的规则生成保证金批次，批次固定商户、来源结算、暂扣金额、
  释放日期与规则版本；后续规则调整不会重算已形成的批次。
- **批次（ReserveBatch）**：每笔暂扣构成 `Withheld = Occupied + Released + Frozen + Remaining`，
  累计占用与累计释放都不会超过原暂扣金额。每次变更递增 `Version`，
  释放扫描、风险扣款、人工冻结均基于同一份批次状态串行更新。
- **风险扣款（Deduct）**：按最早到期优先占用批次；已释放的金额不能被迟到扣款重新占用，
  已扣除的部分不会被释放扫描重复释放。
- **补回（Compensate）**：错误扣款只能通过补回记录修正，补回金额进入商户可用余额，
  原扣款记录（金额与占用明细）保持不变。
- **人工冻结（Freeze/Unfreeze）**：冻结部分既不参与释放也不参与扣款占用。

### 幂等与冲突

- 释放批次号（`ReleaseDue` 的 `releaseNo`）与风险扣款号（`Deduct` 的 `deductionNo`）分别幂等：
  同号相同内容返回原结果；金额、原因或目标变化返回 `ErrConflict`。
- 同一结算款重复登记返回原批次，不重复暂扣。

### 主要接口

```go
s := settlement.NewService()
s.RegisterMerchant("m1")
s.SetReserveRule("m1", 1000, 30, effectiveFrom)        // 10% 暂扣，持有 30 天
batch, _ := s.RegisterSettlement("st1", "m1", 100000, confirmedAt)
ded, _ := s.Deduct("D1", "m1", 3000, "chargeback")     // 最早到期优先占用
rel, _ := s.ReleaseDue("R1", time.Now())               // 到期扫描释放
comp, _ := s.Compensate("C1", "D1", 1000, "误扣补回")   // 补回错误扣款
s.Freeze(batch.ID, 500)                                // 人工冻结
bal, _ := s.Balance("m1")                              // 可用余额 + 每笔保证金剩余构成
```

`Balance` 返回商户可用余额及每笔批次的 `Withheld/Occupied/Released/Frozen/Remaining` 构成。
