package forgejo

import forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"

// UpgradeInFlight reports whether status.upgrade records an upgrade that has not finished,
// i.e. a phase in which the upgrade step sets rc.Upgrading. The orchestrator uses it for
// rc.Upgrading when the upgrade step could not run in a reconcile (an earlier step it
// depends on failed or stopped) but later independent steps still run.
func UpgradeInFlight(f *forgejov1.Forgejo) bool {
	u := f.Status.Upgrade
	if u == nil {
		return false
	}
	switch u.Phase {
	case forgejov1.UpgradePhasePending, forgejov1.UpgradePhaseBackingUp, forgejov1.UpgradePhaseRollingOut,
		forgejov1.UpgradePhaseVerifying, forgejov1.UpgradePhaseRollingBack:
		return true
	}
	return false
}
