# go-settlement

开发环境：Go 1.23.0（`go.mod` 声明 1.18 以兼容更低版本工具链）。

运行测试：

    go test ./...

## 收款账户授权管理

结算付款只能使用「已审核通过且过了安全等待期」的账户版本，任何银行信息修改都会产生新的申请版本，旧审批不会套用到新内容上。

### 账户申请与版本（account.go）

- `AccountService.Submit` 提交账户新增/变更申请，内容包含账户持有人、银行标识、适用币种、期望生效时间；账号明文只在提交时用于计算安全摘要（SHA-256）与掩码（`****` + 后四位），不会被保存。
- 申请创建时冻结内容摘要（`ContentDigest`），任何字段变化都必须通过新的 `Submit` 产生新版本，版本号单调递增。
- 版本状态机：`pending → waiting → effective`，终止态为 `rejected` / `revoked` / `superseded`。

### 审核与等待期

- 需要 `ApprovalQuorum = 2` 名不同审核人通过；任一审核人拒绝即终止该版本。
- 同一审核人不能对同一版本重复决定；审核事件号（`eventID`）幂等，重复投递直接忽略。
- 审核通过后进入 `WaitingPeriod = 24h` 安全等待期，`ActivateDue(now)` 在等待期结束（含边界）后生效，并自动将旧的生效版本置为 `superseded`。
- `Revoke` 可撤销等待中或已生效的版本。

### 结算指令（instruction.go）

- `CreateInstruction` 创建指令时记录当前生效账户版本的证据：版本号、内容摘要、账号摘要与掩码。
- `SendInstruction` 发送前重新校验记录的版本：若已被撤销（`revoked`）或被新版本替代（`superseded`），指令停止并写入明确的 `HaltReason`。
- 已发送的指令永久保留原版本证据，不会静默切换到新账户。

### 查询与脱敏

- `GetVersion` / `History` / `GetInstruction` 提供版本历史与指令查询，均不含明文账号。
- `AccountVersion.String()` 与 `Instruction.String()` 只输出掩码账号与摘要，可安全写入日志。
