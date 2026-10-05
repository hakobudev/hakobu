package ops

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/x0ryz/hakobu/internal/node"
	"github.com/x0ryz/hakobu/internal/store"
)

// Uploading a file into a volume, an SQLite database moving in, say: the
// volume is backed up first, then the file goes to the project's backup
// bucket sealed, like a backup, and the app's server takes it from there
// into the volume with the app stopped, as it restores a backup. The
// sealed copy is deleted once it's in.

var validVolumeFile = regexp.MustCompile(`^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)*$`)

// checkVolumeFile is a file's path inside a volume, relative, without . or
// .. in it.
func checkVolumeFile(name string) (string, error) {
	name = strings.Trim(strings.TrimSpace(name), "/")
	ok := validVolumeFile.MatchString(name)
	for _, part := range strings.Split(name, "/") {
		ok = ok && part != "." && part != ".."
	}
	if !ok {
		return "", fmt.Errorf("a file's path in the volume is like app.db or db/app.db: letters, digits, dots, dashes and underscores")
	}
	return name, nil
}

// StartVolumeUpload writes the file at src (removed when done) into the
// app's volume at name, in the background.
func StartVolumeUpload(s *store.Store, app, volume, name, src string) error {
	name, err := checkVolumeFile(name)
	if err == nil {
		err = hasVolume(s, app, volume)
	}
	var a store.App
	if err == nil {
		a, err = s.GetApp(ctx(), app)
	}
	var target backupTarget
	if err == nil {
		target, err = projectBackupTarget(s, a.ProjectID)
	}
	if err != nil {
		os.Remove(src)
		return err
	}
	job := volumeJob(app, volume)
	what := "uploading " + name
	if err := reserveDB(job, what); err != nil {
		os.Remove(src)
		return err
	}
	err = startJob(s, app, "upload to volume "+volume, func(a store.App, out io.Writer) error {
		err := putVolumeFile(s, a, volume, name, src, target, out)
		finishDB(job, what, err)
		return err
	})
	if err != nil {
		os.Remove(src)
		finishDB(job, what, err)
	}
	return err
}

func putVolumeFile(s *store.Store, app store.App, volume, name, src string, target backupTarget, out io.Writer) error {
	defer os.Remove(src)
	fmt.Fprintln(out, "backing up volume", volume, "first")
	if id, err := backupVolumeHeld(s, app, volume); err != nil {
		return fmt.Errorf("the volume wasn't backed up, so nothing was changed: %w", err)
	} else if id == 0 {
		fmt.Fprintln(out, "the volume is empty yet: nothing to back up")
	}
	key := fmt.Sprintf("uploads/%s_%s/%s.enc", app.Name, volume, time.Now().UTC().Format("20060102-150405"))
	up, err := newUpload(s, target, key)
	if err != nil {
		return err
	}
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	fmt.Fprintln(out, "sending", name, "to the app's server")
	obj, err := node.UploadSealed(ctx(), up, func(w io.Writer) error {
		_, err := io.Copy(w, f)
		return err
	})
	if err != nil {
		return err
	}
	defer func() {
		if err := deleteParts(s, target, key, int64(obj.Parts)); err != nil {
			fmt.Println("the uploaded copy of", name, "stays in the bucket at", key+":", err)
		}
	}()
	dl, err := backupDownload(s, target, key, int64(obj.Parts), obj.SHA256, up.FileKey)
	if err != nil {
		return err
	}
	defer restartWorker(s, app, out)
	return AppNode(s, app).PutVolumeFile(ctx(), liveSpec(app), volume, name, dl, out)
}
