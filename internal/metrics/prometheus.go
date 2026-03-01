package metrics

import (
	"context"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// DemoMetrics 自定义 Prometheus 指标，补充 loadflow 框架内置指标
type DemoMetrics struct {
	// 路由权重（核心：观测 rebalance 过程）
	RouterWeight *prometheus.GaugeVec

	// 流入消息计数
	StreamMessagesIn *prometheus.CounterVec

	// 路由分发计数（stream→pool）
	RouterRouted *prometheus.CounterVec

	// 重均衡事件计数
	RebalancePlan *prometheus.CounterVec

	// 重均衡步长
	RebalanceStep *prometheus.GaugeVec

	// 粘性路由违规计数（Stream 2 key 迁移）
	StickyViolations *prometheus.CounterVec

	// Handler 处理计数
	HandlerProcessed *prometheus.CounterVec

	// Handler 错误计数
	HandlerErrors *prometheus.CounterVec

	// 订单状态机指标（Stream 3）
	OrdersCreated   prometheus.Counter
	OrdersCompleted prometheus.Counter
	OrdersFailed    prometheus.Counter

	// MySQL 写入延迟
	DBLatency *prometheus.HistogramVec
}

// NewDemoMetrics 创建并注册所有自定义指标
func NewDemoMetrics(reg prometheus.Registerer) *DemoMetrics {
	m := &DemoMetrics{
		RouterWeight: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: "loadflow",
				Subsystem: "router",
				Name:      "weight",
				Help:      "Current routing weight for each stream-pool pair",
			},
			[]string{"stream", "pool"},
		),
		StreamMessagesIn: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "loadflow",
				Subsystem: "stream",
				Name:      "messages_in_total",
				Help:      "Total messages sent into each stream",
			},
			[]string{"stream"},
		),
		RouterRouted: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "loadflow",
				Subsystem: "router",
				Name:      "routed_total",
				Help:      "Total messages routed from stream to pool",
			},
			[]string{"stream", "pool"},
		),
		RebalancePlan: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "loadflow",
				Subsystem: "rebalance",
				Name:      "plan_total",
				Help:      "Count of rebalancing plans by result",
			},
			[]string{"stream", "result"},
		),
		RebalanceStep: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: "loadflow",
				Subsystem: "rebalance",
				Name:      "step_size",
				Help:      "Size of the last weight adjustment step",
			},
			[]string{"stream"},
		),
		StickyViolations: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "loadflow",
				Subsystem: "routing",
				Name:      "violations_total",
				Help:      "Sticky routing violations (key migrated to different pool)",
			},
			[]string{"stream"},
		),
		HandlerProcessed: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "loadflow",
				Subsystem: "handler",
				Name:      "processed_total",
				Help:      "Total messages processed by handler",
			},
			[]string{"stream", "pool"},
		),
		HandlerErrors: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "loadflow",
				Subsystem: "handler",
				Name:      "errors_total",
				Help:      "Total handler errors",
			},
			[]string{"stream", "pool"},
		),
		OrdersCreated: prometheus.NewCounter(
			prometheus.CounterOpts{
				Namespace: "loadflow",
				Subsystem: "orders",
				Name:      "created_total",
				Help:      "Total orders created (Stream 3)",
			},
		),
		OrdersCompleted: prometheus.NewCounter(
			prometheus.CounterOpts{
				Namespace: "loadflow",
				Subsystem: "orders",
				Name:      "completed_total",
				Help:      "Total orders completed (Stream 3)",
			},
		),
		OrdersFailed: prometheus.NewCounter(
			prometheus.CounterOpts{
				Namespace: "loadflow",
				Subsystem: "orders",
				Name:      "failed_total",
				Help:      "Total orders failed (Stream 3)",
			},
		),
		DBLatency: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace: "loadflow",
				Subsystem: "db",
				Name:      "write_duration_seconds",
				Help:      "MySQL write latency distribution",
				Buckets:   []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0},
			},
			[]string{"stream", "pool"},
		),
	}

	reg.MustRegister(
		m.RouterWeight,
		m.StreamMessagesIn,
		m.RouterRouted,
		m.RebalancePlan,
		m.RebalanceStep,
		m.StickyViolations,
		m.HandlerProcessed,
		m.HandlerErrors,
		m.OrdersCreated,
		m.OrdersCompleted,
		m.OrdersFailed,
		m.DBLatency,
	)

	return m
}

// KeyTracker 追踪 key 的路由映射，用于检测粘性路由违规
type KeyTracker struct {
	mu      sync.RWMutex
	mapping map[string]map[string]string // stream -> key -> poolName
	metrics *DemoMetrics
}

func NewKeyTracker(metrics *DemoMetrics) *KeyTracker {
	return &KeyTracker{
		mapping: make(map[string]map[string]string),
		metrics: metrics,
	}
}

// Track 记录一个 key 被路由到哪个 pool，如果发生变化则计入 violation
func (kt *KeyTracker) Track(stream, key, pool string) bool {
	kt.mu.Lock()
	defer kt.mu.Unlock()

	if kt.mapping[stream] == nil {
		kt.mapping[stream] = make(map[string]string)
	}

	prevPool, exists := kt.mapping[stream][key]
	kt.mapping[stream][key] = pool

	if exists && prevPool != pool {
		// 粘性违规：key 迁移到了不同的 pool
		kt.metrics.StickyViolations.WithLabelValues(stream).Inc()
		return true // violation
	}
	return false
}

// WeightReporter 周期性上报路由权重到 Prometheus
type WeightReporter struct {
	metrics *DemoMetrics
	mu      sync.RWMutex
	weights map[string]map[string]float64 // stream -> pool -> weight
}

func NewWeightReporter(metrics *DemoMetrics) *WeightReporter {
	return &WeightReporter{
		metrics: metrics,
		weights: make(map[string]map[string]float64),
	}
}

func (wr *WeightReporter) Update(stream string, pools []string, weights []int) {
	wr.mu.Lock()
	defer wr.mu.Unlock()

	if wr.weights[stream] == nil {
		wr.weights[stream] = make(map[string]float64)
	}
	for i, p := range pools {
		if i < len(weights) {
			wr.weights[stream][p] = float64(weights[i])
		}
	}
}

func (wr *WeightReporter) Start(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			wr.mu.RLock()
			for stream, pools := range wr.weights {
				for pool, w := range pools {
					wr.metrics.RouterWeight.WithLabelValues(stream, pool).Set(w)
				}
			}
			wr.mu.RUnlock()
		case <-ctx.Done():
			return
		}
	}
}
