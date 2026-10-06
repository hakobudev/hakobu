package ops

import (
	"fmt"
	"path"
	"strings"

	"github.com/x0ryz/hakobu/internal/node"
	"github.com/x0ryz/hakobu/internal/store"
)

// appBinds mounts the app's volumes; the app and its worker share them.
func appBinds(s *store.Store, app store.App) ([]string, error) {
	vols, err := s.ListVolumes(ctx(), app.ID)
	if err != nil {
		return nil, err
	}
	binds := make([]string, len(vols))
	for i, v := range vols {
		binds[i] = node.Volume(app.Key(), v.Name) + ":" + v.MountPath
	}
	return binds, nil
}

// AddVolume attaches a new volume; it's mounted from the next deploy on.
func AddVolume(s *store.Store, appID int64, name, mountPath string) error {
	if err := checkName("volume", name); err != nil {
		return err
	}
	mountPath = path.Clean(strings.TrimSpace(mountPath))
	if !path.IsAbs(mountPath) || mountPath == "/" || strings.Contains(mountPath, ":") {
		return fmt.Errorf("mount path must be an absolute directory other than /, e.g. /app/data")
	}
	vols, err := s.ListVolumes(ctx(), appID)
	if err != nil {
		return err
	}
	for _, v := range vols {
		if v.Name == name {
			return fmt.Errorf("the app already has a volume named %s", name)
		}
		if v.MountPath == mountPath {
			return fmt.Errorf("volume %s is already mounted at %s", v.Name, mountPath)
		}
	}
	return s.AddVolume(ctx(), store.AddVolumeParams{AppID: appID, Name: name, MountPath: mountPath})
}

// RemoveVolume detaches a volume and deletes its data. A volume the running
// container still uses is deleted by the daily cleanup after the next deploy.
func RemoveVolume(s *store.Store, appID int64, name string) error {
	a, err := s.GetAppByID(ctx(), appID)
	if err != nil {
		return err
	}
	if err := s.DeleteVolume(ctx(), store.DeleteVolumeParams{AppID: appID, Name: name}); err != nil {
		return err
	}
	// The files stay in the bucket, as a deleted database's do.
	if err := s.DeleteVolumeBackupsOf(ctx(), store.DeleteVolumeBackupsOfParams{AppID: appID, Volume: name}); err != nil {
		return err
	}
	return AppNode(s, a).RemoveVolume(ctx(), a.Key(), name)
}

func SetShareVolumes(s *store.Store, appID int64, share bool) error {
	var v int64
	if share {
		v = 1
	}
	return s.SetAppShareVolumes(ctx(), store.SetAppShareVolumesParams{ID: appID, ShareVolumes: v})
}
