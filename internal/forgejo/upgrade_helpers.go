package forgejo

import forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"

// UpgradeHoldsWorkload reports whether a failed upgrade left its new image deployed without
// it ever becoming available: its bootstrap init container may still be running (or retrying)
// "forgejo migrate", which takes no lock. Until that image is verified the workload must stay
// as during the upgrade (one replica, Recreate strategy, no HPA, no restarts by self-healing,
// config rollback or vertical scaling), so that the migration never runs in two pods at once
// and is not interrupted. The upgrade step updates status.upgrade before the later steps
// call this, and it sets rc.ReplicasOverride to 1 itself.
func UpgradeHoldsWorkload(f *forgejov1.Forgejo) bool {
	u := f.Status.Upgrade
	return u != nil && u.Phase == forgejov1.UpgradePhaseFailed && u.RolloutStartTime != nil && u.RestoreName == "" &&
		u.FailedImage != "" && u.FailedImage == u.ToImage && f.Status.Image != u.ToImage
}
