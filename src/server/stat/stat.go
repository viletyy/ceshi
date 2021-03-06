package stat

import (
	"github.com/prometheus/client_golang/prometheus"
)

const (
	namespace = "n9e"
	subsystem = "server"
)

var (
	// 各个周期性任务的执行耗时
	GaugeCronDuration = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Subsystem: subsystem,
		Name:      "cron_duration",
		Help:      "Cron method use duration, unit: ms.",
	}, []string{"cluster", "name"})

	// 从数据库同步数据的时候，同步的条数
	GaugeSyncNumber = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Subsystem: subsystem,
		Name:      "cron_sync_number",
		Help:      "Cron sync number.",
	}, []string{"cluster", "name"})

	// 从各个接收接口接收到的监控数据总量
	CounterSampleTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: subsystem,
		Name:      "samples_received_total",
		Help:      "Total number samples received.",
	}, []string{"cluster", "channel"})

	// 产生的告警总量
	CounterAlertsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: subsystem,
		Name:      "alerts_total",
		Help:      "Total number alert events.",
	}, []string{"cluster"})

	// 内存中的告警事件队列的长度
	GaugeAlertQueueSize = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Subsystem: subsystem,
		Name:      "alert_queue_size",
		Help:      "The size of alert queue.",
	}, []string{"cluster"})
)

func Init() {
	// Register the summary and the histogram with Prometheus's default registry.
	prometheus.MustRegister(
		GaugeCronDuration,
		GaugeSyncNumber,
		CounterSampleTotal,
		CounterAlertsTotal,
		GaugeAlertQueueSize,
	)
}
