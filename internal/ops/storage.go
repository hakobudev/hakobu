package ops

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/x0ryz/hako/internal/backup"
	"github.com/x0ryz/hako/internal/deploy"
	"github.com/x0ryz/hako/internal/store"
)

// rustfsServiceContainer is hako's one self-hosted RustFS container
// — shared by every named Storage entry that picks "rustfs" as its
// provider (one object store, namespaced per storage entry by bucket
// name), not one container per entry.
const rustfsServiceContainer = "hako-rustfs"

// rustfsAPIPort is RustFS's S3 API port inside the container (also its
// documented default) — hako talks to it over the docker network by
// container name, so nothing needs publishing to the host.
const rustfsAPIPort = "9000"

func CreateStorage(s *store.Store, st store.Storage) error {
	if st.Provider == "rustfs" {
		return ensureRustFSService(s, st.Name)
	}
	if st.Provider != "r2" && st.Provider != "s3" {
		return fmt.Errorf("unsupported storage provider: %s (must be 'r2', 's3', or 'rustfs')", st.Provider)
	}
	if st.Region == "" {
		st.Region = "auto"
	}
	return s.CreateStorage(st)
}

func ListStorages(s *store.Store) ([]store.Storage, error) {
	return s.ListStorages()
}

// DeleteStorage refuses to delete a storage still linked to a project or
// still picked as some database's backup destination — better a clear
// error now than a project or the backup job silently losing its
// credentials.
func DeleteStorage(s *store.Store, name string) error {
	linked, err := s.ProjectsLinkedToStorage(name)
	if err != nil {
		return err
	}
	if len(linked) > 0 {
		return fmt.Errorf("storage %q is still linked to project(s) %s — unlink first", name, strings.Join(linked, ", "))
	}

	databases, err := s.ListDatabases()
	if err != nil {
		return err
	}
	var usedBy []string
	for _, db := range databases {
		if db.BackupStorage == name {
			usedBy = append(usedBy, db.Name)
		}
	}
	if len(usedBy) > 0 {
		return fmt.Errorf("storage %q is used for backups of database(s) %s — pick a different one for them first", name, strings.Join(usedBy, ", "))
	}

	return s.DeleteStorage(name)
}

// ensureRustFSService starts hako's one self-hosted RustFS
// container the first time any storage entry needs it, generating its
// access keys once and creating a bucket for this entry — a fully
// self-hosted alternative to R2/S3 for anyone who doesn't want (or can't
// reach) an external object storage account. If another storage entry
// already set up RustFS, this reuses its container/keys and just creates
// name's own bucket rather than starting a second container.
func ensureRustFSService(s *store.Store, name string) error {
	ctx := context.Background()
	bucket := "hako-" + name

	accessKey, secretKey, err := existingRustFSCredentials(s)
	if err != nil {
		accessKey, err = RandomHex(16)
		if err != nil {
			return err
		}
		secretKey, err = RandomHex(32)
		if err != nil {
			return err
		}

		env := []string{
			"RUSTFS_ACCESS_KEY=" + accessKey,
			"RUSTFS_SECRET_KEY=" + secretKey,
			"RUSTFS_ADDRESS=:" + rustfsAPIPort,
			"RUSTFS_CONSOLE_ENABLE=false",
		}
		if _, err := deploy.RunServiceContainer(ctx, rustfsServiceContainer, "rustfs/rustfs:latest", env, rustfsServiceContainer, "/data"); err != nil {
			return err
		}
	}

	// Endpoint stored on Storage is the container-name URL: that's what a
	// deployed project's app (itself running inside a container, reachable
	// via Docker's own DNS) needs. hako's agent process runs on the
	// host, though, so its own S3 calls below (and every later backup) go
	// through hostReachableStorage instead, which resolves the container's
	// live network IP — the same "host can reach a container's IP but not
	// its name" pattern already used for blue/green health checks.
	st := store.Storage{
		Name:            name,
		Provider:        "rustfs",
		Endpoint:        "http://" + rustfsServiceContainer + ":" + rustfsAPIPort,
		AccessKeyID:     accessKey,
		SecretAccessKey: secretKey,
		Bucket:          bucket,
		Region:          "auto",
	}

	hostSt, err := hostReachableStorage(ctx, st)
	if err != nil {
		return fmt.Errorf("rustfs container has no network address yet: %w", err)
	}

	if !deploy.WaitHealthy(hostSt.Endpoint, 30, time.Second, false) {
		return fmt.Errorf("rustfs service failed to become ready")
	}
	if err := backup.NewClient(hostSt).CreateBucket(); err != nil {
		return fmt.Errorf("failed to create rustfs bucket: %w", err)
	}

	return s.CreateStorage(st)
}

// existingRustFSCredentials looks for RustFS access keys already issued
// for another storage entry, so a second entry choosing "rustfs" reuses
// the one running container instead of starting a conflicting second one
// on the same container name.
func existingRustFSCredentials(s *store.Store) (accessKey, secretKey string, err error) {
	storages, err := s.ListStorages()
	if err != nil {
		return "", "", err
	}
	for _, st := range storages {
		if st.Provider == "rustfs" {
			return st.AccessKeyID, st.SecretAccessKey, nil
		}
	}
	return "", "", fmt.Errorf("no existing rustfs credentials")
}

// hostReachableStorage returns a copy of st whose Endpoint hako's own
// (host-process) S3 calls can actually reach. External providers (r2, s3)
// already store a publicly reachable endpoint, so they pass through
// unchanged; "rustfs" stores a container-name URL that only resolves
// inside hako's docker network, so this resolves the container's
// current IP instead.
func hostReachableStorage(ctx context.Context, st store.Storage) (store.Storage, error) {
	if st.Provider != "rustfs" {
		return st, nil
	}
	ip, err := deploy.ContainerIP(ctx, rustfsServiceContainer)
	if err != nil {
		return st, err
	}
	st.Endpoint = "http://" + ip + ":" + rustfsAPIPort
	return st, nil
}

// LinkStorage attaches storageName to project — takes effect on its next
// deploy, same as LinkDatabase, since env vars are baked into a container
// at start time.
func LinkStorage(s *store.Store, projectName, storageName string) error {
	if _, err := s.GetProjectByName(projectName); err != nil {
		return fmt.Errorf("project %q not found: %w", projectName, err)
	}
	if _, err := s.GetStorage(storageName); err != nil {
		return fmt.Errorf("storage %q not found: %w", storageName, err)
	}
	return s.SetProjectLinkedStorage(projectName, storageName)
}

func UnlinkStorage(s *store.Store, projectName string) error {
	if _, err := s.GetProjectByName(projectName); err != nil {
		return fmt.Errorf("project %q not found: %w", projectName, err)
	}
	return s.SetProjectLinkedStorage(projectName, "")
}

// storageEnv builds the env vars a project's linked storage gets — named
// to match what an app like kasl-crm already expects via pydantic
// AliasChoices (S3_ACCESS_KEY_ID/S3_SECRET_ACCESS_KEY/S3_BUCKET_NAME/
// S3_REGION, plus STORAGE_PROVIDER and R2_ACCOUNT_ID so the app's own
// S3_ENDPOINT_URL property can derive the R2 endpoint itself) rather than
// inventing hako-specific names.
func storageEnv(s *store.Store, project store.Project) ([]string, error) {
	if project.LinkedStorage == "" {
		return nil, nil
	}
	st, err := s.GetStorage(project.LinkedStorage)
	if err != nil {
		return nil, fmt.Errorf("linked storage %q not found: %w", project.LinkedStorage, err)
	}

	// kasl-crm's own Settings only recognizes STORAGE_PROVIDER "r2" or "s3"
	// (a pydantic Literal) — "rustfs" is a hako-internal detail about
	// who runs the bucket, not a distinct wire protocol, so it presents to
	// the app as plain "s3" with a custom endpoint, same as any other
	// self-hosted S3-compatible target (MinIO, etc).
	providerEnv := st.Provider
	if providerEnv == "rustfs" {
		providerEnv = "s3"
	}

	env := []string{
		"STORAGE_PROVIDER=" + providerEnv,
		"S3_ACCESS_KEY_ID=" + st.AccessKeyID,
		"S3_SECRET_ACCESS_KEY=" + st.SecretAccessKey,
		"S3_BUCKET_NAME=" + st.Bucket,
		"S3_REGION=" + st.Region,
	}
	if st.Provider == "r2" {
		env = append(env, "R2_ACCOUNT_ID="+st.AccountID)
	} else {
		env = append(env, "S3_ENDPOINT="+st.Endpoint)
	}
	return env, nil
}
