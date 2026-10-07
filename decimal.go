package settlement

import (
	"fmt"
	"math/big"
	"strings"
)

// Decimal 使用 big.Rat 表示的精确十进制数，避免浮点误差。
type Decimal struct {
	r *big.Rat
}

// NewDecimal 从字符串解析精确十进制数，例如 "123.45"。
func NewDecimal(s string) (Decimal, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Decimal{}, fmt.Errorf("settlement: empty decimal")
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return Decimal{}, fmt.Errorf("settlement: invalid decimal %q", s)
	}
	return Decimal{r: r}, nil
}

// MustDecimal 同 NewDecimal，解析失败时 panic，便于测试与常量定义。
func MustDecimal(s string) Decimal {
	d, err := NewDecimal(s)
	if err != nil {
		panic(err)
	}
	return d
}

// ZeroDecimal 返回零值。
func ZeroDecimal() Decimal { return Decimal{r: new(big.Rat)} }

func (d Decimal) norm() Decimal {
	if d.r == nil {
		return ZeroDecimal()
	}
	return d
}

// Add 返回 d+o。
func (d Decimal) Add(o Decimal) Decimal {
	d, o = d.norm(), o.norm()
	return Decimal{r: new(big.Rat).Add(d.r, o.r)}
}

// Sub 返回 d-o。
func (d Decimal) Sub(o Decimal) Decimal {
	d, o = d.norm(), o.norm()
	return Decimal{r: new(big.Rat).Sub(d.r, o.r)}
}

// Cmp 比较大小：-1 小于，0 相等，1 大于。
func (d Decimal) Cmp(o Decimal) int {
	d, o = d.norm(), o.norm()
	return d.r.Cmp(o.r)
}

// Sign 返回符号：-1、0 或 1。
func (d Decimal) Sign() int {
	d = d.norm()
	return d.r.Sign()
}

// IsZero 判断是否为零。
func (d Decimal) IsZero() bool { return d.Sign() == 0 }

// Equal 判断数值是否相等。
func (d Decimal) Equal(o Decimal) bool { return d.Cmp(o) == 0 }

// String 输出最简十进制形式，尽量不使用分数表示。
func (d Decimal) String() string {
	d = d.norm()
	if d.r.IsInt() {
		return d.r.Num().String()
	}
	return d.r.RatString()
}

// Money 表示某一币种下的一笔精确金额。
type Money struct {
	Amount   Decimal
	Currency string
}

// NewMoney 构造金额。
func NewMoney(amount Decimal, currency string) Money {
	return Money{Amount: amount, Currency: strings.ToUpper(currency)}
}

func (m Money) checkCurrency(o Money) error {
	if m.Currency != o.Currency {
		return fmt.Errorf("settlement: currency mismatch %s vs %s", m.Currency, o.Currency)
	}
	return nil
}

// Add 同币种相加。
func (m Money) Add(o Money) (Money, error) {
	if err := m.checkCurrency(o); err != nil {
		return Money{}, err
	}
	return Money{Amount: m.Amount.Add(o.Amount), Currency: m.Currency}, nil
}

// Sub 同币种相减。
func (m Money) Sub(o Money) (Money, error) {
	if err := m.checkCurrency(o); err != nil {
		return Money{}, err
	}
	return Money{Amount: m.Amount.Sub(o.Amount), Currency: m.Currency}, nil
}

// Cmp 同币种比较。
func (m Money) Cmp(o Money) (int, error) {
	if err := m.checkCurrency(o); err != nil {
		return 0, err
	}
	return m.Amount.Cmp(o.Amount), nil
}
