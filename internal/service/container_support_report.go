package service

import (
	"context"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"
	"vornik.io/vornik/internal/api"
)

// support-report collector wiring
// ===============================
//
// Builds the optional collector adapters the support-report endpoint
// embeds (doctor diagnosis, /healthz-style snapshot, metrics text) and
// hands them — plus the judge-verdict + post-mortem repos — to the API
// server via api.WithSupportReportCollectors. Every adapter is nil-safe:
// a deployment missing the DB, the metrics registry, or the doctor
// handler simply omits that bundle section (best-effort, §7).

// wireSupportReportCollectors builds the optional collector adapters and
// hands them to the API server. Called after the apiServer + doctor
// handler exist (the doctor + health adapters depend on both). Nil-safe.
func (c *Container) wireSupportReportCollectors(srv *api.Server, doctor *api.DoctorHandlers) {
	if srv == nil {
		return
	}
	var dr api.SupportDoctorRunner
	if doctor != nil {
		dr = &supportDoctorAdapter{h: doctor}
	}

	var health api.SupportHealthSource = &supportHealthAdapter{srv: srv}

	var metrics api.SupportMetricsSource
	if reg := c.observabilityRegistry(); reg != nil {
		metrics = &supportMetricsAdapter{gatherer: reg}
	}

	var judge api.SupportJudgeReader
	var pm api.SupportPostMortemReader
	if c.repos != nil {
		if c.repos.JudgeVerdicts != nil {
			judge = c.repos.JudgeVerdicts
		}
		if c.repos.PostMortems != nil {
			pm = c.repos.PostMortems
		}
	}

	srv.SetSupportReportCollectors(dr, health, metrics, judge, pm)
}

// supportDoctorAdapter runs the read-only doctor checks for the bundle.
type supportDoctorAdapter struct {
	h *api.DoctorHandlers
}

func (a *supportDoctorAdapter) Run(ctx context.Context) (any, error) {
	return a.h.RunReportReadOnly(ctx), nil
}

// supportHealthAdapter snapshots the in-process readiness results — the
// same name/status/error triples /readyz emits.
type supportHealthAdapter struct {
	srv *api.Server
}

func (a *supportHealthAdapter) Snapshot(ctx context.Context) (any, error) {
	return a.srv.RunReadiness(ctx), nil
}

// supportMetricsAdapter renders the Prometheus registry to the text
// exposition format used by /metrics.
type supportMetricsAdapter struct {
	gatherer prometheus.Gatherer
}

func (a *supportMetricsAdapter) Snapshot(_ context.Context) (string, error) {
	mfs, err := a.gatherer.Gather()
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	enc := expfmt.NewEncoder(&sb, expfmt.FmtText)
	for _, mf := range mfs {
		if err := enc.Encode(mf); err != nil {
			return sb.String(), err
		}
	}
	return sb.String(), nil
}
