package handler

import (
	"fmt"
	"log"
	"sync/atomic"
	"time"

	"github.com/DrWhoRC/loadflow_demo_retail/internal/csvloader"
	"github.com/DrWhoRC/loadflow_demo_retail/internal/model"
	"gorm.io/gorm"
)

// OrderHandler Stream 3: 强有序 — 订单状态机
// 以 InvoiceNo 为 key，提交到 StripedPool
// 按顺序执行: created → item_added(×N) → completed
// 如果乱序，UPDATE 在 INSERT 之前执行就会失败 → 这就是强有序的业务价值证明
type OrderHandler struct {
	db        *gorm.DB
	processed uint64
	errors    uint64
}

func NewOrderHandler(db *gorm.DB) *OrderHandler {
	return &OrderHandler{db: db}
}

// ProcessOrder 处理一个完整的订单事件序列（在 StripedPool 的同一 stripe 中串行执行）
// 事件序列: created → item_added(×N) → completed
func (h *OrderHandler) ProcessOrder(events csvloader.OrderEvents) error {
	invoiceNo := events.InvoiceNo

	// Step 1: 创建订单（status = 'created'）
	order := model.Order{
		InvoiceNo: invoiceNo,
		Status:    "created",
	}
	if err := h.db.Create(&order).Error; err != nil {
		atomic.AddUint64(&h.errors, 1)
		return fmt.Errorf("create order %s: %w", invoiceNo, err)
	}

	// Step 2: 逐个添加明细（status -> 'processing'）
	var totalAmount float64
	for i, item := range events.Items {
		orderItem := model.OrderItem{
			InvoiceNo: invoiceNo,
			StockCode: item.StockCode,
			Quantity:  item.Quantity,
			UnitPrice: item.Price,
		}
		if err := h.db.Create(&orderItem).Error; err != nil {
			atomic.AddUint64(&h.errors, 1)
			return fmt.Errorf("add item to order %s (item %d): %w", invoiceNo, i, err)
		}
		totalAmount += float64(item.Quantity) * item.Price

		// 第一个 item 写入后，更新状态为 processing
		if i == 0 {
			if err := h.db.Model(&model.Order{}).
				Where("invoice_no = ? AND status = 'created'", invoiceNo).
				Update("status", "processing").Error; err != nil {
				atomic.AddUint64(&h.errors, 1)
				return fmt.Errorf("update order %s to processing: %w", invoiceNo, err)
			}
		}
	}

	// Step 3: 完成订单（status -> 'completed'）
	now := time.Now()
	if err := h.db.Model(&model.Order{}).
		Where("invoice_no = ? AND status = 'processing'", invoiceNo).
		Updates(map[string]interface{}{
			"status":       "completed",
			"item_count":   len(events.Items),
			"total_amount": totalAmount,
			"completed_at": &now,
		}).Error; err != nil {
		atomic.AddUint64(&h.errors, 1)
		return fmt.Errorf("complete order %s: %w", invoiceNo, err)
	}

	cnt := atomic.AddUint64(&h.processed, 1)
	if cnt%500 == 0 {
		log.Printf("[OrderHandler] Processed %d orders", cnt)
	}
	return nil
}

func (h *OrderHandler) ProcessedCount() uint64 {
	return atomic.LoadUint64(&h.processed)
}

func (h *OrderHandler) ErrorCount() uint64 {
	return atomic.LoadUint64(&h.errors)
}
