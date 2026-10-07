package models

import (
	"time"

	"github.com/shopspring/decimal"
)

// Candle stores OHLCV price data. When backed by TimescaleDB the table
// is converted to a hypertable partitioned on OpenTime for fast
// time-range queries.
type Candle struct {
	ID        uint            `gorm:"primaryKey;autoIncrement"       json:"id"`
	Symbol    string          `gorm:"type:varchar(20);not null;index:idx_candle_lookup" json:"symbol"`
	Interval  string          `gorm:"type:varchar(5);not null;index:idx_candle_lookup"  json:"interval"` // 1m, 5m, 1h, 1d
	OpenTime  time.Time       `gorm:"not null;index;index:idx_candle_lookup,unique"     json:"open_time"`
	CloseTime time.Time       `gorm:"not null"                       json:"close_time"`
	Open      decimal.Decimal `gorm:"type:numeric(20,8);not null"    json:"open"`
	High      decimal.Decimal `gorm:"type:numeric(20,8);not null"    json:"high"`
	Low       decimal.Decimal `gorm:"type:numeric(20,8);not null"    json:"low"`
	Close     decimal.Decimal `gorm:"type:numeric(20,8);not null"    json:"close"`
	Volume    decimal.Decimal `gorm:"type:numeric(20,8);not null"    json:"volume"`
}

func (Candle) TableName() string { return "candles" }
