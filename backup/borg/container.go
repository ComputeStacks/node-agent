package borg

import (
	"context"
	"cs-agent/containermgr"
	"cs-agent/sshremote"
	"cs-agent/types"
	"errors"
	"math/rand"
	"reflect"
	"strconv"
	"time"

	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	volumeTypes "github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/spf13/viper"
)

func (r *Repository) InitBackupContainer(vol *types.Volume, source *types.Volume) (bool, error) {
	if !reflect.ValueOf(r.Container).IsNil() {
		// If a container already exists, stop.
		return true, nil
	}
	cli, clientErr := client.NewClientWithOpts(client.WithVersion(viper.GetString("docker.version")))
	if clientErr != nil {
		borgLogger().Error("Unable to connect to Docker", "error", clientErr.Error())
		return false, clientErr
	}

	// Check if the backup volume exists, and create it if it does not.
	if _, volErr := r.ensureBackupVolumeExists(cli, source); volErr != nil {
		return false, volErr
	}

	ctx := context.Background()

	// Ensure image exists
	_, _, missingImage := cli.ImageInspectWithRaw(ctx, viper.GetString("backups.borg.image"))
	if missingImage != nil {
		_, err := cli.ImagePull(ctx, viper.GetString("backups.borg.image"), image.PullOptions{})
		if err != nil {
			borgLogger().Error("Fatal error pulling image", "error", clientErr.Error())
			return false, err
		}
	}

	// Container Labels
	labels := make(map[string]string)
	labels["com.computestacks.role"] = "backup"
	labels["com.computestacks.for"] = vol.Name

	if viper.GetBool("backups.borg.ssh.enabled") {
		labels["com.computestacks.backup-kind"] = "ssh"
	} else if viper.GetBool("backups.borg.nfs") {
		labels["com.computestacks.backup-kind"] = "nfs"
	} else {
		labels["com.computestacks.backup-kind"] = "local"
	}

	// Generate Container Name
	t := time.Now()
	rand.New(rand.NewSource(time.Now().UnixNano())) // Seed for random container name
	randNumber := 10 + rand.Intn(1000-10)
	containerName := "backup-" + strconv.Itoa(randNumber) + string(t.Format("150405"))

	borgEnv := []string{
		"BORG_PASSPHRASE=" + viper.GetString("backups.key"),
		"BORG_RELOCATED_REPO_ACCESS_IS_OK=yes",
		"BORG_DELETE_I_KNOW_WHAT_I_AM_DOING=YES",
		"BORG_CHECK_I_KNOW_WHAT_I_AM_DOING=YES",
		"BORG_BASE_DIR=/mnt/borg",
		"BORG_REPO=" + r.repoPath(),
	}

	hostConfig := container.HostConfig{
		NetworkMode: "none",
		Binds:       []string{},
		Mounts:      []mount.Mount{},
		AutoRemove:  true,
		Privileged:  viper.GetBool("docker.privileged"),
	}

	hostConfig.Mounts = append(hostConfig.Mounts, mount.Mount{
		Type:     mount.TypeBind,
		Source:   "/etc/computestacks",
		ReadOnly: true,
		Target:   "/etc/computestacks",
	})

	hostConfig.NetworkMode = "host"

	if viper.GetBool("backups.borg.ssh.enabled") {
		borgEnv = append(borgEnv, "BORG_REMOTE_PATH="+viper.GetString("backups.borg.ssh_borg_remote_path"))
		borgEnv = append(borgEnv, "BORG_RSH=ssh -i "+viper.GetString("backups.borg.ssh.keyfile"))
	}

	hostConfig.Mounts = append(hostConfig.Mounts, mount.Mount{
		Type:   mount.TypeVolume,
		Source: "b-" + source.Name,
		Target: "/mnt/borg",
	})

	if !vol.Trash {
		hostConfig.Mounts = append(hostConfig.Mounts, mount.Mount{
			Type:   mount.TypeVolume,
			Source: vol.Name,
			Target: "/mnt/data",
		})
	}

	resp, err := cli.ContainerCreate(ctx, &container.Config{
		Image:  viper.GetString("backups.borg.image"),
		Labels: labels,
		Env:    borgEnv,
	}, &hostConfig, nil, nil, containerName)

	if err != nil {
		return false, err
	}

	if err := cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return false, err
	}

	r.Container = &containermgr.Container{ID: resp.ID}

	// time.Sleep(250 * time.Millisecond)
	isReady := false
	for counter := 1; counter < 12; counter++ {
		c, errRunning := cli.ContainerInspect(ctx, resp.ID)
		if errRunning == nil && c.State.Running {
			isReady = true
			break
		} else if errRunning == nil {
			borgLogger().Debug("Waiting for container before executing command", "container", c.ID, "state", c.State.Status)
		} else {
			borgLogger().Debug("Waiting for container before executing command", "error", errRunning.Error())
			// For fatal errors, reduce the total count to 3 instead of 12.
			if counter > 2 {
				break
			}
		}
		time.Sleep(time.Second)
	}
	if !isReady {
		return false, errors.New("container never came online")
	}

	return true, nil
}

/*
 * Helper methods to deal with situations where the container is null.
 */

// StopContainer will stop the backup container.
func (r *Repository) StopContainer() bool {
	// A nil repository (e.g. FindRepository returned nil on a build/init
	// failure and the deferred cleanup still fires) has nothing to stop.
	if r == nil || r.Container == nil {
		return true
	}
	return r.Container.Stop()
}

/*
Volumes
*/
func (r *Repository) ensureBackupVolumeExists(cli *client.Client, vol *types.Volume) (bool, error) {
	ctx := context.Background()

	_, existingVolumeErr := cli.VolumeInspect(ctx, "b-"+vol.Name)

	if existingVolumeErr != nil {
		// Container Labels
		labels := make(map[string]string)
		labels["com.computestacks.role"] = "backup"
		labels["com.computestacks.for"] = vol.Name

		// Driver Opts
		driverOpts := make(map[string]string)

		if viper.GetBool("backups.borg.nfs") {
			if viper.GetBool("backups.borg.nfs_create_path") {
				borgLogger().Info("Creating remote volume directory", "volume", "b-"+vol.Name, "type", "nfs")
				sshCmd, ok := nfsRepoPathCommand(vol.Name)
				if !ok {
					borgLogger().Error("Refusing to create remote directory with an unsafe repository name", "volume", "b-"+vol.Name, "type", "nfs")
					return false, errors.New("refusing to create remote directory: unsafe repository name " + vol.Name)
				}
				connInfo := sshremote.ServerConnInfo{
					Server: viper.GetString("backups.borg.nfs_host"),
					Port:   viper.GetString("backups.borg.nfs_ssh.port"),
					User:   viper.GetString("backups.borg.nfs_ssh.user"),
					Key:    viper.GetString("backups.borg.nfs_ssh.keyfile"),
				}

				createDirSuccess, createDirErr := sshremote.SSHCommandBool(sshCmd, connInfo)

				if createDirErr != nil {
					borgLogger().Error("Fatal error creating directory on remote server", "volume", "b-"+vol.Name, "error", createDirErr.Error())
					return false, createDirErr
				}

				if !createDirSuccess {
					borgLogger().Warn("Invalid response while creating remote directory", "volume", "b-"+vol.Name)
					return false, errors.New("invalid response while creating directory")
				}
			}
			driverOpts["type"] = "nfs"
			driverOpts["o"] = "addr=" + viper.GetString("backups.borg.nfs_host") + ",rw,nfsvers=4" + viper.GetString("backups.borg.nfs_opts")
			driverOpts["device"] = ":" + viper.GetString("backups.borg.nfs_host_path") + "/b-" + vol.Name

		} else if viper.GetBool("backups.borg.ssh.enabled") {

			borgLogger().Info("Creating remote volume directory", "repository", "b-"+r.Name, "type", "ssh")
			sshCmd, ok := sshRepoPathCommand(r.Name)
			if !ok {
				borgLogger().Error("Refusing to create remote directory with an unsafe repository name", "repository", r.Name, "type", "ssh")
				return false, errors.New("refusing to create remote directory: unsafe repository name " + r.Name)
			}
			connInfo := sshremote.ServerConnInfo{
				Server: viper.GetString("backups.borg.ssh.host"),
				Port:   viper.GetString("backups.borg.ssh.port"),
				User:   viper.GetString("backups.borg.ssh.user"),
				Key:    viper.GetString("backups.borg.ssh.keyfile"),
			}

			createDirSuccess, createDirErr := sshremote.SSHCommandBool(sshCmd, connInfo)

			if createDirErr != nil {
				borgLogger().Error("Fatal error creating directory on remote server", "repository", r.Name, "error", createDirErr.Error(), "type", "ssh")
				return false, createDirErr
			}

			if !createDirSuccess {
				borgLogger().Warn("Invalid response while creating remote directory", "repository", r.Name, "type", "ssh")
				return false, errors.New("invalid response while creating directory")
			}
		}

		opts := volumeTypes.CreateOptions{
			Name:       "b-" + vol.Name,
			Driver:     "local",
			DriverOpts: driverOpts,
			Labels:     labels,
		}

		_, volErr := cli.VolumeCreate(ctx, opts)

		if volErr != nil {
			borgLogger().Warn("Fatal Error Creating Volume", "volume", "b-"+vol.Name, "error", volErr.Error())
			return false, volErr
		}

		return true, nil

	}
	return true, nil
}

// nfsRepoPathCommand builds the remote shell command that creates the repository
// directory on the NFS server and chowns it to the NFS-squash user (the container
// writes as that user, so borg could not otherwise read it back). The name is
// interpolated into the command, so an unsafe name returns ok=false rather than
// building it.
func nfsRepoPathCommand(name string) (string, bool) {
	if !safeRepoName(name) {
		return "", false
	}
	basePath := viper.GetString("backups.borg.nfs_host_path") + "/b-" + name
	cmd := "mkdir -p " + basePath +
		" && chown -R " + viper.GetString("backups.borg.nfs_ssh.fs_user") + ":" +
		viper.GetString("backups.borg.nfs_ssh.fs_group") + " " + basePath
	return cmd, true
}

// sshRepoPathCommand builds the remote shell command that creates the repository
// directory on the SSH backup server. The name is interpolated into the command,
// so an unsafe name returns ok=false rather than building it.
func sshRepoPathCommand(name string) (string, bool) {
	if !safeRepoName(name) {
		return "", false
	}
	cmd := "mkdir -p " + viper.GetString("backups.borg.ssh.host_path") + "/b-" + name + "/backup"
	return cmd, true
}

func (r *Repository) TrashBackupVolumeExists(vol *types.Volume) (bool, error) {
	ctx := context.Background()
	cli, clientErr := client.NewClientWithOpts(client.WithVersion(viper.GetString("docker.version")))
	if clientErr != nil {
		borgLogger().Error("Unable to connect to Docker", "error", clientErr.Error())
		return false, clientErr
	}
	existingVolume, existingVolumeErr := cli.VolumeInspect(ctx, "b-"+vol.Name)

	if existingVolumeErr != nil {
		borgLogger().Info("Volume does not exist, skipping...")
		return true, nil
	}
	if removeErr := cli.VolumeRemove(ctx, existingVolume.Name, false); removeErr != nil {
		borgLogger().Error("Error Deleting Volume", "error", removeErr.Error())
		return false, removeErr
	}

	if viper.GetBool("backups.borg.nfs") {
		borgLogger().Info("Cleaning remote volume path", "volume", "b-"+vol.Name)

		sshCmd, ok := nfsRepoRemoveCommand(vol.Name)
		if !ok {
			borgLogger().Error("Refusing to remove remote directory with an unsafe repository name", "volume", "b-"+vol.Name)
			return false, errors.New("refusing to remove remote directory: unsafe repository name " + vol.Name)
		}
		connInfo := sshremote.ServerConnInfo{
			Server: viper.GetString("backups.borg.nfs_host"),
			Port:   viper.GetString("backups.borg.nfs_ssh.port"),
			User:   viper.GetString("backups.borg.nfs_ssh.user"),
			Key:    viper.GetString("backups.borg.nfs_ssh.keyfile"),
		}

		destroyDirSuccess, destroyDirErr := sshremote.SSHCommandBool(sshCmd, connInfo)

		if destroyDirErr != nil {
			borgLogger().Error("Error removing remote directory", "volume", "b-"+vol.Name, "error", destroyDirErr.Error())
			return false, destroyDirErr
		}

		if !destroyDirSuccess {
			borgLogger().Warn("Invalid response while destroying remote directory", "volume", "b-"+vol.Name)
			return false, errors.New("invalid response while destroying directory")
		}
	} else if viper.GetBool("backups.borg.ssh.enabled") {

		borgLogger().Info("Cleaning remote volume path", "repository", r.Name, "method", "ssh")

		sshCmd, ok := sshRepoRemoveCommand(vol.Name)
		if !ok {
			borgLogger().Error("Refusing to remove remote directory with an unsafe repository name", "repository", r.Name, "type", "ssh")
			return false, errors.New("refusing to remove remote directory: unsafe repository name " + vol.Name)
		}
		connInfo := sshremote.ServerConnInfo{
			Server: viper.GetString("backups.borg.ssh.host"),
			Port:   viper.GetString("backups.borg.ssh.port"),
			User:   viper.GetString("backups.borg.ssh.user"),
			Key:    viper.GetString("backups.borg.ssh.keyfile"),
		}

		destroyDirSuccess, destroyDirErr := sshremote.SSHCommandBool(sshCmd, connInfo)

		if destroyDirErr != nil {
			borgLogger().Error("Error removing remote directory", "repository", r.Name, "error", destroyDirErr.Error())
			return false, destroyDirErr
		}

		if !destroyDirSuccess {
			borgLogger().Warn("Invalid response while destroying remote directory", "repository", r.Name, "type", "ssh")
			return false, errors.New("invalid response while destroying directory")
		}
	} else {
		borgLogger().Info("NFS disabled, skipping remote file cleanup.")
	}

	borgLogger().Info("Successfully removed backup volume", "volume", "b-"+vol.Name)
	return true, nil
}

// nfsRepoRemoveCommand builds the remote shell command that removes the repository
// directory from the NFS server. The name is interpolated into the command, so an
// unsafe name returns ok=false rather than building it.
func nfsRepoRemoveCommand(name string) (string, bool) {
	if !safeRepoName(name) {
		return "", false
	}
	cmd := "rm -rf " + viper.GetString("backups.borg.nfs_host_path") + "/b-" + name
	return cmd, true
}

// sshRepoRemoveCommand builds the remote shell command that removes the repository
// directory from the SSH backup server. The name is interpolated into the command,
// so an unsafe name returns ok=false rather than building it.
func sshRepoRemoveCommand(name string) (string, bool) {
	if !safeRepoName(name) {
		return "", false
	}
	cmd := "rm -rf " + viper.GetString("backups.borg.ssh.host_path") + "/b-" + name
	return cmd, true
}
