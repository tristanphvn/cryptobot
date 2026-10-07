package riskmanager

import (
	"errors"
	"testing"

	"cryptobot/internal/database/models"

	"github.com/glebarez/sqlite"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := "file:" + t.Name() + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.AutoMigrate(&models.Position{}, &models.Trade{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func seedOpenPosition(t *testing.T, db *gorm.DB, symbol string) {
	t.Helper()
	if err := db.Create(&models.Position{
		Symbol:        symbol,
		Side:          models.TradeSideBuy,
		Quantity:      decimal.NewFromInt(1),
		AvgEntryPrice: decimal.NewFromInt(100),
		IsOpen:        true,
	}).Error; err != nil {
		t.Fatalf("seed %s: %v", symbol, err)
	}
}

// PreTradeCheck — priority-slot reservation. The gate is the entire
// reason an operator sets PRIORITY_SYMBOL, so cover both the block and
// the satisfied paths explicitly.
func TestPreTradeCheck_PrioritySlotReservation(t *testing.T) {
	equity := decimal.NewFromInt(10000)

	t.Run("priority empty disables gate", func(t *testing.T) {
		db := newTestDB(t)
		// Fill 2/3 with non-priority — last slot is up for grabs.
		seedOpenPosition(t, db, "BTCUSDT")
		seedOpenPosition(t, db, "SOLUSDT")
		rm := New(db, Config{MaxOpenPositions: 3, PrioritySymbol: ""})
		if err := rm.PreTradeCheck("DOGEUSDT", models.TradeSideBuy, equity); err != nil {
			t.Fatalf("expected admit when priority disabled, got %v", err)
		}
	})

	t.Run("slot free still admits non-priority", func(t *testing.T) {
		db := newTestDB(t)
		// 1/3 open — 2 slots left, no reservation yet.
		seedOpenPosition(t, db, "BTCUSDT")
		rm := New(db, Config{MaxOpenPositions: 3, PrioritySymbol: "ETHUSDT"})
		if err := rm.PreTradeCheck("SOLUSDT", models.TradeSideBuy, equity); err != nil {
			t.Fatalf("expected admit with 2 slots left, got %v", err)
		}
	})

	t.Run("last slot reserved blocks non-priority", func(t *testing.T) {
		db := newTestDB(t)
		seedOpenPosition(t, db, "BTCUSDT")
		seedOpenPosition(t, db, "SOLUSDT")
		rm := New(db, Config{MaxOpenPositions: 3, PrioritySymbol: "ETHUSDT"})
		err := rm.PreTradeCheck("DOGEUSDT", models.TradeSideBuy, equity)
		var b Breach
		if !errors.As(err, &b) || b.Rule != "priority_slot_reserved" {
			t.Fatalf("expected priority_slot_reserved breach, got %v", err)
		}
	})

	t.Run("last slot admits priority symbol itself", func(t *testing.T) {
		db := newTestDB(t)
		seedOpenPosition(t, db, "BTCUSDT")
		seedOpenPosition(t, db, "SOLUSDT")
		rm := New(db, Config{MaxOpenPositions: 3, PrioritySymbol: "ETHUSDT"})
		if err := rm.PreTradeCheck("ETHUSDT", models.TradeSideBuy, equity); err != nil {
			t.Fatalf("expected priority symbol admitted, got %v", err)
		}
	})

	t.Run("priority already open releases reservation", func(t *testing.T) {
		db := newTestDB(t)
		seedOpenPosition(t, db, "ETHUSDT")
		seedOpenPosition(t, db, "BTCUSDT")
		rm := New(db, Config{MaxOpenPositions: 3, PrioritySymbol: "ETHUSDT"})
		if err := rm.PreTradeCheck("SOLUSDT", models.TradeSideBuy, equity); err != nil {
			t.Fatalf("expected admit once priority already open, got %v", err)
		}
	})

	t.Run("cap still blocks even priority", func(t *testing.T) {
		db := newTestDB(t)
		seedOpenPosition(t, db, "BTCUSDT")
		seedOpenPosition(t, db, "SOLUSDT")
		seedOpenPosition(t, db, "ADAUSDT")
		rm := New(db, Config{MaxOpenPositions: 3, PrioritySymbol: "ETHUSDT"})
		err := rm.PreTradeCheck("ETHUSDT", models.TradeSideBuy, equity)
		var b Breach
		if !errors.As(err, &b) || b.Rule != "max_open_positions" {
			t.Fatalf("expected max_open_positions breach at cap, got %v", err)
		}
	})

	t.Run("SELL bypasses the gate", func(t *testing.T) {
		db := newTestDB(t)
		seedOpenPosition(t, db, "BTCUSDT")
		seedOpenPosition(t, db, "SOLUSDT")
		rm := New(db, Config{MaxOpenPositions: 3, PrioritySymbol: "ETHUSDT"})
		// SELL closes a position — reservation logic only guards BUYs.
		if err := rm.PreTradeCheck("SOLUSDT", models.TradeSideSell, equity); err != nil {
			t.Fatalf("expected SELL admitted regardless of priority, got %v", err)
		}
	})
}

func TestGetStatus_PriorityReserved(t *testing.T) {
	db := newTestDB(t)
	seedOpenPosition(t, db, "BTCUSDT")
	seedOpenPosition(t, db, "SOLUSDT")
	rm := New(db, Config{MaxOpenPositions: 3, PrioritySymbol: "ETHUSDT"})

	s := rm.GetStatus()
	if s.PrioritySymbol != "ETHUSDT" {
		t.Errorf("priority_symbol: got %q, want ETHUSDT", s.PrioritySymbol)
	}
	if !s.PriorityReserved {
		t.Error("priority_reserved: expected true with 2/3 open and no ETH")
	}

	seedOpenPosition(t, db, "ETHUSDT")
	s2 := rm.GetStatus()
	if s2.PriorityReserved {
		t.Error("priority_reserved: expected false once ETH already open")
	}
}
