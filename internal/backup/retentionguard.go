package backup

import (
	"fmt"
	"slices"
	"strings"
	"time"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
)

// AnnotationBackupSizeAccepted on the Forgejo names a backup whose sizes a person accepts:
// the size-drop guard of retention then compares the newest scheduled backup only with those
// that completed no earlier than it. It ends a size-drop hold (BackupSizeDropped) after a
// deliberate cleanup.
const AnnotationBackupSizeAccepted = "forgejo.lucas.icu/backup-size-accepted"

// reasonBackupSizeDropped is the BackupHealthy reason and the event of the size-drop guard.
const reasonBackupSizeDropped = "BackupSizeDropped"

// Retention counts completed scheduled backups only, so it cannot tell good backups from
// "successful" backups of a database or volume that was silently replaced by an empty one
// (or of another instance that took the name): those would push every good backup out of
// retention, locally and remotely. Two guards keep them:
//
//   - Identity: a backup records the SECRET_KEY fingerprint of its instance (status of the
//     ForgejoBackup, manifest of the remote copy). Retention never prunes, nor counts, a backup
//     of another identity. An unknown fingerprint (a backup of an older operator) counts as the
//     instance's.
//   - Size drop: when the newest completed scheduled backup's database dump or data archive is
//     less than half the size of the largest of the other completed scheduled backups that
//     retention holds, retention prunes no completed backup and BackupHealthy is
//     False/BackupSizeDropped, until the sizes recover or the drop is accepted with
//     AnnotationBackupSizeAccepted. Sizes that are not known (0) are not compared.

// ofIdentity reports whether a backup that recorded the SECRET_KEY fingerprint have belongs to
// the instance whose fingerprint is current. A backup without one counts as the instance's;
// while the instance's own is unknown, only those do.
func ofIdentity(have, current string) bool {
	return have == "" || have == current
}

// ofInstanceIdentity returns the backups (in their order) that retention may count and prune.
func ofInstanceIdentity(backups []*forgejov1.ForgejoBackup, current string) []*forgejov1.ForgejoBackup {
	return slices.DeleteFunc(slices.Clone(backups), func(b *forgejov1.ForgejoBackup) bool {
		return !ofIdentity(b.Status.SecretKeyFingerprint, current)
	})
}

// backupSize is what the size-drop guard compares of a completed scheduled backup.
type backupSize struct {
	name string
	at   time.Time
	// database and data are the sizes of the dump and of the data archive (0: unknown, or no
	// data archive).
	database, data int64
}

// localSizes are the sizes of the completed backups of scheduled (newest first by completion).
func localSizes(scheduled []*forgejov1.ForgejoBackup) []backupSize {
	var out []backupSize
	for _, b := range scheduled {
		if b.Status.Phase != forgejov1.BackupPhaseCompleted {
			continue
		}
		s := backupSize{name: b.Name, at: finishedAt(b), database: b.Status.DatabaseSizeBytes}
		if b.Status.IncludesData {
			s.data = b.Status.DataSizeBytes
		}
		out = append(out, s)
	}
	slices.SortStableFunc(out, func(a, b backupSize) int { return b.at.Compare(a.at) })
	return out
}

// remoteSize is what the manifest of a remote backup records of its sizes.
func remoteSize(m *RemoteManifest, at time.Time) backupSize {
	s := backupSize{name: m.Backup, at: at}
	if f := m.File(DBDumpFile); f != nil {
		s.database = f.SizeBytes
	}
	if f := m.File(DataArchiveFile); f != nil && m.IncludesData {
		s.data = f.SizeBytes
	}
	return s
}

// acceptedSince is the completion time of the backup AnnotationBackupSizeAccepted names (zero
// without the annotation or when that backup is unknown): one of sizes, else a completed
// ForgejoBackup of the instance of any reason. Once retention has pruned the accepted backup,
// it has pruned the older ones too, so a stale annotation has no effect.
func acceptedSince(f *forgejov1.Forgejo, sizes []backupSize, backups []forgejov1.ForgejoBackup) time.Time {
	name := f.Annotations[AnnotationBackupSizeAccepted]
	if name == "" {
		return time.Time{}
	}
	for _, s := range sizes {
		if s.name == name {
			return s.at
		}
	}
	for i := range backups {
		if b := &backups[i]; b.Name == name && b.Status.Phase == forgejov1.BackupPhaseCompleted {
			return finishedAt(b)
		}
	}
	return time.Time{}
}

// checkSizeDrop compares the newest of sizes (newest first) with the largest of the others that
// completed no earlier than since, part by part, and describes the parts that are less than
// half of it ("" when none is).
func checkSizeDrop(sizes []backupSize, since time.Time) string {
	if len(sizes) < 2 {
		return ""
	}
	newest := sizes[0]
	var db, data *backupSize
	for i := 1; i < len(sizes); i++ {
		s := &sizes[i]
		if s.at.Before(since) {
			continue
		}
		if db == nil || s.database > db.database {
			db = s
		}
		if data == nil || s.data > data.data {
			data = s
		}
	}
	var found []string
	if db != nil && newest.database > 0 && 2*newest.database < db.database {
		found = append(found, fmt.Sprintf("database dump %d bytes, %s had %d", newest.database, db.name, db.database))
	}
	if data != nil && newest.data > 0 && 2*newest.data < data.data {
		found = append(found, fmt.Sprintf("data archive %d bytes, %s had %d", newest.data, data.name, data.data))
	}
	return strings.Join(found, "; ")
}
