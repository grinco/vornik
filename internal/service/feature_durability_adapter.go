package service

import (
	"context"

	"vornik.io/vornik/internal/featuredoctor"
	"vornik.io/vornik/internal/storage"
)

// storageDurabilityProber adapts storage.Backend.ProbeDurability to
// featuredoctor.DurabilityProber (the config-assistant feature's R6
// prerequisite). storage stays a leaf; the doctor's report type is
// mirrored here.
type storageDurabilityProber struct{ backend *storage.Backend }

func (p storageDurabilityProber) ProbeDurability(ctx context.Context) featuredoctor.DurabilityReport {
	rep := p.backend.ProbeDurability(ctx)
	return featuredoctor.DurabilityReport{Driver: rep.Driver, OK: rep.OK, Detail: rep.Detail}
}
