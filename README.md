# go-settlement

开发环境：Go 1.23.0。

运行测试：

    go test ./...

## 违约处置（default waterfall）

`default_waterfall.go` 实现清算参与方违约处置：参与方未能按时补足净应付头寸时，
系统按既定顺序动用担保资源，并留下可追溯的资金使用台账。

### 登记与发起

- `RegisterParticipant` 登记参与方的可用现金与专属违约基金份额；
  `AddCollateral` 登记专属担保品；`RegisterPosition` 登记未结清净应付头寸；
  `ContributeMutualFund` 注入共同违约基金。
- `PlanDisposal(disposalID, positionID)` 发起处置：冻结头寸、各层资源余额、
  估值版本和处置规则版本。已撤销的担保品、被其他处置占用的资源、
  已冻结的头寸都会被拒绝。

### 处置顺序

`ExecuteDisposal` 严格按以下顺序弥补缺口，前一层未用尽前不动用后一层：

1. 参与方现金（`LayerCash`）
2. 参与方专属担保品（`LayerCollateral`，按担保品 ID 顺序）
3. 参与方专属违约基金份额（`LayerFundShare`）
4. 共同违约基金（`LayerMutualFund`）

所有资源耗尽后仍有缺口时，`Disposal.RemainingGap` 保留剩余缺口，
处置不会被标记为已足额覆盖。

### 并发裁决

计划以冻结版本裁决：执行前若头寸、资源或规则版本发生变化（如补缴到账、
担保品估值变化），旧计划返回 `ErrConflict` 且不消耗任何资源；若处置先完成，
迟到的资源变更只影响剩余余额，不能改写已生成的使用明细。

### 幂等与一致性

同一处置号以相同内容重复提交返回原结果；头寸、规则或资源版本变化返回冲突。
余额扣减与使用台账在同一临界区内完成，失败的执行不会只消耗其中一层资源。

### 查询与追溯

- `GetDisposal`：违约缺口与剩余缺口；
- `UsageRecords`：各层使用明细，每笔记录可追溯到被冻结的头寸与资源版本；
- `RemainingResources`：处置后的剩余资源；
- `MutualFundAllocations`：共同基金分摊查询。

### 失败后的状态

执行因版本冲突失败时，所有资源余额保持不变、不生成任何使用记录，
处置保持 `PLANNED` 状态；资源变化后可重新发起新的处置计划。
