package ops

import (
	"fmt"
	"path"
	"strings"

	"github.com/x0ryz/hakobu/internal/node"
	"github.com/x0ryz/hakobu/internal/store"
)

// appBinds mounts the app's volumes; the app and its worker share them.
func appBinds(s *store.Store, app string) ([]string, error) {
	vols, err := s.ListVolumes(ctx(), app)
	if err != nil {
		return nil, err
	}
	binds := make([]string, len(vols))
	for i, v := range vols {
		binds[i] = node.Volume(app, v.Name) + ":" + v.MountPath
	}
	return binds, nil
}

// AddVolume attaches a new volume; it's mounted from the next deploy on.
func AddVolume(s *store.Store, app, name, mountPath string) error {
	if err := checkName("volume", name); err != nil {
		return err
	}
	mountPath = path.Clean(strings.TrimSpace(mountPath))
	if !path.IsAbs(mountPath) || mountPath == "/" || strings.Contains(mountPath, ":") {
		return fmt.Errorf("mount path must be an absolute directory other than /, e.g. /app/data")
	}
	vols, err := s.ListVolumes(ctx(), app)
	if err != nil {
		return err
	}
	for _, v := range vols {
		if v.Name == name {
			return fmt.Errorf("%s already has a volume named %s", app, name)
		}
		if v.MountPath == mountPath {
			return fmt.Errorf("volume %s is already mounted at %s", v.Name, mountPath)
		}
	}
	return s.AddVolume(ctx(), store.AddVolumeParams{AppName: app, Name: name, MountPath: mountPath})
}

// RemoveVolume detaches a volume and deletes its data. A volume the running
// container still uses is deleted by the daily cleanup after the next deploy.
func RemoveVolume(s *store.Store, app, name string) error {
	if err := s.DeleteVolume(ctx(), store.DeleteVolumeParams{AppName: app, Name: name}); err != nil {
		return err
	}
	// The files stay in the bucket, as a deleted database's do.
	if err := s.DeleteVolumeBackupsOf(ctx(), store.DeleteVolumeBackupsOfParams{AppName: app, Volume: name}); err != nil {
		return err
	}
	a, err := s.GetApp(ctx(), app)
	if err != nil {
		return err
	}
	return AppNode(s, a).RemoveVolume(ctx(), app, name)
}

func SetShareVolumes(s *store.Store, app string, share bool) error {
	var v int64
	if share {
		v = 1
	}
	return s.SetAppShareVolumes(ctx(), store.SetAppShareVolumesParams{Name: app, ShareVolumes: v})
}

// removeOrphanVolumes deletes app volumes no app refers to any more;
// volumes still attached to a container are skipped.
func removeOrphanVolumes(s *store.Store) error {
	vols, err := s.ListAllVolumes(ctx())
	if err != nil {
		return err
	}
	known := map[string]bool{}
	for _, v := range vols {
		known[node.Volume(v.AppName, v.Name)] = true
	}
	return local.RemoveVolumesExcept(ctx(), known)
}
