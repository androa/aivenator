package metrics

import (
	"strconv"
	"time"

	"github.com/aiven/aiven-go-client"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	Namespace = "aivenator"

	LabelAivenOperation = "operation"
	LabelNamespace      = "namespace"
	LabelPool           = "pool"
	LabelResourceType   = "resource_type"
	LabelStatus         = "status"
	LabelSyncState      = "synchronization_state"
	LabelSecretState    = "state"
)

var (
	ApplicationsProcessed = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:      "aiven_applications_processed",
		Namespace: Namespace,
		Help:      "number of applications synchronized with aiven",
	}, []string{LabelSyncState})

	ApplicationProcessingTime = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:      "aiven_application_processing_time_seconds",
		Namespace: Namespace,
		Help:      "seconds from observed to synchronised successfully",
		Buckets:   prometheus.ExponentialBuckets(0.1, 1.4, 20),
	}, []string{LabelSyncState})

	ServiceUsersCreated = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:      "service_users_created",
		Namespace: Namespace,
		Help:      "number of service users created",
	}, []string{LabelPool})

	ServiceUsersDeleted = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:      "service_users_deleted",
		Namespace: Namespace,
		Help:      "number of service users deleted",
	}, []string{LabelPool})

	AivenLatency = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:      "aiven_latency",
		Namespace: Namespace,
		Help:      "latency in aiven api operations",
		Buckets:   prometheus.ExponentialBuckets(0.025, 1.42, 20),
	}, []string{LabelAivenOperation, LabelStatus, LabelPool})

	KubernetesResourcesWritten = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:      "kubernetes_resources_written",
		Namespace: Namespace,
		Help:      "number of kubernetes resources written to the cluster",
	}, []string{LabelNamespace, LabelResourceType})

	KubernetesResourcesDeleted = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:      "kubernetes_resources_deleted",
		Namespace: Namespace,
		Help:      "number of kubernetes resources deleted from the cluster",
	}, []string{LabelNamespace, LabelResourceType})

	SecretsManaged = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name:      "secrets_managed",
		Namespace: Namespace,
		Help:      "number of secrets managed",
	}, []string{LabelNamespace, LabelSecretState})
)

func ObserveAivenLatency(operation, pool string, fun func() error) error {
	timer := time.Now()
	err := fun()
	used := time.Now().Sub(timer)
	status := 200
	if err != nil {
		aivenErr, ok := err.(aiven.Error)
		if ok {
			status = aivenErr.Status
		} else {
			status = 0
		}
	}
	AivenLatency.With(prometheus.Labels{
		LabelAivenOperation: operation,
		LabelPool:           pool,
		LabelStatus:         strconv.Itoa(status),
	}).Observe(used.Seconds())
	return err
}

func Register(registry prometheus.Registerer) {
	registry.MustRegister(
		AivenLatency,
		KubernetesResourcesWritten,
		KubernetesResourcesDeleted,
		ServiceUsersCreated,
		ServiceUsersDeleted,
		ApplicationsProcessed,
		SecretsManaged,
	)
}
