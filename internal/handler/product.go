package handler

import (
	"encoding/json"
	"fmt"
	"log"
	"sync/atomic"

	"github.com/DrWhoRC/loadflow_demo_retail/internal/model"
	"gorm.io/gorm"
)

// ProductMsg Stream 1 发送给 handler 的消息格式
type ProductMsg struct {
	StockCode   string  `json:"stock_code"`
	Description string  `json:"description"`
	UnitPrice   float64 `json:"unit_price"`
	Country     string  `json:"country"`
	PoolName    string  `json:"pool_name"` // 由 runtime 路由后标记（可选）
}

// ProductHandler Stream 1: 纯消费 — 商品目录批量导入
// 不关心 key，不关心顺序，纯吞吐
type ProductHandler struct {
	db       *gorm.DB
	inserted uint64
}

func NewProductHandler(db *gorm.DB) *ProductHandler {
	return &ProductHandler{db: db}
}

func (h *ProductHandler) Handle(msg []byte) error {
	var pm ProductMsg
	if err := json.Unmarshal(msg, &pm); err != nil {
		return fmt.Errorf("unmarshal product msg: %w", err)
	}

	record := model.Product{
		StockCode:   pm.StockCode,
		Description: pm.Description,
		UnitPrice:   pm.UnitPrice,
		Country:     pm.Country,
		PoolName:    pm.PoolName,
	}

	if err := h.db.Create(&record).Error; err != nil {
		return fmt.Errorf("insert product: %w", err)
	}

	cnt := atomic.AddUint64(&h.inserted, 1)
	if cnt%1000 == 0 {
		log.Printf("[ProductHandler] Inserted %d products", cnt)
	}
	return nil
}

func (h *ProductHandler) InsertedCount() uint64 {
	return atomic.LoadUint64(&h.inserted)
}
