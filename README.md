# go-settlement

开发环境：Go 1.23.0。

运行测试：

    go test ./...

## 收款账户授权管理

结算付款只能使用**已审核通过且已过安全等待期**的收款账户版本。账户内容在申请提交时冻结摘要，任何字段变更都必须产生新的申请版本，旧审批不会套用到修改后的银行信息上。

### 核心概念

- `Application`：账户新增/变更申请内容，包含账户持有人、银行标识、账号安全摘要（`DigestAccount` 计算，绝不保存明文账号）、适用币种和生效时间。
- `Version`：申请的不可变版本。状态机：`PENDING → AWAITING_EFFECT → ACTIVE`，终止态为 `REJECTED` / `REVOKED` / `SUPERSEDED`。
- `Instruction`：结算指令，创建时冻结所采用授权账户版本的证据（`AccountEvidence`）。

### 审核与生效

- 每个版本需要 **2 名不同审核人**通过（`MandateConfig.RequiredApprovals`，最低为 2）；任一审核人拒绝即终止该版本。
- 审核事件号（`eventID`）幂等：同一事件重复提交为无操作；同一审核人不能对同一版本重复决定。
- 最终审核通过后进入安全等待期（`MandateConfig.WaitingPeriod`），等待期结束且到达生效时间后，由 `ActivateDue(now)` 激活；激活新版本时旧版本自动置为 `SUPERSEDED`。
- 生效时间早于“最终审核时间 + 等待期”的申请会在审核完成时被拒绝（`ErrEffectiveTooEarly`）。

### 结算指令

- `CreateInstruction` 记录当前 ACTIVE 版本的版本号与内容摘要作为证据，币种必须被该版本覆盖。
- `Send` 前重新校验绑定版本：若版本已被撤销（`REVOKED`）或被新版本替代（`SUPERSEDED`），指令转为 `STOPPED` 并记录明确原因，不会静默切换到新账户。
- 已发送的指令永久保留原版本证据，即使账户之后切换到新版本。

### 查询与脱敏

- 查询接口：`Submit`（申请）、`Approve`/`Reject`（审核）、`Revoke`（撤销）、`ActivateDue`（到期生效）、`History`/`GetVersion`/`ActiveVersion`（版本历史与当前版本）。
- 所有查询返回 `VersionView`，账号仅以掩码摘要（`MaskDigest`，形如 `ab12****cd34`）出现，明文账号不会出现在普通查询和日志中。

### 示例

```go
m := settlement.NewMandateStore(settlement.MandateConfig{WaitingPeriod: 24 * time.Hour})
v, _ := m.Submit(settlement.Application{
    HolderName:    "Alice Trading Ltd",
    BankID:        "BKCHCNBJ",
    AccountDigest: settlement.DigestAccount("6222020200112233445"),
    Currencies:    []string{"CNY"},
    EffectiveAt:   now.Add(24 * time.Hour),
}, now)
_ = m.Approve(v.AccountID, v.VersionNo, "reviewer-1", "evt-1", now)
_ = m.Approve(v.AccountID, v.VersionNo, "reviewer-2", "evt-2", now)
m.ActivateDue(now.Add(24 * time.Hour))

ins := settlement.NewInstructionStore(m)
created, _ := ins.CreateInstruction(v.AccountID, 10000, "CNY", now)
_ = ins.Send(created.ID, now)
```

### 测试

`mandate_test.go` 与 `instruction_test.go` 覆盖：审批门槛与重复审核、事件幂等、拒绝终止、等待期边界、字段变更产生新版本、账户切换并发竞态、撤销/替代后旧版本指令停止、已发送指令保留原版本证据、查询脱敏。
