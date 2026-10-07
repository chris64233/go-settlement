package settlement

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

var (
	// ErrRuleNotFound 表示没有已生效的限额规则。
	ErrRuleNotFound = errors.New("settlement: no effective limit rule")
	// ErrInstructionNotFound 表示指令不存在。
	ErrInstructionNotFound = errors.New("settlement: instruction not found")
	// ErrInvalidState 表示指令当前状态不允许该操作（例如迟到取消）。
	ErrInvalidState = errors.New("settlement: invalid instruction state")
)

// LimitExceededError 表示某一层限额不足，整笔指令被拒绝。
type LimitExceededError struct {
	Layer   string // single / bilateral_net / unilateral_total
	Limit   int64
	Needed  int64
	Message string
}

func (e *LimitExceededError) Error() string {
	return fmt.Sprintf("settlement: %s limit exceeded (limit=%d needed=%d)", e.Layer, e.Limit, e.Needed)
}

// Service 提供限额配置、指令受理与占用查询。
// 所有状态变更都在同一把互斥锁内完成，保证占用检查与
// 占用增加处于同一持久化边界，并发提交不会绕过限额。
type Service struct {
	mu    sync.Mutex
	now   func() time.Time
	seq   int64
	rules map[ruleKey][]*LimitRule

	instructions map[string]*Instruction
	byRequest    map[string]string // requestID -> instructionID，用于幂等受理

	// unilateral 记录单方总敞口占用：参与方+币种+价值日 -> 已占用（持有+已用）。
	unilateral map[occKey]int64
	// net 记录有向双边净敞口：付款方->收款方 的净额。
	net map[flowKey]int64
}

type occKey struct {
	participant string
	currency    string
	valueDay    string
}

type flowKey struct {
	from, to string
	currency string
	valueDay string
}

// NewService 创建服务。clock 用于确定规则生效时间，传 nil 使用系统时间。
func NewService(clock func() time.Time) *Service {
	if clock == nil {
		clock = time.Now
	}
	return &Service{
		now:          clock,
		rules:        make(map[ruleKey][]*LimitRule),
		instructions: make(map[string]*Instruction),
		byRequest:    make(map[string]string),
		unilateral:   make(map[occKey]int64),
		net:          make(map[flowKey]int64),
	}
}

func dayOf(t time.Time) string {
	return t.UTC().Format("2006-01-02")
}
