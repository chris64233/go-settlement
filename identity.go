package settlement

import "time"

// IdentityType 描述收款方的税务身份类型，例如居民企业、非居民个人。
type IdentityType string

const (
	IdentityResidentCompany    IdentityType = "RESIDENT_COMPANY"
	IdentityResidentIndividual IdentityType = "RESIDENT_INDIVIDUAL"
	IdentityNonResidentCompany IdentityType = "NON_RESIDENT_COMPANY"
	IdentityNonResidentPerson  IdentityType = "NON_RESIDENT_INDIVIDUAL"
)

// TaxIdentity 是一次身份登记的内容。有效期为左闭右开区间 [ValidFrom, ValidTo)。
type TaxIdentity struct {
	PartyID   string
	Region    string
	Type      IdentityType
	ValidFrom time.Time
	ValidTo   time.Time
}

// IdentityVersion 是身份登记的一个不可变版本，每次登记生成一个新版本号。
type IdentityVersion struct {
	Version int
	TaxIdentity
}

// Contains 判断给定交易发生日是否落在身份有效期内（左闭右开）。
func (v IdentityVersion) Contains(day time.Time) bool {
	day = dateOnly(day)
	from := dateOnly(v.ValidFrom)
	to := dateOnly(v.ValidTo)
	return !day.Before(from) && day.Before(to)
}

func dateOnly(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}
