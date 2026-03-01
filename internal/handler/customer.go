package handler

import (
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"sync/atomic"

	"github.com/DrWhoRC/loadflow_demo_retail/internal/model"
	"gorm.io/gorm"
)

// CustomerMsg Stream 2 发送给 handler 的消息格式
type CustomerMsg struct {
	CustomerID string  `json:"customer_id"`
	InvoiceNo  string  `json:"invoice_no"`
	Quantity   int     `json:"quantity"`
	UnitPrice  float64 `json:"unit_price"`
	PoolName   string  `json:"pool_name"`
}

// CustomerHandler Stream 2: 粘性路由 — 客户订单聚合
// 以 CustomerID 为 key，同一客户始终路由到同一个 pool
// 同时在内存中维护 per-customer 的累计消费金额（体现粘性的业务价值：本地缓存命中）
type CustomerHandler struct {
	db       *gorm.DB
	inserted uint64

	// 内存聚合缓存（体现粘性路由的本地性优势）
	mu         sync.RWMutex
	custTotals map[string]float64 // customerID -> 累计金额
}

func NewCustomerHandler(db *gorm.DB) *CustomerHandler {
	return &CustomerHandler{
		db:         db,
		custTotals: make(map[string]float64),
	}
}

func (h *CustomerHandler) Handle(msg []byte) error {
	var cm CustomerMsg
	if err := json.Unmarshal(msg, &cm); err != nil {
		return fmt.Errorf("unmarshal customer msg: %w", err)
	}

	totalAmount := float64(cm.Quantity) * cm.UnitPrice

	// 更新内存聚合缓存（粘性路由 => 同一 customer 总是在同一个 pool 的 goroutine 里，
	// 并发冲突概率极低，这就是粘性的业务价值）
	h.mu.Lock()
	h.custTotals[cm.CustomerID] += totalAmount
	h.mu.Unlock()

	record := model.CustomerTransaction{
		CustomerID:  cm.CustomerID,
		InvoiceNo:   cm.InvoiceNo,
		TotalAmount: totalAmount,
		PoolName:    cm.PoolName,
	}

	if err := h.db.Create(&record).Error; err != nil {
		return fmt.Errorf("insert customer transaction: %w", err)
	}

	cnt := atomic.AddUint64(&h.inserted, 1)
	if cnt%1000 == 0 {
		log.Printf("[CustomerHandler] Inserted %d transactions", cnt)
	}
	return nil
}

func (h *CustomerHandler) InsertedCount() uint64 {
	return atomic.LoadUint64(&h.inserted)
}

// GetCustomerTotal 获取某个客户的累计消费金额（用于验证粘性缓存）
func (h *CustomerHandler) GetCustomerTotal(customerID string) float64 {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.custTotals[customerID]
}
