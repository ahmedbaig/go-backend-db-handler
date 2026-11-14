package shop_main

import (
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"
)

const TableNameOrder = "orders"

type Order struct {
	ID         *int32         `gorm:"column:id;type:INTEGER;primaryKey" json:"id"`
	CustomerID int32          `gorm:"column:customer_id;not null" json:"customer_id"`
	Total      float64        `gorm:"column:total" json:"total"`
	Meta       datatypes.JSON `gorm:"column:meta" json:"meta"`
	Note       *string        `gorm:"column:note" json:"note"`
	PlacedAt   time.Time      `gorm:"column:placed_at" json:"placed_at"`
	DeletedAt  gorm.DeletedAt `gorm:"column:deleted_at" json:"deleted_at"`
	internal   string
	Ignored    string `gorm:"-"`
}

func (*Order) TableName() string {
	return TableNameOrder
}

type helper struct{}
