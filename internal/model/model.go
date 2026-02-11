package model

import (
	"database/sql"
	"time"
)

type Printer struct {
	ID               int64
	Name             string
	URI              string
	PPDName          string
	Location         string
	Info             string
	Geo              string
	Org              string
	OrgUnit          string
	State            int
	Accepting        bool
	Shared           bool
	IsTemporary      bool
	IsDefault        bool
	JobSheetsDefault string
	DefaultOptions   string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

const DefaultPPDName = "CUPS-Golang-Generic.ppd"

type Class struct {
	ID               int64
	Name             string
	Location         string
	Info             string
	State            int
	Accepting        bool
	IsDefault        bool
	JobSheetsDefault string
	DefaultOptions   string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

type Job struct {
	ID           int64
	PrinterID    int64
	Name         string
	UserName     string
	OriginHost   string
	Options      string
	State        int
	StateReason  string
	Impressions  int
	SubmittedAt  time.Time
	ProcessingAt *time.Time
	CompletedAt  *time.Time
}

type Document struct {
	ID             int64
	JobID          int64
	FileName       string
	MimeType       string
	FormatSupplied string
	NameSupplied   string
	SizeBytes      int64
	Path           string
	CreatedAt      time.Time
}

type User struct {
	ID           int64
	Username     string
	PasswordHash string
	DigestHA1    string
	IsAdmin      bool
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

type Subscription struct {
	ID           int64
	PrinterID    sql.NullInt64
	JobID        sql.NullInt64
	Events       string
	LeaseSecs    int64
	Owner        string
	RecipientURI string
	PullMethod   string
	TimeInterval int64
	UserData     []byte
	CreatedAt    time.Time
}

type Notification struct {
	ID             int64
	SubscriptionID int64
	Event          string
	CreatedAt      time.Time
}
