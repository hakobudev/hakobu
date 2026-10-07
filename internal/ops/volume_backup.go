package ops

import (
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/hakobudev/hakobu/internal/config"
	"github.com/hakobudev/hakobu/internal/node"
	"github.com/hakobudev/hakobu/internal/panellog"
	"github.com/hakobudev/hakobu/internal/store"
)

// Volumes are backed up daily like databases: a tar of the volume, sealed,
// to the backup bucket, read back right after to prove it's whole. The
// containers using the volume are paused while tar reads it, so files
// written together (an SQLite database and its journal) are copied as of
// one moment, as after a power cut; an app is frozen for as long as that
// takes, seconds for most volumes.

// volumeJob names a volume's backup jobs in dbJobs (DatabaseJob), and its
// backup alerts.
func volumeJob(appID int64, volume string) string { return fmt.Sprintf("vol:%d/%s", appID, volume) }

// VolumeJob is the running or last backup job of an app's volume.
func VolumeJob(appID int64, volume string) DBJob { return jobOf(volumeJob(appID, volume)) }

// errAppBusy is a volume backup put off because the app is being deployed.
var errAppBusy = errors.New("a deploy is in progress")

// BackupVolume uploads a backup of the volume; it returns errAppBusy while
// the app is deploying, and 0 for a volume no deploy has created yet.
func BackupVolume(s *store.Store, appID int64, volume string) (int64, error) {
	a, err := s.GetAppByID(ctx(), appID)
	if err != nil {
		return 0, err
	}
	// Held like a deploy while the app is paused and its archive goes up,
	// so no deploy starts or stops its containers meanwhile; a push is
	// deployed after it.
	if ok, _ := reserve(appID, false); !ok {
		return 0, errAppBusy
	}
	id, err := backupVolumeHeld(s, a, volume)
	if release(appID) {
		if err := StartDeploy(s, appID, "push"); err != nil {
			panellog.Error("deploying", a.Name, "after its backup failed:", err)
		}
	}
	return id, err
}

// backupVolumeHeld is BackupVolume for a caller holding the app.
func backupVolumeHeld(s *store.Store, a store.App, volume string) (int64, error) {
	n := AppNode(s, a)
	if ok, err := n.HasVolume(ctx(), a.Key(), volume); err != nil || !ok {
		return 0, err
	}
	target, err := projectBackupTarget(s, a.ProjectID)
	if err != nil {
		return 0, err
	}
	key := fmt.Sprintf("volumes/%s_%s/%s.tar.enc", a.Key(), volume, time.Now().UTC().Format("20060102-150405"))
	up, err := newUpload(s, target, key)
	if err != nil {
		return 0, err
	}
	obj, err := n.BackupVolume(ctx(), a.Key(), volume, up)
	if err != nil {
		return 0, err
	}
	return s.CreateVolumeBackup(ctx(), store.CreateVolumeBackupParams{
		AppID: a.ID, Volume: volume, ObjectKey: key, Parts: int64(obj.Parts), SizeBytes: obj.Size, SHA256: obj.SHA256, KeyFingerprint: obj.KeyFingerprint,
		AccountID: target.AccountID, Bucket: target.Bucket,
	})
}

// volumeBackupDownload is backupDownload for a volume backup.
func volumeBackupDownload(s *store.Store, b store.VolumeBackup) (node.Download, error) {
	return backupDownload(s, backupTarget{b.AccountID, b.Bucket}, b.ObjectKey, b.Parts, b.SHA256, b.KeyFingerprint)
}

// VerifyVolumeBackup downloads a backup and reads the whole tar in it; the
// result is saved on the backup.
func VerifyVolumeBackup(s *store.Store, id int64) error {
	b, err := s.GetVolumeBackup(ctx(), id)
	if err != nil {
		return err
	}
	files, verifyErr := countTarFiles(s, b)
	msg := ""
	if verifyErr != nil {
		msg = verifyErr.Error()
	}
	if err := s.SetVolumeBackupVerified(ctx(), store.SetVolumeBackupVerifiedParams{
		ID: id, VerifiedAt: time.Now().UTC().Format("2006-01-02T15:04:05Z"), VerifyError: msg, Files: int64(files),
	}); err != nil {
		return err
	}
	return verifyErr
}

func countTarFiles(s *store.Store, b store.VolumeBackup) (int, error) {
	app, err := s.GetAppByID(ctx(), b.AppID)
	if err != nil {
		return 0, err
	}
	dl, err := volumeBackupDownload(s, b)
	if err != nil {
		return 0, err
	}
	return AppNode(s, app).VerifyVolumeBackup(ctx(), dl)
}

// volumeBackupAndCheck backs a volume up, reads the backup back and drops
// old ones.
func volumeBackupAndCheck(s *store.Store, appID int64, volume string) error {
	id, err := BackupVolume(s, appID, volume)
	if err != nil || id == 0 {
		return err
	}
	verifyErr := VerifyVolumeBackup(s, id)
	if err := RotateVolumeBackups(s, appID, volume, time.Now()); err != nil {
		panellog.Error("backup rotation of", volumeJob(appID, volume), "failed:", err)
	}
	if verifyErr != nil {
		return fmt.Errorf("the backup was uploaded, but reading it back failed: %w", verifyErr)
	}
	return nil
}

// StartVolumeBackup backs the volume up in the background.
func StartVolumeBackup(s *store.Store, appID int64, volume string) error {
	if err := hasVolume(s, appID, volume); err != nil {
		return err
	}
	return startDBJob(volumeJob(appID, volume), "backing up", func() error {
		err := volumeBackupAndCheck(s, appID, volume)
		if errors.Is(err, errAppBusy) {
			return fmt.Errorf("%w, try again when it finishes", err)
		}
		return err
	})
}

// StartVolumeVerify reads a backup back in the background.
func StartVolumeVerify(s *store.Store, appID int64, volume string, id int64) error {
	b, err := volumeBackup(s, appID, volume, id)
	if err != nil {
		return err
	}
	return startDBJob(volumeJob(appID, volume), "checking a backup", func() error { return VerifyVolumeBackup(s, b.ID) })
}

// StartVolumeRestore replaces the volume's contents with a backup, as a
// job in the app's deploy history: the app and its worker are stopped
// while it's written and started again after.
func StartVolumeRestore(s *store.Store, appID int64, volume string, id int64) error {
	b, err := volumeBackup(s, appID, volume, id)
	if err != nil {
		return err
	}
	job := volumeJob(appID, volume)
	what := "restoring the backup of " + b.CreatedAt
	if err := reserveDB(job, what); err != nil {
		return err
	}
	err = startJob(s, appID, "restore volume "+volume, func(a store.App, out io.Writer) error {
		err := restoreVolume(s, a, b, out)
		finishDB(job, what, err)
		return err
	})
	if err != nil {
		finishDB(job, what, err)
	}
	return err
}

func restoreVolume(s *store.Store, app store.App, b store.VolumeBackup, out io.Writer) error {
	fmt.Fprintln(out, "restoring the backup of", b.CreatedAt)
	dl, err := volumeBackupDownload(s, b)
	if err != nil {
		return err
	}
	defer restartWorker(s, app, out)
	return AppNode(s, app).RestoreVolume(ctx(), liveSpec(app), b.Volume, dl, out)
}

func hasVolume(s *store.Store, appID int64, volume string) error {
	vols, err := s.ListVolumes(ctx(), appID)
	if err != nil {
		return err
	}
	for _, v := range vols {
		if v.Name == volume {
			return nil
		}
	}
	return fmt.Errorf("the app has no volume %s", volume)
}

// volumeBackup looks up a backup and makes sure it's of the app's volume.
func volumeBackup(s *store.Store, appID int64, volume string, id int64) (store.VolumeBackup, error) {
	b, err := s.GetVolumeBackup(ctx(), id)
	if err != nil || b.AppID != appID || b.Volume != volume {
		return b, fmt.Errorf("no such backup of volume %s", volume)
	}
	return b, hasVolume(s, appID, volume)
}

func volumeBackupAge(b store.VolumeBackup) (int64, string, bool) {
	return b.ID, b.CreatedAt, b.VerifiedAt != "" && b.VerifyError == ""
}

// RotateVolumeBackups deletes the volume's backups the rotation no longer
// keeps, the same as a database's.
func RotateVolumeBackups(s *store.Store, appID int64, volume string, now time.Time) error {
	all, err := s.ListAllVolumeBackups(ctx(), store.ListAllVolumeBackupsParams{AppID: appID, Volume: volume})
	if err != nil {
		return err
	}
	for _, b := range backupsToDrop(all, volumeBackupAge, config.BackupKeep, now) {
		if err := deleteParts(s, backupTarget{b.AccountID, b.Bucket}, b.ObjectKey, b.Parts); err != nil {
			return err // retried by the next rotation
		}
		if err := s.DeleteVolumeBackup(ctx(), b.ID); err != nil {
			return err
		}
	}
	return nil
}

// VolumeBackupDue backs up every volume whose last backup is older than
// config.BackupEvery, keyed by volumeJob like NoteBackup wants them. A
// volume of an app being deployed waits for the next hour.
func VolumeBackupDue(s *store.Store) map[string]error {
	results := map[string]error{}
	vols, err := s.ListAllVolumes(ctx())
	if err != nil || BackupBucket(s) == "" {
		return results
	}
	for _, v := range vols {
		last, err := s.ListVolumeBackups(ctx(), store.ListVolumeBackupsParams{AppID: v.AppID, Volume: v.Name, Limit: 1})
		if err == nil && len(last) > 0 {
			if t, err := time.Parse("2006-01-02T15:04:05Z", last[0].CreatedAt); err == nil && time.Since(t) < config.BackupEvery {
				continue
			}
		}
		err = runDBJob(volumeJob(v.AppID, v.Name), "backing up (scheduled)", func() error { return volumeBackupAndCheck(s, v.AppID, v.Name) })
		if !errors.Is(err, errAppBusy) {
			results[volumeJob(v.AppID, v.Name)] = err
		}
	}
	return results
}
