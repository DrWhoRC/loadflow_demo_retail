package model

import (
	"time"
)

// =====================================================================
// Stream 1: 纯消费 — 商品目录批量导入
// =====================================================================

// Product 商品记录 (Stream 1: Keyless WRR)
// 每行 CSV 直接 INSERT，不关心顺序，纯吞吐
type Product struct {
	ID          uint      `gorm:"primaryKey;autoIncrement"`
	StockCode   string    `gorm:"type:varchar(20);index"`
	Description string    `gorm:"type:text"`
	UnitPrice   float64   `gorm:"type:decimal(10,2)"`
	Country     string    `gorm:"type:varchar(50)"`
	PoolName    string    `gorm:"type:varchar(50)"` // 记录被哪个 pool 消费的（可观测性）
	InsertedAt  time.Time `gorm:"autoCreateTime"`
}

func (Product) TableName() string { return "products" }

// =====================================================================
// Stream 2: 粘性路由 — 客户订单聚合统计
// =====================================================================

// CustomerTransaction 客户交易记录 (Stream 2: Sticky Per Pool)
// 以 CustomerID 为 key，同一客户始终路由到同一 pool
type CustomerTransaction struct {
	ID          uint      `gorm:"primaryKey;autoIncrement"`
	CustomerID  string    `gorm:"type:varchar(20);index:idx_customer"`
	InvoiceNo   string    `gorm:"type:varchar(20)"`
	TotalAmount float64   `gorm:"type:decimal(10,2)"`
	PoolName    string    `gorm:"type:varchar(50)"`
	InsertedAt  time.Time `gorm:"autoCreateTime"`
}

func (CustomerTransaction) TableName() string { return "customer_transactions" }

// =====================================================================
// Stream 3: 强有序 — 订单状态机
// =====================================================================

// Order 订单主表 (Stream 3: StripedPool 强有序)
// 状态机: created → processing → completed
type Order struct {
	InvoiceNo   string     `gorm:"type:varchar(20);primaryKey"`
	Status      string     `gorm:"type:varchar(20);default:'created'"`
	ItemCount   int        `gorm:"default:0"`
	TotalAmount float64    `gorm:"type:decimal(10,2);default:0"`
	CreatedAt   time.Time  `gorm:"autoCreateTime"`
	CompletedAt *time.Time `gorm:"default:null"`
}

func (Order) TableName() string { return "orders" }

// OrderItem 订单明细
type OrderItem struct {
	ID         uint      `gorm:"primaryKey;autoIncrement"`
	InvoiceNo  string    `gorm:"type:varchar(20);index:idx_invoice"`
	StockCode  string    `gorm:"type:varchar(20)"`
	Quantity   int       `gorm:"type:int"`
	UnitPrice  float64   `gorm:"type:decimal(10,2)"`
	InsertedAt time.Time `gorm:"autoCreateTime"`
}

func (OrderItem) TableName() string { return "order_items" }
