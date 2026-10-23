package runner

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

const (
	labelRunner    = "runner"
	labelName      = "name"
	labelNamespace = "namespace"
)

// Metrics of ForgejoRunners. Labels: namespace, name (the Forgejo instance, as on every
// operator metric, so that the instance's PrometheusRule selects them) and runner (the
// ForgejoRunner).
var (
	RunnersDesired = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "forgejo_operator_runners_desired",
		Help: "spec.replicas of a ForgejoRunner.",
	}, []string{labelNamespace, labelName, labelRunner})

	RunnersRegistered = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "forgejo_operator_runners_registered",
		Help: "Runners of a ForgejoRunner that are registered with Forgejo.",
	}, []string{labelNamespace, labelName, labelRunner})

	RunnersOnline = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "forgejo_operator_runners_online",
		Help: "Runners of a ForgejoRunner that Forgejo reports online (idle or active). Absent while Forgejo's API cannot be read.",
	}, []string{labelNamespace, labelName, labelRunner})

	RunnerJobsWaiting = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "forgejo_operator_runner_jobs_waiting",
		Help: "Jobs in the scope of a ForgejoRunner that wait for a runner with its labels. Absent while Forgejo's API cannot be read.",
	}, []string{labelNamespace, labelName, labelRunner})
)

func init() {
	metrics.Registry.MustRegister(RunnersDesired, RunnersRegistered, RunnersOnline, RunnerJobsWaiting)
}

// ForgetMetrics drops the series of a deleted ForgejoRunner.
func ForgetMetrics(namespace, runner string) {
	l := prometheus.Labels{labelNamespace: namespace, labelRunner: runner}
	for _, v := range []*prometheus.GaugeVec{RunnersDesired, RunnersRegistered, RunnersOnline, RunnerJobsWaiting} {
		v.DeletePartialMatch(l)
	}
}

// ForgetAPIMetrics drops the series that come from Forgejo's API, while it cannot be read.
func ForgetAPIMetrics(namespace, runner string) {
	l := prometheus.Labels{labelNamespace: namespace, labelRunner: runner}
	RunnersOnline.DeletePartialMatch(l)
	RunnerJobsWaiting.DeletePartialMatch(l)
}
