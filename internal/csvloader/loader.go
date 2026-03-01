package csvloader

import (
	"encoding/csv"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
)

// RawRecord 代表 CSV 中的一行原始记录
type RawRecord struct {
	Invoice     string
	StockCode   string
	Description string
	Quantity    int
	InvoiceDate time.Time
	Price       float64
	CustomerID  string
	Country     string
}

// OrderEvents 代表一个 Invoice 的完整事件序列（Stream 3 用）
type OrderEvents struct {
	InvoiceNo string
	Items     []OrderItemEvent
}

// OrderItemEvent 代表一个订单明细事件
type OrderItemEvent struct {
	StockCode string
	Quantity  int
	Price     float64
}

// LoadResult 加载结果
type LoadResult struct {
	// Stream 1: 所有行（无 key，纯消费）
	AllRecords []RawRecord

	// Stream 2: 按 CustomerID 分组（粘性路由用）
	// 返回所有记录，handler 里用 CustomerID 做 key
	CustomerRecords []RawRecord

	// Stream 3: 按 InvoiceNo 分组并排序的事件序列（强有序用）
	OrderEventsList []OrderEvents
}

// LoadCSV 加载 CSV 文件，返回三条 stream 的数据
// maxRows: 最大读取行数，0 表示全部读取
func LoadCSV(filePath string, maxRows int) (*LoadResult, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("open csv: %w", err)
	}
	defer f.Close()

	reader := csv.NewReader(f)
	reader.LazyQuotes = true
	reader.TrimLeadingSpace = true

	// 跳过 header
	header, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("read header: %w", err)
	}
	log.Printf("[CSVLoader] Header: %v", header)

	var allRecords []RawRecord
	var customerRecords []RawRecord               // 有 CustomerID 的记录
	orderMap := make(map[string][]OrderItemEvent) // InvoiceNo -> items
	orderInvoices := make([]string, 0)            // 保持 Invoice 出现顺序

	rowCount := 0
	skipped := 0

	for {
		if maxRows > 0 && rowCount >= maxRows {
			break
		}

		row, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			skipped++
			continue
		}

		if len(row) < 8 {
			skipped++
			continue
		}

		record, err := parseRow(row)
		if err != nil {
			skipped++
			continue
		}

		// 过滤无效数据（退货、数量<=0、价格<=0）
		if record.Quantity <= 0 || record.Price <= 0 {
			skipped++
			continue
		}

		rowCount++

		// Stream 1: 所有有效记录
		allRecords = append(allRecords, record)

		// Stream 2: 需要有 CustomerID 的记录
		if record.CustomerID != "" && record.CustomerID != "0" {
			customerRecords = append(customerRecords, record)
		}

		// Stream 3: 按 InvoiceNo 聚合（只处理纯数字 Invoice，排除 C 开头的取消单）
		if record.Invoice != "" && !strings.HasPrefix(record.Invoice, "C") {
			if _, exists := orderMap[record.Invoice]; !exists {
				orderInvoices = append(orderInvoices, record.Invoice)
			}
			orderMap[record.Invoice] = append(orderMap[record.Invoice], OrderItemEvent{
				StockCode: record.StockCode,
				Quantity:  record.Quantity,
				Price:     record.Price,
			})
		}
	}

	// 构建 Stream 3 的 OrderEvents 列表
	var orderEventsList []OrderEvents
	for _, inv := range orderInvoices {
		items := orderMap[inv]
		if len(items) > 0 {
			orderEventsList = append(orderEventsList, OrderEvents{
				InvoiceNo: inv,
				Items:     items,
			})
		}
	}

	log.Printf("[CSVLoader] Loaded: total=%d, customerRecords=%d, orders=%d (skipped=%d)",
		len(allRecords), len(customerRecords), len(orderEventsList), skipped)

	return &LoadResult{
		AllRecords:      allRecords,
		CustomerRecords: customerRecords,
		OrderEventsList: orderEventsList,
	}, nil
}

// parseRow 解析一行 CSV 数据
func parseRow(row []string) (RawRecord, error) {
	// Invoice,StockCode,Description,Quantity,InvoiceDate,Price,Customer ID,Country
	quantity, err := strconv.Atoi(strings.TrimSpace(row[3]))
	if err != nil {
		// 尝试 float 解析（有些行可能是 12.0）
		qf, err2 := strconv.ParseFloat(strings.TrimSpace(row[3]), 64)
		if err2 != nil {
			return RawRecord{}, fmt.Errorf("parse quantity: %w", err)
		}
		quantity = int(qf)
	}

	price, err := strconv.ParseFloat(strings.TrimSpace(row[5]), 64)
	if err != nil {
		return RawRecord{}, fmt.Errorf("parse price: %w", err)
	}

	invoiceDate, err := time.Parse("2006-01-02 15:04:05", strings.TrimSpace(row[4]))
	if err != nil {
		// 尝试备选格式
		invoiceDate, err = time.Parse("1/2/2006 15:04", strings.TrimSpace(row[4]))
		if err != nil {
			invoiceDate = time.Now() // fallback
		}
	}

	// Customer ID 可能是 "13085.0" 形式
	customerID := strings.TrimSpace(row[6])
	if strings.Contains(customerID, ".") {
		if f, err := strconv.ParseFloat(customerID, 64); err == nil {
			customerID = strconv.Itoa(int(math.Round(f)))
		}
	}

	return RawRecord{
		Invoice:     strings.TrimSpace(row[0]),
		StockCode:   strings.TrimSpace(row[1]),
		Description: strings.TrimSpace(row[2]),
		Quantity:    quantity,
		InvoiceDate: invoiceDate,
		Price:       price,
		CustomerID:  customerID,
		Country:     strings.TrimSpace(row[7]),
	}, nil
}
