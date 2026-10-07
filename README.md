# go-settlement

开发环境：Go 1.23.0。

运行测试：

    go test ./...

## 违约处置（Default Waterfall）

当参与方未能按时补足净应付头寸时，系统按既定顺序动用担保资源弥补缺口。

### 处置顺序

资金严格按以下层级依次动用，前一层未用尽前不动用后一层：

1. 参与方现金（`cash`）
2. 参与方专属担保品（`collateral`）
3. 参与方违约基金份额（`fund_share`）
4. 共同违约基金池（`mutual_fund`）

任一层全部动用后仍有缺口时，处置记录保留 `RemainingGap`，且 `Completed()` 为 false，
不会把处置标记为已完成。

### 使用流程

1. `RegisterParticipant` 登记参与方及其现金、专属担保品、违约基金份额；
   `RegisterPosition` 登记未结清的净应付头寸；`FundMutualFund` 向共同基金池注资。
2. `InitiateDisposal(disposalID, positionID)` 发起处置，冻结头寸、各层资源余额、
   估值版本和处置规则版本。已撤销或被其他处置占用的头寸/资源不能再次使用。
3. `ExecuteDisposal(disposalID)` 按冻结版本执行瀑布式扣减，余额扣减与使用台账
   在同一临界区内完成，二者始终一致。

### 并发与幂等

- 发起处置后资源被冻结：补缴（`TopUpCash`）、估值变化（`RevalueCollateral`）、
  份额调整（`AdjustFundShare`）及其他处置均返回冲突。
- 资源先发生变化（版本推进）时，按旧冻结版本执行返回 `ErrConflict`，且不会消耗任何一层资源。
- 处置先完成时，迟到的资源变更只作用于剩余余额，不能改写已生成的使用明细。
- 同一处置号相同内容重复提交返回原结果；头寸、规则或资源版本变化返回 `ErrConflict`。

### 查询与追溯

- `GetDisposal` 查询违约缺口和各层使用明细；
- `RemainingResources` 查询参与方剩余资源；
- `MutualFundAllocation` 查询共同基金分摊和池余额；
- 每笔 `Usage` 记录都携带被冻结的头寸快照和资源版本快照，可完整追溯。

### 失败后的状态

- 发起或执行返回冲突时，不扣减任何余额、不写入台账，处置保持未执行状态，
  可在版本对齐后重新发起或执行。
- 执行成功但资源不足时，已动用的各层按台账如实扣减，`RemainingGap` 记录未弥补缺口，
  头寸保持未结清，处置不会被标记为完成。
