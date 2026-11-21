package provider

import (
	"errors"
	"net/http"
	"time"

	"github.com/yourorg/ztna/controller/internal/providerquery"
)

// GetMetricSeries: GET /provider/metrics/{series}?subject=&from=&to=&step=
// (Sprint 24 M1, F23, D-49). Relay and fleet relay series: relay.metrics.read
// (relay-ops, super-admin); tenant series and fleet totals of tenant data:
// tenant.metrics.read (super-admin only, D-40 extended). from and to are
// RFC 3339 (default: the last 24 hours); step is 1m or 1h (default: 1m up to
// 24 hours, 1h beyond). Missing points are gaps. Not audited (a read of
// aggregates, like the relay list).
func (h *ReadHandlers) GetMetricSeries(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actor(w, r)
	if !ok {
		return
	}
	def, known := providerquery.LookupMetricSeries(r.PathValue("series"))
	if !known {
		writeReadError(w, http.StatusNotFound, "not_found")
		return
	}
	if !h.allowed(w, h.authz.CanReadMetricSeries(actor, def.Name, def.TenantData)) {
		return
	}
	q := r.URL.Query()
	now := time.Now().UTC()
	to, from := now, now.Add(-24*time.Hour)
	if v := q.Get("to"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeReadError(w, http.StatusBadRequest, "invalid_range")
			return
		}
		to, from = t, t.Add(-24*time.Hour)
	}
	if v := q.Get("from"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeReadError(w, http.StatusBadRequest, "invalid_range")
			return
		}
		from = t
	}
	mq, err := providerquery.NewMetricQuery(def, q.Get("subject"), from, to, q.Get("step"), now)
	if err != nil {
		var qe *providerquery.MetricQueryError
		if errors.As(err, &qe) {
			writeReadError(w, http.StatusBadRequest, qe.Code)
			return
		}
		h.queryError(w, err)
		return
	}
	data, err := providerquery.ReadMetricSeries(r.Context(), h.db, mq)
	if err != nil {
		h.queryError(w, err)
		return
	}
	writeReadJSON(w, data)
}
