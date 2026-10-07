package settlement

import (
	"fmt"
	"math/big"
	"strings"
)

// Amount 使用 big.Rat 表示的精确十进制金额，避免浮点误差。
type Amount struct {
	r *big.Rat
}

// ParseAmount 解析十进制字符串（如 "123.45"）为精确金额。
func ParseAmount(s string) (Amount, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Amount{}, fmt.Errorf("settlement: empty amount")
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return Amount{}, fmt.Errorf("settlement: invalid amount %q", s)
	}
	return Amount{r: r}, nil
}

// MustAmount 解析失败时 panic，仅用于测试与常量。
func MustAmount(s string) Amount {
	a, err := ParseAmount(s)
	if err != nil {
		panic(err)
	}
	return a
}

func amountFromRat(r *big.Rat) Amount {
	return Amount{r: new(big.Rat).Set(r)}
}

// Zero 返回零金额。
func Zero() Amount { return Amount{r: new(big.Rat)} }

func (a Amount) norm() Amount {
	if a.r == nil {
		return Zero()
	}
	return a
}

// Add 返回两金额之和。
func (a Amount) Add(b Amount) Amount {
	a, b = a.norm(), b.norm()
	return amountFromRat(new(big.Rat).Add(a.r, b.r))
}

// Sub 返回 a-b 的差。
func (a Amount) Sub(b Amount) Amount {
	a, b = a.norm(), b.norm()
	return amountFromRat(new(big.Rat).Sub(a.r, b.r))
}

// Cmp 比较大小：-1 小于，0 相等，1 大于。
func (a Amount) Cmp(b Amount) int {
	a, b = a.norm(), b.norm()
	return a.r.Cmp(b.r)
}

// Sign 返回金额符号。
func (a Amount) Sign() int {
	a = a.norm()
	return a.r.Sign()
}

// IsZero 报告金额是否为零。
func (a Amount) IsZero() bool { return a.Sign() == 0 }

// String 输出最简十进制表示；无法精确表示为有限小数时保留 10 位小数。
func (a Amount) String() string {
	a = a.norm()
	if a.r.IsInt() {
		return a.r.Num().String()
	}
	if s, ok := decimalString(a.r); ok {
		return s
	}
	return a.r.FloatString(10)
}

// decimalString 在分母只含因子 2 和 5 时给出精确有限小数。
func decimalString(r *big.Rat) (string, bool) {
	denom := new(big.Int).Set(r.Denom())
	count := func(p int64) int {
		n := 0
		prime := big.NewInt(p)
		for {
			q, m := new(big.Int).QuoRem(denom, prime, new(big.Int))
			if m.Sign() != 0 {
				return n
			}
			denom = q
			n++
		}
	}
	twos := count(2)
	fives := count(5)
	if denom.Cmp(big.NewInt(1)) != 0 {
		return "", false
	}
	scale := twos
	if fives > scale {
		scale = fives
	}
	num := new(big.Int).Set(r.Num())
	mul2 := new(big.Int).Exp(big.NewInt(2), big.NewInt(int64(scale-twos)), nil)
	mul5 := new(big.Int).Exp(big.NewInt(5), big.NewInt(int64(scale-fives)), nil)
	num.Mul(num, mul2)
	num.Mul(num, mul5)
	neg := num.Sign() < 0
	if neg {
		num.Neg(num)
	}
	digits := num.String()
	for len(digits) <= scale {
		digits = "0" + digits
	}
	intPart := digits[:len(digits)-scale]
	fracPart := strings.TrimRight(digits[len(digits)-scale:], "0")
	if neg {
		intPart = "-" + intPart
	}
	if fracPart == "" {
		return intPart, true
	}
	return intPart + "." + fracPart, true
}

// MarshalJSON 以字符串形式编码，保证精度不丢失。
func (a Amount) MarshalJSON() ([]byte, error) {
	return []byte(`"` + a.String() + `"`), nil
}

// UnmarshalJSON 从字符串或数字解码。
func (a *Amount) UnmarshalJSON(data []byte) error {
	s := strings.Trim(string(data), `"`)
	parsed, err := ParseAmount(s)
	if err != nil {
		return err
	}
	*a = parsed
	return nil
}
