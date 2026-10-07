# go-settlement

开发环境：Go 1.23.0。

运行测试：

    go test ./...

## 商户滚动保证金（reserve 包）

结算款按当时生效的规则暂扣一部分作为保证金，持有期结束后释放到商户可用余额；
保证金释放与风险扣款共享同一份可用余额，任何一笔金额只能被使用一次。

### 核心概念

- **规则版本（ReserveRule）**：每次调整生成新的不可变版本（暂扣比例 `RateBP`、持有天数 `HoldDays`、生效时间）。
- **保证金批次（ReserveBatch）**：登记结算款时按当时生效的规则生成，固定商户、来源结算、暂扣金额、释放日期和规则版本；后续规则调整不会重算已有批次。
- **风险扣款（RiskDeduction）**：按释放日期最早优先占用批次；每批 `Deducted + Released` 永不超过原暂扣金额。
- **释放扫描（ReleaseRun）**：按批次逐笔处理，只有未被占用的余额（`Amount - Deducted - Released`）进入商户可用余额。
- **补回（Compensation）**：错误扣款只通过补回记录修正，原扣款记录保持不变。

### API 一览（reserve/service.go）

```go
s := reserve.NewService()
s.RegisterMerchant("m1", "Acme")
s.CreateRule("m1", "rule-1", 1000, 30, effectiveFrom)      // 暂扣 10%，持有 30 天
s.RegisterSettlement("m1", "st1", 10000, confirmedAt)      // 生成保证金批次 1000
s.Deduct("m1", "d1", 400, "fraud")                         // 风险扣款，最早到期优先
s.ScanReleases("m1", "run1", asOf)                         // 到期释放扫描
s.Compensate("m1", "c1", "d1", 400, "wrong deduction")     // 补回错误扣款
view, _ := s.Balance("m1")                                 // 可用余额 + 每批剩余构成
```

### 并发与幂等

- 所有变更在同一互斥锁下提交，并递增批次的乐观版本号（`ReserveBatch.Version`）；
  释放扫描、风险扣款与人工冻结并发时基于同一批次版本更新，已释放金额不会被迟到扣款
  重新占用，已扣除部分也不会被扫描重复释放。
- 结算登记、释放批次号、风险扣款号、补回号分别幂等：同号相同内容返回原结果；
  金额、原因或目标变化返回 `ErrConflict`。

### 测试

    go test ./reserve/... -race
